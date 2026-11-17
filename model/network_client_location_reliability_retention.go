// The write bounds and the retention of network_client_location_reliability:
// how often a connected client's update_block_number is refreshed, the counts
// of each location reliability pass, and the deletes that read the block.
//
// A row is one client's location fold. It goes with its client: the reaps'
// shared cascade deletes it once the client's delete commits
// (removeReapedNetworkClientDependents). Otherwise it goes once its client has
// not been seen for a while (owner 2026-11-16, "11 days except top-level";
// PARTITION-CANDIDATES.md §9.1):
//   - a top-level client's row 31 days after its last refresh
//     (TopLevelRetention and the refresh lag). The provider status page reads
//     every active top-level client's row with no time bound
//     (loadProviderStatusFacts), and a top-level client stays active 30 days
//     idle (TopLevelClientIdleExpiration). bringyourctl provider inspect reads
//     one by id (InspectProviderEgress), and payout weights read the rows of
//     the providers in a window up to client_reliability's 10 days old
//     (readReliabilityWindowWeights), which would leave no margin at 11 days.
//   - a child's or a gone client's row 11 days after (ShortRetention and the
//     lag). Provider status reads top-level clients only; a child is a window
//     client, which never provides, so no payout window holds it; and the
//     other readers join network_client, which a gone client has left.
//
// One taskworker chain (work.RemoveOldClientLocationReliabilities) runs both
// parts. The trim deletes every row past 31 days, oldest first, as before.
// The walk then reads the rows past 11 days in (update_block_number,
// client_id) order from its cursor, deletes those of children and gone
// clients, and passes over top-level ones, which the trim takes at 31 days.
// A row's block only moves forward, to its pass's block, so every row that
// crosses the walk's cutoff is ahead of the cursor, and the walk examines it
// once. The walk is paced to a rows-per-second ceiling that its Redis brake
// can lower or stop; stopped, every row keeps its 31 days as before.
//
// Safe for concurrent use; one chain runs at a time.
package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// A connected client's update_block_number is rewritten only once it is this
// old (UpdateClientLocationReliabilitiesInTx), so a pass rewrites a row only
// when one of its values changed or once a day. The block number can then lag
// the client's last pass by this much, and retention allows for it.
const ClientLocationReliabilityRefreshInterval = 24 * time.Hour

// ClientLocationReliabilityRefreshInterval in reliability blocks.
const clientLocationReliabilityRefreshBlocks = int64(ClientLocationReliabilityRefreshInterval / ReliabilityBlockDuration)

// The clients the last pass computed, by identity kind. A hosted child
// identity (network_client.source_client_id set) is a client most readers
// exclude; its share is how much of the pass's work they skip.
var clientLocationReliabilityPassClients = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_client_location_reliability_pass_clients",
	Help: "Clients the last client location reliability pass computed, by identity kind: hosted_child (source_client_id set) or top_level.",
}, []string{"identity"})

// The rows the last pass inserted or rewrote; the rest kept their row.
var clientLocationReliabilityPassWrittenRows = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_client_location_reliability_pass_written_rows",
	Help: "Rows the last client location reliability pass inserted or rewrote; its other clients were unchanged and their rows were not written.",
})

// Why the walk's run ended.
const (
	// a batch came back short: the walk reached its cutoff
	ClientLocationReliabilityRetentionStopCaughtUp = "caught_up"
	// the walk's budget is spent
	ClientLocationReliabilityRetentionStopBudget = "budget"
	// the brake is off, or sets no rate
	ClientLocationReliabilityRetentionStopBrakeOff = "brake_off"
	// the brake could not be read
	ClientLocationReliabilityRetentionStopBrakeUnread = "brake_unread"
	ClientLocationReliabilityRetentionStopCanceled    = "canceled"
	// a batch failed; the next run retries it from the cursor
	ClientLocationReliabilityRetentionStopError = "error"
)

var clientLocationReliabilityRetentionStops = []string{
	ClientLocationReliabilityRetentionStopCaughtUp,
	ClientLocationReliabilityRetentionStopBudget,
	ClientLocationReliabilityRetentionStopBrakeOff,
	ClientLocationReliabilityRetentionStopBrakeUnread,
	ClientLocationReliabilityRetentionStopCanceled,
	ClientLocationReliabilityRetentionStopError,
}

var clientLocationReliabilityRetentionRunsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_client_location_reliability_retention_runs_total",
	Help: "Runs of the client location reliability retention's walk (rows of children and gone clients 11 days after their last refresh), by why the run ended",
}, []string{"stop"})

var clientLocationReliabilityRetentionExaminedCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_client_location_reliability_retention_examined_rows_total",
	Help: "Rows the client location reliability retention's walk examined; the deletes count in urnetwork_retention_deleted_rows_total",
})

var clientLocationReliabilityRetentionCursorGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_client_location_reliability_retention_cursor_timestamp_seconds",
	Help: "The refresh time (update_block_number) of the last row the client location reliability retention's walk examined in this process: rows refreshed before it have been examined. Caught up, it trails now by the short window, 11 days",
})

func init() {
	for _, stop := range clientLocationReliabilityRetentionStops {
		clientLocationReliabilityRetentionRunsCounter.WithLabelValues(stop)
	}
	prometheus.MustRegister(
		clientLocationReliabilityPassClients,
		clientLocationReliabilityPassWrittenRows,
		clientLocationReliabilityRetentionRunsCounter,
		clientLocationReliabilityRetentionExaminedCounter,
		clientLocationReliabilityRetentionCursorGauge,
	)
}

// Publishes the counts of one pass: its clients by identity kind, and how many
// rows its upsert wrote.
func recordClientLocationReliabilityPass(clientLocationReliabilities map[server.Id]*clientLocationReliability, writtenRowCount int64) {
	hostedChildCount, topLevelCount := 0, 0
	for _, reliability := range clientLocationReliabilities {
		if reliability.hostedChild {
			hostedChildCount += 1
		} else {
			topLevelCount += 1
		}
	}
	clientLocationReliabilityPassClients.WithLabelValues("hosted_child").Set(float64(hostedChildCount))
	clientLocationReliabilityPassClients.WithLabelValues("top_level").Set(float64(topLevelCount))
	clientLocationReliabilityPassWrittenRows.Set(float64(writtenRowCount))
}

type ClientLocationReliabilityRetentionSettings struct {
	// A top-level client's row goes this long after the client's last pass,
	// so this and the refresh interval after its last refresh: as long as the
	// client can stay active idle, since the provider status page reads every
	// active top-level client's row.
	TopLevelRetention time.Duration
	// A child's or a gone client's row goes this long after the client's last
	// pass, this and the refresh interval after its last refresh (owner
	// 2026-11-16: 11 days except top-level).
	ShortRetention time.Duration
	// Rows the trim deletes in one transaction; positive.
	TrimBatchCount int
	// The trim starts no batch once this long has passed since it began, and
	// the next run continues.
	TrimBudget time.Duration
	// Rows the walk examines per second while the brake sets no rate: a
	// cautious start, so the backlog of 2026-11-16 (about 330M rows) drains
	// under watch in about four days; the brake raises it. Not positive, the
	// walk runs only at a rate the brake sets.
	DefaultRowsPerSecond float64
	// The highest rate the brake can set (the contract horizon reaper's
	// ceiling).
	MaxRowsPerSecond float64
	// Rows one walk transaction examines; positive.
	WalkBatchCount int
	// Bounds each brake read.
	ControlTimeout time.Duration
	// Bounds each walk batch's lock waits; a batch that times out ends the
	// run with an error, and the next run retries it.
	BatchLockTimeout time.Duration
	// One run's walk.
	WalkBudget time.Duration
	// The chain's schedule: a run that left work starts again after
	// MoreInterval, otherwise after RunInterval; the chain starts StartDelay
	// after a taskworker starts, and one claim is held at most TaskMaxTime.
	RunInterval  time.Duration
	MoreInterval time.Duration
	StartDelay   time.Duration
	TaskMaxTime  time.Duration
	// the clock the trim's budget is read from
	now func() time.Time
}

// 10,000 rows per trim transaction keeps each delete short. 90 s keeps the
// trim well inside the run while removing far more than a quarter hour of
// expirations. The walk's batch of 2,000 is one short transaction every two
// seconds at the default rate.
func DefaultClientLocationReliabilityRetentionSettings() *ClientLocationReliabilityRetentionSettings {
	return &ClientLocationReliabilityRetentionSettings{
		TopLevelRetention:    TopLevelClientIdleExpiration,
		ShortRetention:       10 * 24 * time.Hour,
		TrimBatchCount:       10000,
		TrimBudget:           90 * time.Second,
		DefaultRowsPerSecond: 1000,
		MaxRowsPerSecond:     2000,
		WalkBatchCount:       2000,
		ControlTimeout:       2 * time.Second,
		BatchLockTimeout:     2 * time.Second,
		WalkBudget:           4 * time.Minute,
		RunInterval:          15 * time.Minute,
		MoreInterval:         15 * time.Second,
		StartDelay:           15 * time.Minute,
		TaskMaxTime:          30 * time.Minute,
		now:                  server.NowUtc,
	}
}

