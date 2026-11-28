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
// What a batch deletes is bounded by history rows, not clients: at most
// DeleteBatchRowCount registrations, so a client that rotated its key hundreds
// of times goes over several batches: its heads, then its history in chunks,
// then its record, an order the guard admits. The cursor moves only past
// clients whose every row is gone. Until 2026-11-23 a batch deleted the whole
// history of its page's due clients in one statement; from 2026-11-21 the page
// at the cursor held 500 clients with 73,879 registrations, every run's first
// batch was canceled at its 5 s statement bound and rolled back, and nothing
// was deleted for two days while every run reported success
// (DBDISK-RETENTION-REVIEW.md §3.3). A batch that runs into a bound now fails
// its run.
// A pass also starts over at the oldest record once it is
// DeletePassRestartInterval old. The reap of retired children records them
// gone as of their deactivation, 30 days earlier, so their records are written
// behind a cursor that has already passed those times; until 2026-11-23 they
// waited for the pass to reach the horizon, which on Main it never did again
// (55.3M of the 56.5M due records on 2026-11-23).
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
	"github.com/prometheus/client_golang/prometheus"
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
	// drains under watch. The brake raises it. A batch counts the records it
	// read or the registrations it deleted, whichever is more, so the rate
	// also bounds the registrations deleted per second.
	DefaultDeleteClientsPerSecond float64
	// The highest rate the brake can set.
	MaxDeleteClientsPerSecond float64
	// Bounds each brake and horizon control read.
	ControlTimeout time.Duration
	// Gone records one delete batch reads, in (gone_time, client_id) order
	// after the pass's cursor: the most clients it deletes, and the bound on
	// what it examines, whatever share of the records is due and however many
	// of their clients still have a row.
	DeleteBatchCount int
	// History rows one delete batch deletes at most, from its page's due
	// clients in order, so what a batch deletes, and so its time, is bounded
	// by rows however many registrations a client owns. A client with more
	// rows than remain in a batch keeps the rest, and its record, for the next
	// batch, and the cursor stays before it. Positive.
	DeleteBatchRowCount int
	// A delete pass that began this long ago starts over at the oldest record
	// at the next run, so a record written behind the cursor, such as a reaped
	// child gone as of its deactivation 30 days earlier, waits at most about
	// this long. A start over reads past the index entries of the records
	// deleted since the table's last vacuum once.
	DeletePassRestartInterval time.Duration
	// Due records the probe after each run's deletes counts at most, for the
	// due gauge the stall alert reads. Non-positive skips the probe.
	DueProbeLimit int
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
		// A page of single-registration clients whole. On Main the history
		// delete took 163 ms on average for a page (about 3,000 rows a second)
		// and one canceled attempt got through about 61k rows in its 5 s
		// (2026-11-23), so 1,000 rows take a third of a second to a tenth of
		// that: well inside the 5 s statement bound
		DeleteBatchRowCount: 1000,
		// at 1,000 a second the deletes of an hour, about 2.4M records, are
		// what a start over reads past; locally 1M unvacuumed deleted entries
		// took 90 ms to read past, and 4 ms once marked
		DeletePassRestartInterval: time.Hour,
		DueProbeLimit:             1000000,
		DiscoverBatchCount:        10000,
		DiscoverInterval:          7 * 24 * time.Hour,
		RunBudget:                 4 * time.Minute,
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
// st_client_key_gone with all of network_client. Its history reads and deletes
// probe st_client_key_history by key the same way. No table is joined any
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
	// the last gone record a delete pass is done with, in (gone_time,
	// client_id) order: deleted, or passed over because another transaction
	// held it or its client still had a row; never one whose client still has
	// history to delete. Nil starts a pass at the oldest record. A pass ends
	// when a batch reads a short page and finishes it, so the next one rereads
	// what an earlier batch passed over.
	DeleteAfterGoneTime *time.Time `json:"delete_after_gone_time,omitempty"`
	DeleteAfterClientId *server.Id `json:"delete_after_client_id,omitempty"`
	// when the current delete pass began at the oldest record. A pass that
	// began DeletePassRestartInterval ago, or whose start is unknown (a cursor
	// from before 2026-11-23), starts over there at the next run.
	DeletePassStartTime *time.Time `json:"delete_pass_start_time,omitempty"`
	// the last head client a discovery pass read; nil starts a pass
	DiscoverAfterClientId *server.Id `json:"discover_after_client_id,omitempty"`
	// when the last complete discovery pass ended
	DiscoverCompleteTime *time.Time `json:"discover_complete_time,omitempty"`
}

