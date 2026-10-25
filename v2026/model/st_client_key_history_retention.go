// Signed key history of a client that is gone is deleted 91 days after it
// went (PARTITION-MAINTENANCE-PLAN.md Part D). Original-close verification
// reads the history of a report's own client (contract_close_original.go)
// while that client's contract is open, and a contract opens only between
// active clients, so the horizon outlasts every close report that can still
// read it. st_client_key_gone records when each client went
// (db_identity_retention.go); the history guard admits a delete only from the
// retention transaction and only for a client gone more than 91 days, with no
// client row and no head left. Functions are safe concurrently.
package model

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

type StClientKeyHistoryRetentionSettings struct {
	// history goes this long after its client went. The migration's guard
	// refuses anything newer than 91 days, so a shorter setting deletes no
	// earlier.
	Retention time.Duration
	// gone clients whose heads and history one transaction deletes
	DeleteBatchCount int
	// heads one discovery transaction reads
	DiscoverBatchCount int
	// a discovery pass starts this long after the last one completed
	DiscoverInterval time.Duration
	// one run's work: deletion first, discovery with what remains
	RunBudget time.Duration
}

func DefaultStClientKeyHistoryRetentionSettings() *StClientKeyHistoryRetentionSettings {
	return &StClientKeyHistoryRetentionSettings{
		Retention:          server.ContractRetention,
		DeleteBatchCount:   500,
		DiscoverBatchCount: 10000,
		DiscoverInterval:   7 * 24 * time.Hour,
		RunBudget:          4 * time.Minute,
	}
}

// Where the next run resumes.
type StClientKeyHistoryRetentionCursor struct {
	// the last head client a discovery pass read; nil starts a pass
	DiscoverAfterClientId *server.Id `json:"discover_after_client_id,omitempty"`
	// when the last complete discovery pass ended
	DiscoverCompleteTime *time.Time `json:"discover_complete_time,omitempty"`
}

type StClientKeyHistoryRetentionReport struct {
	// gone clients whose history was deleted
	RemovedClientCount  int64
	RemovedHistoryCount int64
	// clients the discovery found gone without a record of when
	DiscoveredClientCount int64
	// work remains for an immediate rerun
	More bool
}

// One batch of clients gone longer than the retention, oldest first, from
// st_client_key_gone_gone_time. The bound repeats the guard's 91 days on the
// database clock, so a batch never holds a row the guard refuses.
const stClientKeyHistoryRetentionBatchSql = `
	SELECT st_client_key_gone.client_id
	FROM st_client_key_gone
	WHERE
		st_client_key_gone.gone_time < $1 AND
		st_client_key_gone.gone_time < (now() AT TIME ZONE 'UTC') - interval '91 days' AND
		NOT EXISTS (
			SELECT 1 FROM network_client
			WHERE network_client.client_id = st_client_key_gone.client_id
		)
	ORDER BY st_client_key_gone.gone_time
	LIMIT $2
	FOR UPDATE OF st_client_key_gone SKIP LOCKED
`

// A discovery page of heads in client order, recording when a client without
// a row was first seen gone. A client deleted before the delete trigger
// recorded it, or by a delete that bypassed the trigger, is gone at least
// since then. The page bound advances even when nothing was gone.
const stClientKeyHistoryDiscoverPageSqlTemplate = `
	WITH page AS MATERIALIZED (
		SELECT client_id
		FROM st_client_key_head
		{{cursor}}
		ORDER BY client_id
		LIMIT $1
	), gone AS (
		INSERT INTO st_client_key_gone (client_id, gone_time)
		SELECT DISTINCT page.client_id, now() AT TIME ZONE 'UTC'
		FROM page
		WHERE NOT EXISTS (
			SELECT 1 FROM network_client
			WHERE network_client.client_id = page.client_id
		)
		ON CONFLICT (client_id) DO NOTHING
		RETURNING 1
	), bound AS (
		SELECT client_id FROM page ORDER BY client_id DESC LIMIT 1
	)
	SELECT (SELECT count(*) FROM page), (SELECT count(*) FROM gone), bound.client_id
	FROM bound
`

var stClientKeyHistoryDiscoverFirstPageSql = strings.NewReplacer(
	"{{cursor}}", "",
).Replace(stClientKeyHistoryDiscoverPageSqlTemplate)

var stClientKeyHistoryDiscoverResumedPageSql = strings.NewReplacer(
	"{{cursor}}", "WHERE client_id > $2",
).Replace(stClientKeyHistoryDiscoverPageSqlTemplate)