// The largest update_block_number a window deletes at maxTime: the row of a
// client last seen more than retention before maxTime, whose block can trail
// that pass by the refresh interval. A row is never removed sooner than
// retention after its client was last seen.
func clientLocationReliabilityCutoffBlockNumber(maxTime time.Time, retention time.Duration) int64 {
	minTime := maxTime.Add(-retention - ClientLocationReliabilityRefreshInterval)
	return reliabilityBlockNumber(minTime) - 1
}

// The retention's runtime brake: the Redis hash
// `{client_location_reliability_retention}control`. It stops or slows the
// walk without a deploy and is read before every batch, so a change applies
// from the next batch. Its fields, all optional:
//   - `mode`: on or off. Off, or any other value, stops the walk.
//   - `maxRowsPerSecond`: rows the walk examines per second, at most the
//     code's MaxRowsPerSecond; zero, or a value that does not parse, stops the
//     walk.
//
// Without the hash the walk runs at DefaultRowsPerSecond. A run that cannot
// read the hash deletes nothing more. The trim at 31 days and the deletes with
// a client run whatever the brake says, as before it existed.
const clientLocationReliabilityRetentionControlKey = "{client_location_reliability_retention}control"

const (
	ClientLocationReliabilityRetentionControlFieldMode             = "mode"
	ClientLocationReliabilityRetentionControlFieldMaxRowsPerSecond = "maxRowsPerSecond"
)

// The effective brake: the code settings as the Redis hash narrowed them.
type ClientLocationReliabilityRetentionControl struct {
	// the hash was present
	Present bool
	// rows the walk examines per second; not positive, the walk does not run
	RowsPerSecond float64
}

// The brake the fields narrow the settings to. Pure: fields is the hash as
// read, empty when the key is absent. A mode other than on stops the walk,
// and a rate that does not parse is zero.
func NarrowClientLocationReliabilityRetentionControl(fields map[string]string, settings *ClientLocationReliabilityRetentionSettings) *ClientLocationReliabilityRetentionControl {
	control := &ClientLocationReliabilityRetentionControl{
		Present:       0 < len(fields),
		RowsPerSecond: min(settings.DefaultRowsPerSecond, settings.MaxRowsPerSecond),
	}
	if value, ok := fields[ClientLocationReliabilityRetentionControlFieldMaxRowsPerSecond]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(rate) {
			rate = 0
		}
		control.RowsPerSecond = min(settings.MaxRowsPerSecond, rate)
	}
	if value, ok := fields[ClientLocationReliabilityRetentionControlFieldMode]; ok && strings.TrimSpace(value) != "on" {
		control.RowsPerSecond = 0
	}
	return control
}

// Reads the brake and narrows the settings with it. One bounded Redis read,
// never retried; on an error the caller deletes nothing more.
func GetClientLocationReliabilityRetentionControl(ctx context.Context, settings *ClientLocationReliabilityRetentionSettings) (control *ClientLocationReliabilityRetentionControl, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.ControlTimeout)
	defer cancel()
	var fields map[string]string
	returnErr = server.RedisWithDeadline(readCtx, func(r server.RedisClient) error {
		var err error
		fields, err = r.HGetAll(readCtx, clientLocationReliabilityRetentionControlKey).Result()
		return err
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return nil, returnErr
	}
	return NarrowClientLocationReliabilityRetentionControl(fields, settings), nil
}

// Writes fields into the brake, replacing those fields only. ctx must carry a
// deadline.
func SetClientLocationReliabilityRetentionControl(ctx context.Context, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	values := make([]any, 0, 2*len(fields))
	for field, value := range fields {
		values = append(values, field, value)
	}
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.HSet(ctx, clientLocationReliabilityRetentionControlKey, values...).Err()
	}, server.OptRedisNoPreflight())
}

// Deletes the brake, so the code settings apply. ctx must carry a deadline.
func RemoveClientLocationReliabilityRetentionControl(ctx context.Context) error {
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.Del(ctx, clientLocationReliabilityRetentionControlKey).Err()
	}, server.OptRedisNoPreflight())
}

// Where the walk resumes: the last row it examined, in (update_block_number,
// client_id) order. Nil starts at the oldest row. The rows behind the cursor
// were examined, and those still there are top-level rows, or the few a pass
// held at the time; the trim takes them at 31 days.
type ClientLocationReliabilityRetentionCursor struct {
	AfterUpdateBlockNumber *int64     `json:"after_update_block_number,omitempty"`
	AfterClientId          *server.Id `json:"after_client_id,omitempty"`
}

