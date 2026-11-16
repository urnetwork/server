// Signed key history of a client that is gone is deleted 7 days after it went
// (PARTITION-MAINTENANCE-PLAN.md Part D; owner 2026-11-01, and 2026-11-16 for
// 7 days, PARTITION-CANDIDATES.md §8.4), never sooner than the guard's floor,
// server.ClientKeyHistoryRetentionFloor. It no longer follows the contract
// horizon reaper: no reader of a gone client's keys depends on how long
// contracts are kept. A runtime brake of its own
// (st_client_key_history_retention_control.go) stops or paces the deletes, and
// its `horizonDays` can narrow the horizon, never below the floor.
// Original-close verification reads the history of a report's own client
// (contract_close_original.go) for an unresolved, undisputed contract the
// client is party to. A report needs the client's credential, which its
// deactivation revokes in the same transaction and live transports recheck,
// so a gone client's last report lands within minutes of its going; one past
// the horizon is still admitted, recording history_not_found, and an admitted
// report keeps its own copy of the registration. db_identity_retention.go
// lists the other readers, each of which refuses or ignores a gone client.
// st_client_key_gone records when each client went; the history guard admits a
// delete only from the retention transaction and only for a client gone longer
// than the floor, with no client row and no head left.
// Functions are safe concurrently.
package model

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

type StClientKeyHistoryRetentionSettings struct {
	// History goes this long after its client went, and the brake's
	// horizonDays can only narrow it. 7 days (owner, 2026-11-16), the guard's
	// floor: close report admission, the one reader of a gone client's keys,
	// needs the client's credential, which its deactivation revokes
	// (db_identity_retention.go). The guard refuses anything newer than its
	// floor, so no setting deletes earlier than
	// server.ClientKeyHistoryRetentionFloor; a value below it counts as the
	// floor.
	Retention time.Duration
	// Gone clients deleted per second while the brake sets no rate: a cautious
	// start, so a backlog that comes due at once, such as the 2026-10-24..27
	// records, drains under watch. The brake raises it.
	DefaultDeleteClientsPerSecond float64
	// The highest rate the brake can set.
	MaxDeleteClientsPerSecond float64
	// Bounds each brake and horizon control read.
	ControlTimeout time.Duration
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
		Retention: 7 * 24 * time.Hour,
		// a run deletes for half its budget and the next starts a minute after
		// a run that left work, so 150 a second is about 18k clients a run and
		// 8.6M a day; the brake raises it to drain a backlog, 1000 a second
		// about 57M a day. A gone client has one registration on average, and
		// a batch of 500 took 0.17 s on Main (2026-11-16), so the rate, not
		// the batch, bounds the pace
		DefaultDeleteClientsPerSecond: 150,
		MaxDeleteClientsPerSecond:     2000,
		ControlTimeout:                2 * time.Second,
		DeleteBatchCount:              500,
		DiscoverBatchCount:            10000,
		DiscoverInterval:              7 * 24 * time.Hour,
		RunBudget:                     4 * time.Minute,
	}
}

// The horizon of one run without a brake horizon: Retention, never below the
// guard's floor.
func (self *StClientKeyHistoryRetentionSettings) horizon() time.Duration {
	return max(self.Retention, server.ClientKeyHistoryRetentionFloor)
}

// The horizon of one run: the brake's own horizon when it sets one, otherwise
// horizon's. brake is the run's first read of the brake, nil when it could not
// be read; a run that cannot read the brake deletes nothing either way.
func (self *StClientKeyHistoryRetentionSettings) runHorizon(brake *StClientKeyHistoryRetentionControl) time.Duration {
	if brake != nil && brake.Horizon != nil {
		return max(*brake.Horizon, server.ClientKeyHistoryRetentionFloor)
	}
	return self.horizon()
}