// Why a run's deletes ended.
const (
	// a batch read a short page and finished it: nothing more is due
	StClientKeyHistoryRetentionStopDue = "due"
	// the delete half of the run budget is spent
	StClientKeyHistoryRetentionStopBudget = "budget"
	// the brake is off, or sets no rate
	StClientKeyHistoryRetentionStopBrakeOff = "brake_off"
	// the brake could not be read
	StClientKeyHistoryRetentionStopBrakeUnread = "brake_unread"
	StClientKeyHistoryRetentionStopCanceled    = "canceled"
	// a batch ran into one of its bounds (BatchTimeout, BatchStatementTimeout,
	// BatchLockTimeout) and rolled back; the run fails, the next run retries
	// from the cursor of the batches that committed, and this run discovers
	// nothing either
	StClientKeyHistoryRetentionStopBatchBound = "batch_bound"
)

var stClientKeyHistoryRetentionStops = []string{
	StClientKeyHistoryRetentionStopDue,
	StClientKeyHistoryRetentionStopBudget,
	StClientKeyHistoryRetentionStopBrakeOff,
	StClientKeyHistoryRetentionStopBrakeUnread,
	StClientKeyHistoryRetentionStopCanceled,
	StClientKeyHistoryRetentionStopBatchBound,
}

var stClientKeyHistoryRetentionRunsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_client_key_history_retention_runs_total",
	Help: "Runs of the key history retention's deletes, by why they ended; batch_bound is a batch that ran into its bound and rolled back, which fails the run",
}, []string{"stop"})

func init() {
	for _, stop := range stClientKeyHistoryRetentionStops {
		stClientKeyHistoryRetentionRunsCounter.WithLabelValues(stop)
	}
	prometheus.MustRegister(stClientKeyHistoryRetentionRunsCounter)
}

type StClientKeyHistoryRetentionReport struct {
	// how long after its client went history was deleted in this run
	Horizon time.Duration
	// why the deletes ended, one of StClientKeyHistoryRetentionStop*
	DeleteStop string
	// the delete pass started over at the oldest record at the run's start:
	// it had run DeletePassRestartInterval, or its start was unknown
	DeletePassRestarted bool
	// gone records the delete batches read, deleted or not; a page a batch
	// did not finish is read again
	ExaminedRecordCount int64
	// gone clients whose every registration, head and record went
	RemovedClientCount  int64
	RemovedHistoryCount int64
	// records due at the run's end, at most DueProbeLimit; -1 when the probe
	// did not run or did not complete
	DueRecordCount int64
	// clients the discovery found gone without a record of when
	DiscoveredClientCount int64
	// a discovery page ran into one of its bounds and rolled back; the pass
	// resumes from its cursor at the next run
	DiscoverBounded bool
	// work remains for an immediate rerun
	More bool
}

// A delete batch or a discovery page ran into its bound and rolled back. The
// run fails: the task reports it, and its retry resumes from the cursor of
// what committed.
func (self *StClientKeyHistoryRetentionReport) Bounded() bool {
	return self.DeleteStop == StClientKeyHistoryRetentionStopBatchBound || self.DiscoverBounded
}

// One delete batch's page: the records gone longer than the retention,
// oldest first, after the pass's cursor, then the page's records whose client
// has no row, which the batch locks to delete. The page is a range of
// st_client_key_gone_gone_time cut at the page size before any client is
// checked, so the batch reads at most the page (and the rest of the page's
// last gone_time, which the client_id order sorts) and probes network_client
// by key once per record of it. Its size is part of the statement, so that a
// generic plan knows the page is small. The cursor keeps a pass from reading
// again the index entries of the records earlier batches deleted, which stay
// until vacuum. Both bounds repeat the guard's floor on the database clock,
// so a batch never holds a row the guard refuses. One row: the page's records
// in order, as clients and gone times, and its due clients.
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
	)
	SELECT
		ARRAY(SELECT client_id FROM page ORDER BY gone_time, client_id),
		ARRAY(SELECT gone_time FROM page ORDER BY gone_time, client_id),
		ARRAY(SELECT client_id FROM due)
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