// Deletes the key history of clients gone longer than the retention, then
// continues the discovery pass of clients gone without a record, each in
// short transactions until the run budget is spent.
func RemoveGoneStClientKeyHistory(
	ctx context.Context,
	now time.Time,
	cursor StClientKeyHistoryRetentionCursor,
	settings *StClientKeyHistoryRetentionSettings,
) (report *StClientKeyHistoryRetentionReport, next StClientKeyHistoryRetentionCursor) {
	report = &StClientKeyHistoryRetentionReport{}
	next = cursor
	start := time.Now()
	deleteDeadline := start.Add(settings.RunBudget / 2)
	discoverDeadline := start.Add(settings.RunBudget)

	minGoneTime := now.Add(-settings.Retention).UTC()
	for {
		select {
		case <-ctx.Done():
			report.More = true
			return
		default:
		}
		removedClientIds, removedHistoryCount := removeGoneStClientKeyHistoryBatch(ctx, minGoneTime, settings.DeleteBatchCount)
		report.RemovedClientCount += int64(len(removedClientIds))
		report.RemovedHistoryCount += removedHistoryCount
		removeGoneClientPublicKeys(ctx, removedClientIds)
		if len(removedClientIds) < settings.DeleteBatchCount {
			break
		}
		if !time.Now().Before(deleteDeadline) {
			report.More = true
			break
		}
	}

	if next.DiscoverAfterClientId == nil && next.DiscoverCompleteTime != nil && now.Before(next.DiscoverCompleteTime.Add(settings.DiscoverInterval)) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			report.More = true
			return
		default:
		}
		if !time.Now().Before(discoverDeadline) {
			report.More = true
			return
		}
		scannedCount, discoveredCount, lastClientId := discoverGoneStClientKeyHistoryPage(ctx, next.DiscoverAfterClientId, settings.DiscoverBatchCount)
		report.DiscoveredClientCount += discoveredCount
		if scannedCount < int64(settings.DiscoverBatchCount) {
			completeTime := now
			next.DiscoverAfterClientId = nil
			next.DiscoverCompleteTime = &completeTime
			return
		}
		next.DiscoverAfterClientId = lastClientId
	}
}

// One short transaction: heads first, since a retained head marks a complete
// archived history (db_client_key_policy_history.go), then the history, then
// the records. Returns the clients whose history went.
func removeGoneStClientKeyHistoryBatch(ctx context.Context, minGoneTime time.Time, batchCount int) (removedClientIds []server.Id, removedHistoryCount int64) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		// an attempt that does not commit removed nothing
		removedClientIds = nil
		removedHistoryCount = 0

		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.client_key_history_retention = 'on'`))
		clientIds := []server.Id{}
		result, err := tx.Query(ctx, stClientKeyHistoryRetentionBatchSql, minGoneTime, batchCount)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
		if len(clientIds) == 0 {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM st_client_key_head WHERE client_id = ANY($1)`,
			clientIds,
		))
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM st_client_key_history WHERE client_id = ANY($1)`,
			clientIds,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM st_client_key_gone WHERE client_id = ANY($1)`,
			clientIds,
		))
		removedClientIds = clientIds
		removedHistoryCount = tag.RowsAffected()
	}, server.TxReadCommitted)
	return
}

// One discovery transaction. lastClientId is the page's last head client, nil
// when the page was empty.
func discoverGoneStClientKeyHistoryPage(ctx context.Context, afterClientId *server.Id, batchCount int) (scannedCount int64, discoveredCount int64, lastClientId *server.Id) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		scannedCount, discoveredCount, lastClientId = 0, 0, nil
		var row pgx.Row
		if afterClientId == nil {
			row = tx.QueryRow(ctx, stClientKeyHistoryDiscoverFirstPageSql, batchCount)
		} else {
			row = tx.QueryRow(ctx, stClientKeyHistoryDiscoverResumedPageSql, batchCount, *afterClientId)
		}
		var bound server.Id
		err := row.Scan(&scannedCount, &discoveredCount, &bound)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		lastClientId = &bound
	}, server.TxReadCommitted)
	return
}

// A retired head answers for a client's key, so once the heads are gone the
// legacy Redis key must be too, or a key the client reap failed to delete
// would answer again (GetClientPublicKey). Best effort: the reap already
// deleted these, and a failure is logged.
func removeGoneClientPublicKeys(ctx context.Context, clientIds []server.Id) {
	if len(clientIds) == 0 {
		return
	}
	server.HandleError(func() {
		forEachReapedClientRedisChunk(clientIds, func(chunk []server.Id) {
			server.Redis(ctx, func(r server.RedisClient) {
				_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
					for _, clientId := range chunk {
						pipe.Del(ctx, clientPublicKeyRedisKey(clientId))
					}
					return nil
				})
				server.Raise(err)
			})
		})
	})
}
