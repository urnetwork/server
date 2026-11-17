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
// A delete batch reads one page of the records in (gone_time, client_id) order
// and checks only that page's clients for a row, so what a batch reads is
// bounded by its page, never by how many records are due or how many of their
// clients still exist. Each batch and discovery transaction is also bounded on
// the server by its statement, lock and transaction timeouts, so none holds a
// snapshot for more than seconds. Before 2026-11-16 one ordered query selected
// the due records with the client check as its filter; once the 7-day horizon
// made nearly every record due, the planner, which expects every record's
// client to still have a row, read it as an anti-join of all of
// st_client_key_gone with all of network_client, and a single batch ran 30
// minutes on Main holding its snapshot.
// Functions are safe concurrently.
package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
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
	// Gone records the deletes examine per second while the brake sets no rate,
	// and so the most clients they delete per second: a cautious start, so a
	// backlog that comes due at once, such as the 2026-10-24..27 records,
	// drains under watch. The brake raises it.
	DefaultDeleteClientsPerSecond float64
	// The highest rate the brake can set.
	MaxDeleteClientsPerSecond float64
	// Bounds each brake and horizon control read.
	ControlTimeout time.Duration
	// Gone records one delete batch reads, in (gone_time, client_id) order
	// after the pass's cursor: the most it deletes, and the bound on what it
	// examines, whatever share of the records is due and however many of
	// their clients still have a row.
	DeleteBatchCount int
	// heads one discovery transaction reads
	DiscoverBatchCount int
	// a discovery pass starts this long after the last one completed
	DiscoverInterval time.Duration
	// one run's work: deletion first, discovery with what remains
	RunBudget time.Duration
	// Bounds one delete batch or discovery page from its begin to its commit:
	// on the server as the transaction's transaction_timeout, so no batch
	// holds a snapshot or a transaction id longer, and here as the deadline for
	// its connection and its statements. Main sets no transaction_timeout for
	// the maintenance role, so nothing else bounds them but the task's
	// MaxTime. A batch past it rolls back, and the run deletes nothing more.
	// Non-positive leaves the transaction unbounded.
	BatchTimeout time.Duration
	// Bounds each statement of a delete batch or discovery page, as its
	// statement_timeout. Non-positive leaves statements unbounded.
	BatchStatementTimeout time.Duration
	// Bounds each lock wait of a delete batch or discovery page, as its
	// lock_timeout. Non-positive leaves lock waits unbounded.
	BatchLockTimeout time.Duration
}

func DefaultStClientKeyHistoryRetentionSettings() *StClientKeyHistoryRetentionSettings {
	return &StClientKeyHistoryRetentionSettings{
		Retention: 7 * 24 * time.Hour,
		// a run deletes for half its budget and the next starts a minute after
		// a run that left work, so 150 a second is about 18k clients a run and
		// 8.6M a day; the brake raises it to drain a backlog, 1000 a second
		// about 57M a day. A gone client has one registration on average, and
		// a page of 500 was read and checked in 50-200 ms on Main (2026-11-16,
		// read only), so the rate, not the batch, bounds the pace
		DefaultDeleteClientsPerSecond: 150,
		MaxDeleteClientsPerSecond:     2000,
		ControlTimeout:                2 * time.Second,
		DeleteBatchCount:              500,
		DiscoverBatchCount:            10000,
		DiscoverInterval:              7 * 24 * time.Hour,
		RunBudget:                     4 * time.Minute,
		// tens of times a page's measured time, and seconds of snapshot at most
		BatchTimeout:          10 * time.Second,
		BatchStatementTimeout: 5 * time.Second,
		// the other retention walks' lock bound
		BatchLockTimeout: 2 * time.Second,
	}
}

// The join methods a batch or discovery page may not use. Its client checks
// probe network_client, and its locks st_client_key_gone, by key once per
// record of its page, so no misestimate can plan them as a join of whole
// tables, as the 2026-11-16 batch was planned: a merge anti-join of all of
// st_client_key_gone with all of network_client. Neither table is joined any
// other way in these transactions.
const stClientKeyHistoryRetentionPlanSql = `SET LOCAL enable_hashjoin = off; SET LOCAL enable_mergejoin = off`

// Runs callback in one maintenance transaction bounded by the batch
// settings: its statements, its lock waits and the transaction itself end at
// their bounds on the server, and the transaction's deadline also bounds
// acquiring its connection. Its joins probe by key
// (stClientKeyHistoryRetentionPlanSql). callback gets the transaction's
// context.
func (self *StClientKeyHistoryRetentionSettings) boundedTx(ctx context.Context, callback func(context.Context, server.PgTx)) {
	if 0 < self.BatchTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, self.BatchTimeout)
		defer cancel()
	}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.SetTransactionTimeoutInTx(ctx, tx, self.BatchTimeout)
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, max(0, self.BatchStatementTimeout.Milliseconds()))))
		server.RaisePgResult(tx.Exec(ctx, stClientKeyHistoryRetentionPlanSql))
		callback(ctx, tx)
	}, server.TxReadCommitted, server.TxLockTimeout(self.BatchLockTimeout))
}