// The registrations one batch deletes: the first rows of its due clients ($1,
// in page order), at most the limit. A nested loop over the array probes the
// primary key by client and stops at the limit, so a batch reads and deletes
// at most that many rows however many a client owns. The array's order only
// decides which clients go first: the batch learns which clients it finished
// from the table. Its size is part of the statement, as the page's is.
const stClientKeyHistoryRetentionRowsSqlTemplate = `
	SELECT history.client_id, history.domain_hash, history.generation
	FROM unnest($1::uuid[]) AS due(client_id)
	CROSS JOIN LATERAL (
		SELECT
			st_client_key_history.client_id,
			st_client_key_history.domain_hash,
			st_client_key_history.generation
		FROM st_client_key_history
		WHERE st_client_key_history.client_id = due.client_id
		LIMIT {{limit}}
	) AS history
	LIMIT {{limit}}
`

// At most rowCount registrations of the due clients.
func stClientKeyHistoryRetentionRowsSql(rowCount int) string {
	return strings.NewReplacer(
		"{{limit}}", strconv.Itoa(rowCount),
	).Replace(stClientKeyHistoryRetentionRowsSqlTemplate)
}

// Every head of the clients ($1) whose registrations the batch deletes: a
// head names a registration, and the guard admits a history delete only once
// its client has none.
const stClientKeyHistoryRetentionDeleteHeadsSql = `
	DELETE FROM st_client_key_head WHERE client_id = ANY($1)
`

// The registrations the batch read, each by its primary key.
const stClientKeyHistoryRetentionDeleteRowsSql = `
	DELETE FROM st_client_key_history
	USING unnest($1::uuid[], $2::bytea[], $3::bigint[]) AS doomed(client_id, domain_hash, generation)
	WHERE
		st_client_key_history.client_id = doomed.client_id AND
		st_client_key_history.domain_hash = doomed.domain_hash AND
		st_client_key_history.generation = doomed.generation
`

// The records of the due clients ($1) that have no registration left: the
// clients the batch finished, which its cursor may pass.
const stClientKeyHistoryRetentionDeleteRecordsSql = `
	DELETE FROM st_client_key_gone
	WHERE
		client_id = ANY($1) AND
		NOT EXISTS (
			SELECT 1 FROM st_client_key_history
			WHERE st_client_key_history.client_id = st_client_key_gone.client_id
		)
	RETURNING client_id
`

// The records due at $1, at most a limit, through the gone_time index: those
// whose client still has a row count too (none on Main's 2026-11-23 sample).
const stClientKeyHistoryDueProbeSqlTemplate = `
	SELECT count(*) FROM (
		SELECT 1 FROM st_client_key_gone
		WHERE
			gone_time < $1 AND
			gone_time < (now() AT TIME ZONE 'UTC') - st_client_key_history_retention_floor()
		LIMIT {{limit}}
	) AS due
`