// Where the next run resumes.
type StClientKeyHistoryRetentionCursor struct {
	// the last gone record a delete pass removed, in (gone_time, client_id)
	// order; nil starts a pass at the oldest record. A pass ends when a batch
	// comes back short, so the next one rereads what an earlier batch passed
	// over (a record another transaction held).
	DeleteAfterGoneTime *time.Time `json:"delete_after_gone_time,omitempty"`
	DeleteAfterClientId *server.Id `json:"delete_after_client_id,omitempty"`
	// the last head client a discovery pass read; nil starts a pass
	DiscoverAfterClientId *server.Id `json:"discover_after_client_id,omitempty"`
	// when the last complete discovery pass ended
	DiscoverCompleteTime *time.Time `json:"discover_complete_time,omitempty"`
}

// Why a run's deletes ended.
const (
	// a batch came back short: nothing more is due
	StClientKeyHistoryRetentionStopDue = "due"
	// the delete half of the run budget is spent
	StClientKeyHistoryRetentionStopBudget = "budget"
	// the brake is off, or sets no rate
	StClientKeyHistoryRetentionStopBrakeOff = "brake_off"
	// the brake could not be read
	StClientKeyHistoryRetentionStopBrakeUnread = "brake_unread"
	StClientKeyHistoryRetentionStopCanceled    = "canceled"
)

type StClientKeyHistoryRetentionReport struct {
	// how long after its client went history was deleted in this run
	Horizon time.Duration
	// why the deletes ended, one of StClientKeyHistoryRetentionStop*
	DeleteStop string
	// gone clients whose history was deleted
	RemovedClientCount  int64
	RemovedHistoryCount int64
	// clients the discovery found gone without a record of when
	DiscoveredClientCount int64
	// work remains for an immediate rerun
	More bool
}

// One batch of clients gone longer than the retention, oldest first, from
// st_client_key_gone_gone_time, after the pass's cursor: the index entries of
// the records earlier batches deleted stay until vacuum, and a batch that
// started over at the oldest record would read them all again. The bound
// repeats the guard's floor on the database clock, so a batch never holds a
// row the guard refuses.
const stClientKeyHistoryRetentionBatchSqlTemplate = `
	SELECT st_client_key_gone.client_id, st_client_key_gone.gone_time
	FROM st_client_key_gone
	WHERE
		st_client_key_gone.gone_time < $1 AND
		st_client_key_gone.gone_time < (now() AT TIME ZONE 'UTC') - st_client_key_history_retention_floor() AND
		{{cursor}}
		NOT EXISTS (
			SELECT 1 FROM network_client
			WHERE network_client.client_id = st_client_key_gone.client_id
		)
	ORDER BY st_client_key_gone.gone_time, st_client_key_gone.client_id
	LIMIT $2
	FOR UPDATE OF st_client_key_gone SKIP LOCKED
`

var stClientKeyHistoryRetentionFirstBatchSql = strings.NewReplacer(
	"{{cursor}}", "",
).Replace(stClientKeyHistoryRetentionBatchSqlTemplate)