type ClientLocationReliabilityRetentionReport struct {
	// rows past the top-level window the trim deleted
	TrimmedCount int64
	// why the walk ended, one of ClientLocationReliabilityRetentionStop*
	Stop          string
	ExaminedCount int64
	// rows of children and gone clients the walk deleted
	RemovedCount int64
	// work remains for an immediate rerun
	More bool
}

// Deletes the rows past their windows: first every row past the top-level
// window, then, from cursor, the rows of children and gone clients past the
// short window, until the walk catches up with its cutoff, its budget is
// spent or the brake stops it.
func RemoveOldClientLocationReliabilities(
	ctx context.Context,
	now time.Time,
	cursor ClientLocationReliabilityRetentionCursor,
	settings *ClientLocationReliabilityRetentionSettings,
) (report *ClientLocationReliabilityRetentionReport, next ClientLocationReliabilityRetentionCursor) {
	report = &ClientLocationReliabilityRetentionReport{}
	var trimMore bool
	report.TrimmedCount, trimMore = settings.trim(ctx, now)
	next = settings.walk(ctx, now, cursor, report)
	report.More = report.More || trimMore
	return
}

// Deletes every row past the top-level window, oldest first, one batch per
// transaction, until a batch comes back short or the trim's budget has
// passed. Each batch rechecks the cutoff on the rows it deletes, so a row a
// concurrent pass just refreshed is kept. more is true when the budget ended
// the trim after a full batch.
func (self *ClientLocationReliabilityRetentionSettings) trim(ctx context.Context, maxTime time.Time) (removedCount int64, more bool) {
	maxBlockNumber := clientLocationReliabilityCutoffBlockNumber(maxTime, self.TopLevelRetention)
	startTime := self.now()
	for {
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				DELETE FROM network_client_location_reliability
				USING (
					SELECT client_id
					FROM network_client_location_reliability
					WHERE update_block_number <= $1
					ORDER BY update_block_number
					LIMIT $2
				) AS expired
				WHERE
					network_client_location_reliability.client_id = expired.client_id AND
					network_client_location_reliability.update_block_number <= $1
				`,
				maxBlockNumber,
				self.TrimBatchCount,
			))
			batchCount = tag.RowsAffected()
		}, server.TxReadCommitted)
		// counted once the batch committed
		countRetentionDeletedRows(RetentionTableNetworkClientLocationReliability, batchCount)
		removedCount += batchCount
		if batchCount < int64(self.TrimBatchCount) {
			return removedCount, false
		}
		if self.TrimBudget <= self.now().Sub(startTime) {
			return removedCount, true
		}
	}
}

// The walk's run: batches from the cursor until one comes back short, the
// budget is spent or the brake stops it, each paced to the brake's rate. A
// failed batch ends the run with the cursor of the last committed one.
func (self *ClientLocationReliabilityRetentionSettings) walk(
	ctx context.Context,
	now time.Time,
	cursor ClientLocationReliabilityRetentionCursor,
	report *ClientLocationReliabilityRetentionReport,
) (next ClientLocationReliabilityRetentionCursor) {
	next = cursor
	defer func() {
		clientLocationReliabilityRetentionRunsCounter.WithLabelValues(report.Stop).Inc()
		if next.AfterUpdateBlockNumber != nil {
			cursorTime := time.UnixMilli(*next.AfterUpdateBlockNumber * ReliabilityBlockDuration.Milliseconds())
			clientLocationReliabilityRetentionCursorGauge.Set(float64(cursorTime.Unix()))
		}
	}()
	cutoffBlockNumber := clientLocationReliabilityCutoffBlockNumber(now, self.ShortRetention)
	deadline := time.Now().Add(self.WalkBudget)
	pace := newRetentionPace()
	for {
		select {
		case <-ctx.Done():
			report.Stop = ClientLocationReliabilityRetentionStopCanceled
			report.More = true
			return
		default:
		}
		control, err := GetClientLocationReliabilityRetentionControl(ctx, self)
		if err != nil {
			if glog.V(1) {
				glog.Infof("[nclr]retention control unread, deleting nothing more this run: %s\n", err)
			}
			report.Stop = ClientLocationReliabilityRetentionStopBrakeUnread
			return
		}
		rate := control.RowsPerSecond
		if !(0 < rate) {
			report.Stop = ClientLocationReliabilityRetentionStopBrakeOff
			return
		}
		var examinedCount, removedCount int64
		var last *ClientLocationReliabilityRetentionCursor
		batchErr := server.HandleError(func() {
			examinedCount, removedCount, last = self.removeShortWindowBatch(ctx, cutoffBlockNumber, next)
		})
		if batchErr != nil {
			glog.Errorf("[nclr]retention batch after %+v failed; the next run retries it: %s\n", next, batchErr)
			report.Stop = ClientLocationReliabilityRetentionStopError
			report.More = true
			return
		}
		report.ExaminedCount += examinedCount
		report.RemovedCount += removedCount
		clientLocationReliabilityRetentionExaminedCounter.Add(float64(examinedCount))
		if last != nil {
			next = *last
		}
		if examinedCount < int64(self.WalkBatchCount) {
			report.Stop = ClientLocationReliabilityRetentionStopCaughtUp
			return
		}
		if !pace.WaitAfter(ctx, int(examinedCount), rate, deadline) || !time.Now().Before(deadline) {
			report.Stop = ClientLocationReliabilityRetentionStopBudget
			report.More = true
			return
		}
	}
}

// One page of rows past the short window's cutoff ($1) in
// (update_block_number, client_id) order after the cursor, a range of the
// update_block_number index, deleting those of clients that are not top-level
// clients, children and gone clients alike, in one statement. A row another
// transaction holds is skipped, and one a pass refreshed since the page was
// read is kept; both stay for the trim. The page size is part of the
// statement, so that a generic plan knows the page is small and probes by key.
const clientLocationReliabilityShortWindowSqlTemplate = `
	WITH page AS MATERIALIZED (
		SELECT client_id, update_block_number
		FROM network_client_location_reliability
		WHERE {{cursor}}update_block_number <= $1
		ORDER BY update_block_number, client_id
		LIMIT {{limit}}
	), due AS MATERIALIZED (
		SELECT reliability.client_id
		FROM page
		JOIN network_client_location_reliability AS reliability ON reliability.client_id = page.client_id
		WHERE
			reliability.update_block_number <= $1 AND
			NOT EXISTS (
				SELECT 1 FROM network_client
				WHERE
					network_client.client_id = reliability.client_id AND
					network_client.source_client_id IS NULL
			)
		FOR UPDATE OF reliability SKIP LOCKED
	), removed AS (
		DELETE FROM network_client_location_reliability AS reliability
		USING due
		WHERE reliability.client_id = due.client_id
		RETURNING 1
	), bound AS (
		SELECT update_block_number, client_id FROM page
		ORDER BY update_block_number DESC, client_id DESC
		LIMIT 1
	)
	SELECT (SELECT count(*) FROM page), (SELECT count(*) FROM removed), bound.update_block_number, bound.client_id
	FROM bound
`

// The first page of the walk, or with resumed a page after the cursor ($2,
// $3), of batchCount rows.
func clientLocationReliabilityShortWindowSql(batchCount int, resumed bool) string {
	cursor := ""
	if resumed {
		cursor = "(update_block_number, client_id) > ($2, $3) AND "
	}
	return strings.NewReplacer(
		"{{cursor}}", cursor,
		"{{limit}}", strconv.Itoa(batchCount),
	).Replace(clientLocationReliabilityShortWindowSqlTemplate)
}

func (self *ClientLocationReliabilityRetentionSettings) removeShortWindowBatch(
	ctx context.Context,
	cutoffBlockNumber int64,
	cursor ClientLocationReliabilityRetentionCursor,
) (examinedCount int64, removedCount int64, last *ClientLocationReliabilityRetentionCursor) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		// an attempt that does not commit removed nothing
		examinedCount, removedCount, last = 0, 0, nil
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, self.BatchLockTimeout.Milliseconds())))
		var row pgx.Row
		if cursor.AfterUpdateBlockNumber == nil || cursor.AfterClientId == nil {
			row = tx.QueryRow(ctx, clientLocationReliabilityShortWindowSql(self.WalkBatchCount, false), cutoffBlockNumber)
		} else {
			row = tx.QueryRow(ctx, clientLocationReliabilityShortWindowSql(self.WalkBatchCount, true), cutoffBlockNumber, *cursor.AfterUpdateBlockNumber, *cursor.AfterClientId)
		}
		var boundUpdateBlockNumber int64
		var boundClientId server.Id
		err := row.Scan(&examinedCount, &removedCount, &boundUpdateBlockNumber, &boundClientId)
		if errors.Is(err, pgx.ErrNoRows) {
			// an empty page: nothing past the cursor is due
			return
		}
		server.Raise(err)
		last = &ClientLocationReliabilityRetentionCursor{
			AfterUpdateBlockNumber: &boundUpdateBlockNumber,
			AfterClientId:          &boundClientId,
		}
	}, server.TxReadCommitted)
	// counted once the batch committed
	countRetentionDeletedRows(RetentionTableNetworkClientLocationReliability, removedCount)
	return
}