func stClientKeyHistoryDueProbeSql(limit int) string {
	return strings.NewReplacer(
		"{{limit}}", strconv.Itoa(limit),
	).Replace(stClientKeyHistoryDueProbeSqlTemplate)
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
// probes what is still due and continues the discovery pass of clients gone
// without a record, each in short transactions until the run budget is spent.
// The deletes are paced by the records their batches read or the rows they
// deleted, whichever is more, so a region of records the batches cannot
// delete is walked at the same pace, and a client with many rows goes at the
// same rate in rows.
func RemoveGoneStClientKeyHistory(
	ctx context.Context,
	now time.Time,
	cursor StClientKeyHistoryRetentionCursor,
	settings *StClientKeyHistoryRetentionSettings,
) (report *StClientKeyHistoryRetentionReport, next StClientKeyHistoryRetentionCursor) {
	report = &StClientKeyHistoryRetentionReport{DueRecordCount: -1}
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

	// a pass that has run its interval, or whose start is unknown, starts
	// over at the oldest record, which reaches the records written behind it
	if next.DeleteAfterGoneTime != nil || next.DeleteAfterClientId != nil {
		if next.DeletePassStartTime == nil || !now.Before(next.DeletePassStartTime.Add(settings.DeletePassRestartInterval)) {
			next.DeleteAfterGoneTime, next.DeleteAfterClientId, next.DeletePassStartTime = nil, nil, nil
			report.DeletePassRestarted = true
		}
	}

	settings.removeGone(ctx, now, minGoneTime, deleteDeadline, report, &next)
	stClientKeyHistoryRetentionRunsCounter.WithLabelValues(report.DeleteStop).Inc()
	if report.DeleteStop == StClientKeyHistoryRetentionStopCanceled {
		return
	}
	report.DueRecordCount = settings.probeDue(ctx, minGoneTime)
	if report.DeleteStop == StClientKeyHistoryRetentionStopBatchBound {
		return
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

// The run's deletes: batches from the cursor until a batch finishes a short
// page, the delete half of the budget is spent, the brake stops them or a
// batch runs into its bound, each paced to the brake's rate. Sets the report's
// stop and counts, and moves next past what the committed batches finished.
func (self *StClientKeyHistoryRetentionSettings) removeGone(
	ctx context.Context,
	now time.Time,
	minGoneTime time.Time,
	deadline time.Time,
	report *StClientKeyHistoryRetentionReport,
	next *StClientKeyHistoryRetentionCursor,
) {
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
		brake, err := GetStClientKeyHistoryRetentionControl(ctx, self)
		if err != nil {
			glog.V(1).Infof("[ckh]retention control unread, deleting nothing more this run: %s\n", err)
			report.DeleteStop = StClientKeyHistoryRetentionStopBrakeUnread
			return
		}
		if !brake.On {
			report.DeleteStop = StClientKeyHistoryRetentionStopBrakeOff
			return
		}
		passStart := next.DeleteAfterGoneTime == nil || next.DeleteAfterClientId == nil
		batch, bounded := removeGoneStClientKeyHistoryBatch(ctx, minGoneTime, next.DeleteAfterGoneTime, next.DeleteAfterClientId, self)
		if bounded {
			// the cursor stays, so the retry rereads the page
			report.DeleteStop = StClientKeyHistoryRetentionStopBatchBound
			return
		}
		report.ExaminedRecordCount += batch.examinedCount
		report.RemovedClientCount += batch.removedClientCount
		report.RemovedHistoryCount += batch.historyCount
		removeGoneClientPublicKeys(ctx, batch.headClientIds)
		if batch.passComplete {
			// the next pass starts at the oldest record
			next.DeleteAfterGoneTime, next.DeleteAfterClientId, next.DeletePassStartTime = nil, nil, nil
			report.DeleteStop = StClientKeyHistoryRetentionStopDue
			return
		}
		if passStart && next.DeletePassStartTime == nil {
			passStartTime := now
			next.DeletePassStartTime = &passStartTime
		}
		if 0 < batch.advancedCount {
			lastGoneTime, lastClientId := batch.lastGoneTime, batch.lastClientId
			next.DeleteAfterGoneTime, next.DeleteAfterClientId = &lastGoneTime, &lastClientId
		}
		if !pace.WaitAfter(ctx, int(max(batch.examinedCount, batch.historyCount)), brake.MaxClientsPerSecond, deadline) || !time.Now().Before(deadline) {
			report.More = true
			report.DeleteStop = StClientKeyHistoryRetentionStopBudget
			return
		}
	}
}

// What one committed batch read and removed, and where its cursor moved.
type goneStClientKeyHistoryBatch struct {
	// records the page read
	examinedCount int64
	// due clients whose heads the batch deleted with some or all of their
	// registrations
	headClientIds []server.Id
	// due clients whose every registration, head and record are gone
	removedClientCount int64
	historyCount       int64
	// records of the page, from its start, that the batch is done with:
	// deleted, or passed over, up to the first due client with registrations
	// left
	advancedCount int64
	// the last of those, the cursor of the batch after it, while
	// advancedCount is positive
	lastGoneTime time.Time
	lastClientId server.Id
	// the page was short and the batch finished it: the pass is complete
	passComplete bool
}

// One short transaction (boundedTx): the page and its due records, then at
// most DeleteBatchRowCount of their registrations, the due clients in page
// order: the heads of the clients those belong to first, since a retained
// head marks a complete archived history (db_client_key_policy_history.go),
// then the registrations, then the records of the due clients with none left.
// The page comes after (afterGoneTime, afterClientId), the start of the pass
// when nil. bounded is true when the transaction ran into one of its bounds
// and rolled back, removing nothing; any other raise propagates.
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
	var removedHeadCount int64
	settings.boundedTx(ctx, func(txCtx context.Context, tx server.PgTx) {
		// an attempt that does not commit removed nothing
		batch = goneStClientKeyHistoryBatch{}
		removedHeadCount = 0

		server.RaisePgResult(tx.Exec(txCtx, `SET LOCAL urnetwork.client_key_history_retention = 'on'`))
		var row pgx.Row
		if afterGoneTime == nil || afterClientId == nil {
			row = tx.QueryRow(txCtx, stClientKeyHistoryRetentionBatchSql(settings.DeleteBatchCount, false), minGoneTime)
		} else {
			row = tx.QueryRow(txCtx, stClientKeyHistoryRetentionBatchSql(settings.DeleteBatchCount, true), minGoneTime, afterGoneTime.UTC(), *afterClientId)
		}
		var pageClientIds []server.Id
		var pageGoneTimes []time.Time
		var dueClientIds []server.Id
		server.Raise(row.Scan(&pageClientIds, &pageGoneTimes, &dueClientIds))
		if len(pageClientIds) != len(pageGoneTimes) {
			server.Raise(fmt.Errorf("key history retention page has %d clients and %d gone times", len(pageClientIds), len(pageGoneTimes)))
		}
		batch.examinedCount = int64(len(pageClientIds))
		due := map[server.Id]bool{}
		for _, clientId := range dueClientIds {
			due[clientId] = true
		}
		// the due clients in page order
		pageDueClientIds := []server.Id{}
		for _, clientId := range pageClientIds {
			if due[clientId] {
				pageDueClientIds = append(pageDueClientIds, clientId)
			}
		}

		removed := map[server.Id]bool{}
		if 0 < len(pageDueClientIds) {
			rowClientIds := []server.Id{}
			rowDomainHashes := [][]byte{}
			rowGenerations := []int64{}
			result, err := tx.Query(txCtx, stClientKeyHistoryRetentionRowsSql(settings.DeleteBatchRowCount), pageDueClientIds)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var domainHash []byte
					var generation int64
					server.Raise(result.Scan(&clientId, &domainHash, &generation))
					rowClientIds = append(rowClientIds, clientId)
					rowDomainHashes = append(rowDomainHashes, domainHash)
					rowGenerations = append(rowGenerations, generation)
				}
			})
			headClientIds := []server.Id{}
			seen := map[server.Id]bool{}
			for _, clientId := range rowClientIds {
				if !seen[clientId] {
					seen[clientId] = true
					headClientIds = append(headClientIds, clientId)
				}
			}
			if 0 < len(headClientIds) {
				headTag := server.RaisePgResult(tx.Exec(txCtx, stClientKeyHistoryRetentionDeleteHeadsSql, headClientIds))
				historyTag := server.RaisePgResult(tx.Exec(
					txCtx,
					stClientKeyHistoryRetentionDeleteRowsSql,
					rowClientIds,
					rowDomainHashes,
					rowGenerations,
				))
				removedHeadCount = headTag.RowsAffected()
				batch.historyCount = historyTag.RowsAffected()
				batch.headClientIds = headClientIds
			}
			result, err = tx.Query(txCtx, stClientKeyHistoryRetentionDeleteRecordsSql, pageDueClientIds)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					server.Raise(result.Scan(&clientId))
					removed[clientId] = true
				}
			})
			batch.removedClientCount = int64(len(removed))
		}

		// the cursor passes the page's records up to the first due client
		// with registrations left
		for i, clientId := range pageClientIds {
			if due[clientId] && !removed[clientId] {
				break
			}
			batch.advancedCount = int64(i + 1)
			batch.lastGoneTime, batch.lastClientId = pageGoneTimes[i].UTC(), clientId
		}
		batch.passComplete = batch.advancedCount == batch.examinedCount && batch.examinedCount < int64(settings.DeleteBatchCount)
	})
	// counted once the batch committed
	countRetentionDeletedRows(RetentionTableStClientKeyHead, removedHeadCount)
	countRetentionDeletedRows(RetentionTableStClientKeyHistory, batch.historyCount)
	countRetentionDeletedRows(RetentionTableStClientKeyGone, batch.removedClientCount)
	return
}

// Counts the records due at minGoneTime, at most DueProbeLimit, in one bounded
// transaction, and publishes the count as the gone records' due rows. -1 when
// the probe is off or did not complete; the gauge then keeps its last count.
// A probe that fails never fails the run, whose deletes have committed.
func (self *StClientKeyHistoryRetentionSettings) probeDue(ctx context.Context, minGoneTime time.Time) (dueCount int64) {
	if self.DueProbeLimit <= 0 {
		return -1
	}
	defer func() {
		if r := recover(); r != nil {
			glog.Infof("[ckh]the due probe did not complete; its gauge keeps its last count: %s\n", server.ErrorJsonNoStack(r))
			dueCount = -1
		}
	}()
	self.boundedTx(ctx, func(txCtx context.Context, tx server.PgTx) {
		server.Raise(tx.QueryRow(txCtx, stClientKeyHistoryDueProbeSql(self.DueProbeLimit), minGoneTime).Scan(&dueCount))
	})
	recordRetentionDueRows(RetentionTableStClientKeyGone, dueCount, time.Now())
	return dueCount
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