// The SQLSTATE that ends a session whose transaction ran past
// transaction_timeout (PostgreSQL 17+), which pgerrcode does not name.
const pgTransactionTimeoutCode = "25P04"

// Whether r is a bound of one batch ending it while the run's ctx is live: a
// lock wait past BatchLockTimeout (55P03), a statement canceled (57014) by
// BatchStatementTimeout or an operator, the transaction past BatchTimeout
// (25P04, or the batch's own deadline), or a transaction left idle past the
// server's bound (25P03). The end of the run's ctx, and every other raise,
// is not.
func stClientKeyHistoryRetentionBounded(ctx context.Context, r any) bool {
	err, ok := r.(error)
	if !ok || ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case pgerrcode.LockNotAvailable, pgerrcode.QueryCanceled, pgerrcode.IdleInTransactionSessionTimeout, pgTransactionTimeoutCode:
		return true
	default:
		return false
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
	// the last gone record a delete pass examined, in (gone_time, client_id)
	// order, deleted or not; nil starts a pass at the oldest record. A pass
	// ends when a batch reads a short page, so the next one rereads what an
	// earlier batch passed over (a record another transaction held, or one
	// whose client still had a row).
	DeleteAfterGoneTime *time.Time `json:"delete_after_gone_time,omitempty"`
	DeleteAfterClientId *server.Id `json:"delete_after_client_id,omitempty"`
	// the last head client a discovery pass read; nil starts a pass
	DiscoverAfterClientId *server.Id `json:"discover_after_client_id,omitempty"`
	// when the last complete discovery pass ended
	DiscoverCompleteTime *time.Time `json:"discover_complete_time,omitempty"`
}

// Why a run's deletes ended.
const (
	// a batch read a short page: nothing more is due
	StClientKeyHistoryRetentionStopDue = "due"
	// the delete half of the run budget is spent
	StClientKeyHistoryRetentionStopBudget = "budget"
	// the brake is off, or sets no rate
	StClientKeyHistoryRetentionStopBrakeOff = "brake_off"
	// the brake could not be read
	StClientKeyHistoryRetentionStopBrakeUnread = "brake_unread"
	StClientKeyHistoryRetentionStopCanceled    = "canceled"
	// a batch ran into one of its bounds (BatchTimeout, BatchStatementTimeout,
	// BatchLockTimeout) and rolled back; the next run retries its page, and
	// this run discovers nothing either
	StClientKeyHistoryRetentionStopBatchBound = "batch_bound"
)

type StClientKeyHistoryRetentionReport struct {
	// how long after its client went history was deleted in this run
	Horizon time.Duration
	// why the deletes ended, one of StClientKeyHistoryRetentionStop*
	DeleteStop string
	// gone records the delete batches read, deleted or not
	ExaminedRecordCount int64
	// gone clients whose history was deleted
	RemovedClientCount  int64
	RemovedHistoryCount int64
	// clients the discovery found gone without a record of when
	DiscoveredClientCount int64
	// a discovery page ran into one of its bounds and rolled back; the pass
	// resumes from its cursor at the next run
	DiscoverBounded bool
	// work remains for an immediate rerun
	More bool
}

// One delete batch: a page of the records gone longer than the retention,
// oldest first, after the pass's cursor, then the page's records whose client
// has no row, which the batch locks to delete. The page is a range of
// st_client_key_gone_gone_time cut at the page size before any client is
// checked, so the batch reads at most the page (and the rest of the page's
// last gone_time, which the client_id order sorts) and probes network_client
// by key once per record of it. Its size is part of the statement, so that a
// generic plan knows the page is small. The cursor keeps a pass from reading
// again the index entries of the records earlier batches deleted, which stay
// until vacuum. Both bounds repeat the guard's floor on the database clock,
// so a batch never holds a row the guard refuses. One row: the page's record
// count, the due clients and the page's last record; none for an empty page.
const stClientKeyHistoryRetentionBatchSqlTemplate = `
	WITH page AS MATERIALIZED (
		SELECT client_id, gone_time
		FROM st_client_key_gone
		WHERE
			{{cursor}}
			gone_time < $1 AND
			gone_time < (now() AT TIME ZONE 'UTC') - st_client_key_history_retention_floor()
		ORDER BY gone_time, client_id
		LIMIT {{limit}}
	), due AS MATERIALIZED (
		SELECT st_client_key_gone.client_id
		FROM page
		JOIN st_client_key_gone ON st_client_key_gone.client_id = page.client_id
		WHERE
			st_client_key_gone.gone_time < $1 AND
			st_client_key_gone.gone_time < (now() AT TIME ZONE 'UTC') - st_client_key_history_retention_floor() AND
			NOT EXISTS (
				SELECT 1 FROM network_client
				WHERE network_client.client_id = st_client_key_gone.client_id
			)
		FOR UPDATE OF st_client_key_gone SKIP LOCKED
	), bound AS (
		SELECT gone_time, client_id FROM page
		ORDER BY gone_time DESC, client_id DESC
		LIMIT 1
	)
	SELECT
		(SELECT count(*) FROM page),
		ARRAY(SELECT client_id FROM due),
		bound.gone_time,
		bound.client_id
	FROM bound
`