// gone_time leads the row comparison as its own bound, so the index seeks to
// the cursor
var stClientKeyHistoryRetentionResumedBatchSql = strings.NewReplacer(
	"{{cursor}}", `st_client_key_gone.gone_time >= $3 AND
		(st_client_key_gone.gone_time, st_client_key_gone.client_id) > ($3, $4) AND`,
).Replace(stClientKeyHistoryRetentionBatchSqlTemplate)

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

	// the brake's own horizon is read once a run; the deletes reread the brake
	// before every batch
	runBrake, runBrakeErr := GetStClientKeyHistoryRetentionControl(ctx, settings)
	if runBrakeErr != nil {
		runBrake = nil
	}
	report.Horizon = settings.runHorizon(runBrake)
	minGoneTime := now.Add(-report.Horizon).UTC()
	pace := newRetentionPace()
	for {
		select {
		case <-ctx.Done():
			report.More = true
			report.DeleteStop = StClientKeyHistoryRetentionStopCanceled
			return
		default:
		}
		// the brake, before every batch
		brake, err := GetStClientKeyHistoryRetentionControl(ctx, settings)
		if err != nil {
			glog.V(1).Infof("[ckh]retention control unread, deleting nothing more this run: %s\n", err)
			report.DeleteStop = StClientKeyHistoryRetentionStopBrakeUnread
			break
		}
		if !brake.On {
			report.DeleteStop = StClientKeyHistoryRetentionStopBrakeOff
			break
		}
		batch := removeGoneStClientKeyHistoryBatch(ctx, minGoneTime, next.DeleteAfterGoneTime, next.DeleteAfterClientId, settings.DeleteBatchCount)
		report.RemovedClientCount += int64(len(batch.clientIds))
		report.RemovedHistoryCount += batch.historyCount
		removeGoneClientPublicKeys(ctx, batch.clientIds)
		if len(batch.clientIds) < settings.DeleteBatchCount {
			// the pass is complete; the next starts at the oldest record
			next.DeleteAfterGoneTime, next.DeleteAfterClientId = nil, nil
			report.DeleteStop = StClientKeyHistoryRetentionStopDue
			break
		}
		next.DeleteAfterGoneTime, next.DeleteAfterClientId = &batch.lastGoneTime, &batch.lastClientId
		if !pace.WaitAfter(ctx, len(batch.clientIds), brake.MaxClientsPerSecond, deleteDeadline) || !time.Now().Before(deleteDeadline) {
			report.More = true
			report.DeleteStop = StClientKeyHistoryRetentionStopBudget
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

// What one committed batch removed, and the last record it read: the cursor
// of the batch after it.
type goneStClientKeyHistoryBatch struct {
	clientIds    []server.Id
	historyCount int64
	lastGoneTime time.Time
	lastClientId server.Id
}

// One short transaction: heads first, since a retained head marks a complete
// archived history (db_client_key_policy_history.go), then the history, then
// the records. The records read come after (afterGoneTime, afterClientId),
// the start of the pass when nil.
func removeGoneStClientKeyHistoryBatch(ctx context.Context, minGoneTime time.Time, afterGoneTime *time.Time, afterClientId *server.Id, batchCount int) (batch goneStClientKeyHistoryBatch) {
	var removedHeadCount, removedGoneCount int64
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		// an attempt that does not commit removed nothing
		batch = goneStClientKeyHistoryBatch{}
		removedHeadCount = 0
		removedGoneCount = 0

		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.client_key_history_retention = 'on'`))
		clientIds := []server.Id{}
		var result server.PgResult
		var err error
		if afterGoneTime == nil || afterClientId == nil {
			result, err = tx.Query(ctx, stClientKeyHistoryRetentionFirstBatchSql, minGoneTime, batchCount)
		} else {
			result, err = tx.Query(ctx, stClientKeyHistoryRetentionResumedBatchSql, minGoneTime, batchCount, afterGoneTime.UTC(), *afterClientId)
		}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var goneTime time.Time
				server.Raise(result.Scan(&clientId, &goneTime))
				clientIds = append(clientIds, clientId)
				batch.lastGoneTime, batch.lastClientId = goneTime, clientId
			}
		})
		if len(clientIds) == 0 {
			return
		}
		headTag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM st_client_key_head WHERE client_id = ANY($1)`,
			clientIds,
		))
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM st_client_key_history WHERE client_id = ANY($1)`,
			clientIds,
		))
		goneTag := server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM st_client_key_gone WHERE client_id = ANY($1)`,
			clientIds,
		))
		batch.clientIds = clientIds
		batch.historyCount = tag.RowsAffected()
		removedHeadCount = headTag.RowsAffected()
		removedGoneCount = goneTag.RowsAffected()
	}, server.TxReadCommitted)
	// counted once the batch committed
	countRetentionDeletedRows(RetentionTableStClientKeyHead, removedHeadCount)
	countRetentionDeletedRows(RetentionTableStClientKeyHistory, batch.historyCount)
	countRetentionDeletedRows(RetentionTableStClientKeyGone, removedGoneCount)
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