// The first page of a pass, or with resumed a page after the cursor ($2, $3),
// of batchCount records. gone_time leads the row comparison as its own bound,
// so the index seeks to the cursor.
func stClientKeyHistoryRetentionBatchSql(batchCount int, resumed bool) string {
	cursor := ""
	if resumed {
		cursor = `gone_time >= $2 AND (gone_time, client_id) > ($2, $3) AND`
	}
	return strings.NewReplacer(
		"{{cursor}}", cursor,
		"{{limit}}", strconv.Itoa(batchCount),
	).Replace(stClientKeyHistoryRetentionBatchSqlTemplate)
}

// A discovery page of heads in client order, recording when a client without
// a row was first seen gone. A client deleted before the delete trigger
// recorded it, or by a delete that bypassed the trigger, is gone at least
// since then. The page bound advances even when nothing was gone. Its size is
// part of the statement, as the delete batch's is.
const stClientKeyHistoryDiscoverPageSqlTemplate = `
	WITH page AS MATERIALIZED (
		SELECT client_id
		FROM st_client_key_head
		{{cursor}}
		ORDER BY client_id
		LIMIT {{limit}}
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

// The first page of a discovery pass, or with resumed the page after the
// cursor ($1), of batchCount heads.
func stClientKeyHistoryDiscoverPageSql(batchCount int, resumed bool) string {
	cursor := ""
	if resumed {
		cursor = "WHERE client_id > $1"
	}
	return strings.NewReplacer(
		"{{cursor}}", cursor,
		"{{limit}}", strconv.Itoa(batchCount),
	).Replace(stClientKeyHistoryDiscoverPageSqlTemplate)
}

// Deletes the key history of clients gone longer than the retention, then
// continues the discovery pass of clients gone without a record, each in
// short transactions until the run budget is spent. The deletes are paced by
// the records their batches read, so a region of records the batches cannot
// delete is walked at the same pace.
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
		batch, bounded := removeGoneStClientKeyHistoryBatch(ctx, minGoneTime, next.DeleteAfterGoneTime, next.DeleteAfterClientId, settings)
		if bounded {
			// the cursor stays, so the next run retries the page
			report.DeleteStop = StClientKeyHistoryRetentionStopBatchBound
			return
		}
		report.ExaminedRecordCount += batch.examinedCount
		report.RemovedClientCount += int64(len(batch.clientIds))
		report.RemovedHistoryCount += batch.historyCount
		removeGoneClientPublicKeys(ctx, batch.clientIds)
		if batch.examinedCount < int64(settings.DeleteBatchCount) {
			// the pass is complete; the next starts at the oldest record
			next.DeleteAfterGoneTime, next.DeleteAfterClientId = nil, nil
			report.DeleteStop = StClientKeyHistoryRetentionStopDue
			break
		}
		next.DeleteAfterGoneTime, next.DeleteAfterClientId = &batch.lastGoneTime, &batch.lastClientId
		if !pace.WaitAfter(ctx, int(batch.examinedCount), brake.MaxClientsPerSecond, deleteDeadline) || !time.Now().Before(deleteDeadline) {
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
		scannedCount, discoveredCount, lastClientId, bounded := discoverGoneStClientKeyHistoryPage(ctx, next.DiscoverAfterClientId, settings)
		if bounded {
			// the cursor stays, so the next run retries the page
			report.DiscoverBounded = true
			return
		}
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

// What one committed batch read and removed, and the last record of its page:
// the cursor of the batch after it.
type goneStClientKeyHistoryBatch struct {
	examinedCount int64
	clientIds     []server.Id
	historyCount  int64
	lastGoneTime  time.Time
	lastClientId  server.Id
}

// One short transaction (boundedTx): the page and its due records, then heads
// first, since a retained head marks a complete archived history
// (db_client_key_policy_history.go), then the history, then the records. The
// page comes after (afterGoneTime, afterClientId), the start of the pass when
// nil. bounded is true when the transaction ran into one of its bounds and
// rolled back, removing nothing; any other raise propagates.
func removeGoneStClientKeyHistoryBatch(
	ctx context.Context,
	minGoneTime time.Time,
	afterGoneTime *time.Time,
	afterClientId *server.Id,
	settings *StClientKeyHistoryRetentionSettings,
) (batch goneStClientKeyHistoryBatch, bounded bool) {
	defer func() {
		if r := recover(); r != nil {
			if !stClientKeyHistoryRetentionBounded(ctx, r) {
				panic(r)
			}
			glog.Infof("[ckh]a delete batch ran into its bound and rolled back, deleting nothing more this run: %s\n", server.ErrorJsonNoStack(r))
			batch = goneStClientKeyHistoryBatch{}
			bounded = true
		}
	}()
	var removedHeadCount, removedGoneCount int64
	settings.boundedTx(ctx, func(txCtx context.Context, tx server.PgTx) {
		// an attempt that does not commit removed nothing
		batch = goneStClientKeyHistoryBatch{}
		removedHeadCount = 0
		removedGoneCount = 0

		server.RaisePgResult(tx.Exec(txCtx, `SET LOCAL urnetwork.client_key_history_retention = 'on'`))
		var row pgx.Row
		if afterGoneTime == nil || afterClientId == nil {
			row = tx.QueryRow(txCtx, stClientKeyHistoryRetentionBatchSql(settings.DeleteBatchCount, false), minGoneTime)
		} else {
			row = tx.QueryRow(txCtx, stClientKeyHistoryRetentionBatchSql(settings.DeleteBatchCount, true), minGoneTime, afterGoneTime.UTC(), *afterClientId)
		}
		var clientIds []server.Id
		err := row.Scan(&batch.examinedCount, &clientIds, &batch.lastGoneTime, &batch.lastClientId)
		if errors.Is(err, pgx.ErrNoRows) {
			// an empty page: nothing after the cursor is due
			batch = goneStClientKeyHistoryBatch{}
			return
		}
		server.Raise(err)
		if len(clientIds) == 0 {
			return
		}
		headTag := server.RaisePgResult(tx.Exec(
			txCtx,
			`DELETE FROM st_client_key_head WHERE client_id = ANY($1)`,
			clientIds,
		))
		tag := server.RaisePgResult(tx.Exec(
			txCtx,
			`DELETE FROM st_client_key_history WHERE client_id = ANY($1)`,
			clientIds,
		))
		goneTag := server.RaisePgResult(tx.Exec(
			txCtx,
			`DELETE FROM st_client_key_gone WHERE client_id = ANY($1)`,
			clientIds,
		))
		batch.clientIds = clientIds
		batch.historyCount = tag.RowsAffected()
		removedHeadCount = headTag.RowsAffected()
		removedGoneCount = goneTag.RowsAffected()
	})
	// counted once the batch committed
	countRetentionDeletedRows(RetentionTableStClientKeyHead, removedHeadCount)
	countRetentionDeletedRows(RetentionTableStClientKeyHistory, batch.historyCount)
	countRetentionDeletedRows(RetentionTableStClientKeyGone, removedGoneCount)
	return
}

// One discovery transaction (boundedTx). lastClientId is the page's last head
// client, nil when the page was empty. bounded is true when the transaction
// ran into one of its bounds and rolled back, recording nothing; any other
// raise propagates.
func discoverGoneStClientKeyHistoryPage(
	ctx context.Context,
	afterClientId *server.Id,
	settings *StClientKeyHistoryRetentionSettings,
) (scannedCount int64, discoveredCount int64, lastClientId *server.Id, bounded bool) {
	defer func() {
		if r := recover(); r != nil {
			if !stClientKeyHistoryRetentionBounded(ctx, r) {
				panic(r)
			}
			glog.Infof("[ckh]a discovery page ran into its bound and rolled back, discovering nothing more this run: %s\n", server.ErrorJsonNoStack(r))
			scannedCount, discoveredCount, lastClientId = 0, 0, nil
			bounded = true
		}
	}()
	settings.boundedTx(ctx, func(txCtx context.Context, tx server.PgTx) {
		scannedCount, discoveredCount, lastClientId = 0, 0, nil
		var row pgx.Row
		if afterClientId == nil {
			row = tx.QueryRow(txCtx, stClientKeyHistoryDiscoverPageSql(settings.DiscoverBatchCount, false))
		} else {
			row = tx.QueryRow(txCtx, stClientKeyHistoryDiscoverPageSql(settings.DiscoverBatchCount, true), *afterClientId)
		}
		var bound server.Id
		err := row.Scan(&scannedCount, &discoveredCount, &bound)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		server.Raise(err)
		lastClientId = &bound
	})
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
