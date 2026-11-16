// Retention of two tables that had no deletes (owner, 2026-11-16;
// DBDISK-STABILIZE.md S4): the provider work session journal's heads and the
// device audit state. One taskworker chain (work.RemoveExpiredSessionState)
// walks each table in key order from a cursor, in short batches, at a
// rows-per-second ceiling that a Redis brake can lower or stop. The two passes
// run side by side in a run, each with its own cursor, pace and stop.
//
// provider_work_session_head: a head goes once its client has no event left in
// the journal, whose weekly partitions drop at the contract horizon, no open
// connection and no receipt; so 35 to 42 days after the client's last
// connection opened or closed. The head guard repeats the event and receipt
// check and admits the delete only from this transaction's opt-in (server
// db_provider_work_session_head_retention.go). A head whose row another
// transaction holds is skipped, and a head that advanced since the page was
// read is kept: its client appended meanwhile.
//
// audit_device_state: an offline row goes once its device has been offline
// for DeviceStateRetention. SweepDeviceAuditEvents reads online rows only, so
// offline state is the same to it as no state, and the transition stays in
// audit_device_event for its 180 days, which the devices series reads. Online
// rows are never deleted here.
//
// Both walks are paced by the rows they examine. A pass that reaches the end
// of its table starts over no sooner than its pass interval after it began.
// Functions are safe concurrently; one chain runs at a time.
package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

type SessionStateRetentionSettings struct {
	// An offline device audit state row goes once its device has been offline
	// this long. Nothing reads offline state; the margin keeps the deletes
	// clear of rows the sweep, every 15 minutes, may still write. Non-positive
	// deletes offline rows as soon as the pass reaches them.
	DeviceStateRetention time.Duration
	// Rows each pass examines per second while the brake sets no rate for its
	// table: a backlog of 600M heads drains in about three and a half days,
	// 117M device states in about 16 hours (owner, 2026-11-16).
	DefaultRowsPerSecond float64
	// The highest rate the brake can set for a table (owner, 2026-11-16: at
	// most 2,000 rows a second, as the contract horizon reaper).
	MaxRowsPerSecond float64
	// Heads one transaction examines.
	HeadBatchCount int
	// Device states one transaction examines.
	DeviceStateBatchCount int
	// A head pass starts over no sooner than this after the last one began:
	// a pass rereads every head, and heads become due only as the journal's
	// weeks drop, weekly.
	HeadPassInterval time.Duration
	// A device state pass starts over no sooner than this after the last one
	// began, so an offline row goes at most this long after it is due.
	DeviceStatePassInterval time.Duration
	// Bounds each brake read.
	ControlTimeout time.Duration
	// Bounds each batch's lock waits; a batch that times out ends its pass's
	// run with an error, and the next run retries it.
	BatchLockTimeout time.Duration
	// One run's work, for each pass.
	RunBudget time.Duration
	// The chain's schedule: a run that left work starts again after
	// MoreInterval, otherwise after RunInterval; the chain starts StartDelay
	// after a taskworker starts, and one claim is held at most TaskMaxTime.
	RunInterval  time.Duration
	MoreInterval time.Duration
	StartDelay   time.Duration
	TaskMaxTime  time.Duration
}

func DefaultSessionStateRetentionSettings() *SessionStateRetentionSettings {
	return &SessionStateRetentionSettings{
		DeviceStateRetention:    24 * time.Hour,
		DefaultRowsPerSecond:    2000,
		MaxRowsPerSecond:        2000,
		HeadBatchCount:          1000,
		DeviceStateBatchCount:   2000,
		HeadPassInterval:        24 * time.Hour,
		DeviceStatePassInterval: 6 * time.Hour,
		ControlTimeout:          2 * time.Second,
		BatchLockTimeout:        2 * time.Second,
		RunBudget:               4 * time.Minute,
		RunInterval:             15 * time.Minute,
		MoreInterval:            15 * time.Second,
		StartDelay:              5 * time.Minute,
		TaskMaxTime:             30 * time.Minute,
	}
}

// The tables, as their metric labels.
const (
	SessionStateRetentionTableHead        = RetentionTableProviderWorkSessionHead
	SessionStateRetentionTableDeviceState = RetentionTableAuditDeviceState
)

// Why a pass's run ended.
const (
	// the pass reached the end of its table
	SessionStateRetentionStopPass = "pass"
	// the run budget is spent
	SessionStateRetentionStopBudget = "budget"
	// the pass is complete and the next is not due yet
	SessionStateRetentionStopWaiting = "waiting"
	// the brake is off for the table
	SessionStateRetentionStopBrakeOff = "brake_off"
	// the brake could not be read
	SessionStateRetentionStopBrakeUnread = "brake_unread"
	SessionStateRetentionStopCanceled    = "canceled"
	// a batch failed; the next run retries it
	SessionStateRetentionStopError = "error"
)

var sessionStateRetentionTables = []string{SessionStateRetentionTableHead, SessionStateRetentionTableDeviceState}

var sessionStateRetentionStops = []string{
	SessionStateRetentionStopPass,
	SessionStateRetentionStopBudget,
	SessionStateRetentionStopWaiting,
	SessionStateRetentionStopBrakeOff,
	SessionStateRetentionStopBrakeUnread,
	SessionStateRetentionStopCanceled,
	SessionStateRetentionStopError,
}

var sessionStateRetentionRunsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_session_state_retention_runs_total",
	Help: "Runs of the session state retention's passes (provider work session heads, device audit state), by table and why the run ended",
}, []string{"table", "stop"})

var sessionStateRetentionExaminedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_session_state_retention_examined_rows_total",
	Help: "Rows the session state retention's passes examined, by table; the deletes count in urnetwork_retention_deleted_rows_total",
}, []string{"table"})

var sessionStateRetentionPassCompletedGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_session_state_retention_pass_completed_timestamp_seconds",
	Help: "When this process last saw a session state retention pass reach the end of its table, by table",
}, []string{"table"})

func init() {
	for _, table := range sessionStateRetentionTables {
		for _, stop := range sessionStateRetentionStops {
			sessionStateRetentionRunsCounter.WithLabelValues(table, stop)
		}
		sessionStateRetentionExaminedCounter.WithLabelValues(table)
	}
	prometheus.MustRegister(sessionStateRetentionRunsCounter, sessionStateRetentionExaminedCounter, sessionStateRetentionPassCompletedGauge)
}

// The retention's runtime brake: the Redis hash
// `{session_state_retention}control`. It stops or slows the deletes without a
// deploy and is read before every batch, so a change applies from the next
// batch. Its fields, all optional:
//   - `mode`: on or off. Off, or any other value, deletes nothing.
//   - `maxHeadsPerSecond`, `maxDeviceStatesPerSecond`: rows the table's pass
//     examines per second, at most the code's MaxRowsPerSecond; zero, or a
//     value that does not parse, stops that table's pass.
//
// Without the hash both passes run at DefaultRowsPerSecond. A pass that cannot
// read the hash deletes nothing more in that run.
const sessionStateRetentionControlKey = "{session_state_retention}control"

const (
	SessionStateRetentionControlFieldMode                     = "mode"
	SessionStateRetentionControlFieldMaxHeadsPerSecond        = "maxHeadsPerSecond"
	SessionStateRetentionControlFieldMaxDeviceStatesPerSecond = "maxDeviceStatesPerSecond"
)

// The effective brake: the code settings as the Redis hash narrowed them.
type SessionStateRetentionControl struct {
	// the hash was present
	Present bool
	// rows each table's pass examines per second; a table whose rate is not
	// positive does not run
	TableRowsPerSecond map[string]float64
}

// The rate of a table's pass, zero when it does not run.
func (self *SessionStateRetentionControl) RowsPerSecond(table string) float64 {
	return self.TableRowsPerSecond[table]
}

// The brake the fields narrow the settings to. Pure: fields is the hash as
// read, empty when the key is absent. A mode other than on stops both passes;
// a rate that does not parse is zero.
func NarrowSessionStateRetentionControl(fields map[string]string, settings *SessionStateRetentionSettings) *SessionStateRetentionControl {
	rate := min(settings.DefaultRowsPerSecond, settings.MaxRowsPerSecond)
	control := &SessionStateRetentionControl{
		Present:            0 < len(fields),
		TableRowsPerSecond: map[string]float64{SessionStateRetentionTableHead: rate, SessionStateRetentionTableDeviceState: rate},
	}
	for field, table := range map[string]string{
		SessionStateRetentionControlFieldMaxHeadsPerSecond:        SessionStateRetentionTableHead,
		SessionStateRetentionControlFieldMaxDeviceStatesPerSecond: SessionStateRetentionTableDeviceState,
	} {
		value, ok := fields[field]
		if !ok {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(parsed) {
			parsed = 0
		}
		control.TableRowsPerSecond[table] = min(settings.MaxRowsPerSecond, parsed)
	}
	if value, ok := fields[SessionStateRetentionControlFieldMode]; ok && strings.TrimSpace(value) != "on" {
		for table := range control.TableRowsPerSecond {
			control.TableRowsPerSecond[table] = 0
		}
	}
	return control
}

// Reads the brake and narrows the settings with it. One bounded Redis read,
// never retried; on an error the caller deletes nothing more.
func GetSessionStateRetentionControl(ctx context.Context, settings *SessionStateRetentionSettings) (control *SessionStateRetentionControl, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.ControlTimeout)
	defer cancel()
	var fields map[string]string
	returnErr = server.RedisWithDeadline(readCtx, func(r server.RedisClient) error {
		var err error
		fields, err = r.HGetAll(readCtx, sessionStateRetentionControlKey).Result()
		return err
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return nil, returnErr
	}
	return NarrowSessionStateRetentionControl(fields, settings), nil
}

// Writes fields into the brake, replacing those fields only. ctx must carry a
// deadline.
func SetSessionStateRetentionControl(ctx context.Context, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	values := make([]any, 0, 2*len(fields))
	for field, value := range fields {
		values = append(values, field, value)
	}
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.HSet(ctx, sessionStateRetentionControlKey, values...).Err()
	}, server.OptRedisNoPreflight())
}

// Deletes the brake, so the code settings apply. ctx must carry a deadline.
func RemoveSessionStateRetentionControl(ctx context.Context) error {
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.Del(ctx, sessionStateRetentionControlKey).Err()
	}, server.OptRedisNoPreflight())
}

// Where each pass resumes.
type SessionStateRetentionCursor struct {
	// the last head the pass examined; nil reads from the first head
	HeadAfterClientId *server.Id `json:"head_after_client_id,omitempty"`
	// when the current or last head pass began
	HeadPassStartTime *time.Time `json:"head_pass_start_time,omitempty"`
	// the last head pass reached the end of the table; the next starts a pass
	// interval after it began
	HeadPassComplete bool `json:"head_pass_complete,omitempty"`
	// the last device state the pass examined; nil reads from the first
	DeviceStateAfterDeviceId *server.Id `json:"device_state_after_device_id,omitempty"`
	// when the current or last device state pass began
	DeviceStatePassStartTime *time.Time `json:"device_state_pass_start_time,omitempty"`
	// the last device state pass reached the end of the table
	DeviceStatePassComplete bool `json:"device_state_pass_complete,omitempty"`
}

// One table's place in its walk.
type sessionStateRetentionPlace struct {
	after         *server.Id
	passStartTime *time.Time
	passComplete  bool
}

// One table's run.
type SessionStateRetentionPassReport struct {
	// why the run ended, one of SessionStateRetentionStop*
	Stop          string
	ExaminedCount int64
	RemovedCount  int64
	// work remains for an immediate rerun
	More bool
}

type SessionStateRetentionReport struct {
	Head        SessionStateRetentionPassReport
	DeviceState SessionStateRetentionPassReport
}

// Work remains for an immediate rerun.
func (self *SessionStateRetentionReport) More() bool {
	return self.Head.More || self.DeviceState.More
}

// One table's walk: a batch examines the rows after the cursor and deletes
// the due ones in one transaction, returning how many it examined and
// removed and the last key it examined, nil when it examined none.
type sessionStateRetentionBatch func(ctx context.Context, after *server.Id, batchCount int) (examinedCount int64, removedCount int64, last *server.Id)

// Removes the due heads and device states, each pass from where its last run
// stopped, until the run budget is spent, both passes side by side.
func RemoveExpiredSessionState(
	ctx context.Context,
	now time.Time,
	cursor SessionStateRetentionCursor,
	settings *SessionStateRetentionSettings,
) (report *SessionStateRetentionReport, next SessionStateRetentionCursor) {
	report = &SessionStateRetentionReport{}
	next = cursor
	deadline := time.Now().Add(settings.RunBudget)
	deviceStateCutoff := now.Add(-settings.DeviceStateRetention).UTC()
	var headPlace, deviceStatePlace sessionStateRetentionPlace
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		report.Head, headPlace = runSessionStateRetentionPass(
			ctx, now, deadline, settings, SessionStateRetentionTableHead, settings.HeadBatchCount, settings.HeadPassInterval,
			sessionStateRetentionPlace{after: cursor.HeadAfterClientId, passStartTime: cursor.HeadPassStartTime, passComplete: cursor.HeadPassComplete},
			func(ctx context.Context, after *server.Id, batchCount int) (int64, int64, *server.Id) {
				return removeIdleProviderWorkSessionHeadBatch(ctx, settings, after, batchCount)
			},
		)
	}()
	go func() {
		defer workers.Done()
		report.DeviceState, deviceStatePlace = runSessionStateRetentionPass(
			ctx, now, deadline, settings, SessionStateRetentionTableDeviceState, settings.DeviceStateBatchCount, settings.DeviceStatePassInterval,
			sessionStateRetentionPlace{after: cursor.DeviceStateAfterDeviceId, passStartTime: cursor.DeviceStatePassStartTime, passComplete: cursor.DeviceStatePassComplete},
			func(ctx context.Context, after *server.Id, batchCount int) (int64, int64, *server.Id) {
				return removeOfflineDeviceStateBatch(ctx, settings, deviceStateCutoff, after, batchCount)
			},
		)
	}()
	workers.Wait()
	next.HeadAfterClientId, next.HeadPassStartTime, next.HeadPassComplete = headPlace.after, headPlace.passStartTime, headPlace.passComplete
	next.DeviceStateAfterDeviceId, next.DeviceStatePassStartTime, next.DeviceStatePassComplete = deviceStatePlace.after, deviceStatePlace.passStartTime, deviceStatePlace.passComplete
	return
}

// One table's run: batches from its place until the pass ends, the budget is
// spent or the brake stops it, each paced to the brake's rate. A failed batch
// ends the run with the place of the last committed one. A complete pass
// starts over once a pass interval has passed since it began; a pass that
// has not reached the end, even one whose first batch failed, continues.
func runSessionStateRetentionPass(
	ctx context.Context,
	now time.Time,
	deadline time.Time,
	settings *SessionStateRetentionSettings,
	table string,
	batchCount int,
	passInterval time.Duration,
	place sessionStateRetentionPlace,
	batch sessionStateRetentionBatch,
) (report SessionStateRetentionPassReport, next sessionStateRetentionPlace) {
	next = place
	defer func() {
		sessionStateRetentionRunsCounter.WithLabelValues(table, report.Stop).Inc()
	}()
	if place.after == nil && (place.passComplete || place.passStartTime == nil) {
		if place.passComplete && place.passStartTime != nil && now.Before(place.passStartTime.Add(passInterval)) {
			report.Stop = SessionStateRetentionStopWaiting
			return
		}
		start := now
		next.passStartTime, next.passComplete = &start, false
	}
	pace := newRetentionPace()
	for {
		select {
		case <-ctx.Done():
			report.Stop = SessionStateRetentionStopCanceled
			report.More = true
			return
		default:
		}
		control, err := GetSessionStateRetentionControl(ctx, settings)
		if err != nil {
			glog.V(1).Infof("[ssr]%s retention control unread, deleting nothing more this run: %s\n", table, err)
			report.Stop = SessionStateRetentionStopBrakeUnread
			return
		}
		rate := control.RowsPerSecond(table)
		if !(0 < rate) {
			report.Stop = SessionStateRetentionStopBrakeOff
			return
		}
		var examinedCount, removedCount int64
		var last *server.Id
		batchErr := server.HandleError(func() {
			examinedCount, removedCount, last = batch(ctx, next.after, batchCount)
		})
		if batchErr != nil {
			glog.Errorf("[ssr]%s retention batch after %v failed; the next run retries it: %s\n", table, next.after, batchErr)
			report.Stop = SessionStateRetentionStopError
			report.More = true
			return
		}
		report.ExaminedCount += examinedCount
		report.RemovedCount += removedCount
		sessionStateRetentionExaminedCounter.WithLabelValues(table).Add(float64(examinedCount))
		if examinedCount < int64(batchCount) {
			// the pass is complete; the next starts at the first row
			next.after, next.passComplete = nil, true
			sessionStateRetentionPassCompletedGauge.WithLabelValues(table).Set(float64(time.Now().Unix()))
			report.Stop = SessionStateRetentionStopPass
			return
		}
		next.after = last
		if !pace.WaitAfter(ctx, int(examinedCount), rate, deadline) || !time.Now().Before(deadline) {
			report.Stop = SessionStateRetentionStopBudget
			report.More = true
			return
		}
	}
}

// One page of heads in client order after the cursor, in one short read
// committed transaction under the head retention's opt-in. A head goes when
// its client has no open connection, no event left in the journal and no
// receipt, and its row is free and unchanged since the page was read: a head
// that advanced belongs to a client that appended meanwhile. The guard checks
// the events and receipts again for each head it admits.
const sessionStateRetentionHeadPageSqlTemplate = `
	SELECT client_id, sequence FROM provider_work_session_head
	{{cursor}}
	ORDER BY client_id
	LIMIT $1
`

// the first page of a pass, and a page after the cursor ($2), each a range of
// the primary key
var sessionStateRetentionHeadFirstPageSql = strings.NewReplacer(
	"{{cursor}}", "",
).Replace(sessionStateRetentionHeadPageSqlTemplate)

var sessionStateRetentionHeadResumedPageSql = strings.NewReplacer(
	"{{cursor}}", "WHERE client_id > $2",
).Replace(sessionStateRetentionHeadPageSqlTemplate)

const sessionStateRetentionHeadDeleteSql = `
	WITH page AS MATERIALIZED (
		SELECT * FROM unnest($1::uuid[], $2::bigint[]) AS page(client_id, sequence)
	), due AS MATERIALIZED (
		SELECT head.client_id
		FROM provider_work_session_head head
		JOIN page ON page.client_id = head.client_id AND page.sequence = head.sequence
		WHERE NOT EXISTS (
			SELECT 1 FROM network_client_connection connection
			WHERE connection.client_id = head.client_id AND connection.connected
		) AND NOT EXISTS (
			SELECT 1 FROM provider_work_session_event event WHERE event.client_id = head.client_id
		) AND NOT EXISTS (
			SELECT 1 FROM provider_work_session_receipt receipt WHERE receipt.client_id = head.client_id
		)
		FOR UPDATE OF head SKIP LOCKED
	)
	DELETE FROM provider_work_session_head head
	USING due
	WHERE head.client_id = due.client_id
`

func removeIdleProviderWorkSessionHeadBatch(ctx context.Context, settings *SessionStateRetentionSettings, after *server.Id, batchCount int) (examinedCount int64, removedCount int64, last *server.Id) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		// an attempt that does not commit removed nothing
		examinedCount, removedCount, last = 0, 0, nil
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, settings.BatchLockTimeout.Milliseconds())))
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL %s = 'on'`, server.ProviderWorkSessionHeadRetentionSetting)))
		clientIds := []server.Id{}
		sequences := []int64{}
		var result server.PgResult
		var err error
		if after == nil {
			result, err = tx.Query(ctx, sessionStateRetentionHeadFirstPageSql, batchCount)
		} else {
			result, err = tx.Query(ctx, sessionStateRetentionHeadResumedPageSql, batchCount, *after)
		}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var sequence int64
				server.Raise(result.Scan(&clientId, &sequence))
				clientIds = append(clientIds, clientId)
				sequences = append(sequences, sequence)
			}
		})
		examinedCount = int64(len(clientIds))
		if examinedCount == 0 {
			return
		}
		lastClientId := clientIds[len(clientIds)-1]
		last = &lastClientId
		tag := server.RaisePgResult(tx.Exec(ctx, sessionStateRetentionHeadDeleteSql, clientIds, sequences))
		removedCount = tag.RowsAffected()
	}, server.TxReadCommitted)
	// counted once the batch committed
	countRetentionDeletedRows(RetentionTableProviderWorkSessionHead, removedCount)
	return
}

// One page of device states in device order after the cursor, deleting the
// offline rows that went offline before cutoff, in one short read committed
// transaction. A row the sweep holds is skipped, and one it set online since
// the page was read is kept.
const sessionStateRetentionDeviceStateSqlTemplate = `
	WITH page AS MATERIALIZED (
		SELECT device_id FROM audit_device_state
		{{cursor}}
		ORDER BY device_id
		LIMIT $1
	), due AS MATERIALIZED (
		SELECT state.device_id
		FROM audit_device_state state
		JOIN page ON page.device_id = state.device_id
		WHERE NOT state.online AND state.event_time < $2
		FOR UPDATE OF state SKIP LOCKED
	), removed AS (
		DELETE FROM audit_device_state state
		USING due
		WHERE state.device_id = due.device_id
		RETURNING 1
	), bound AS (
		SELECT device_id FROM page ORDER BY device_id DESC LIMIT 1
	)
	SELECT (SELECT count(*) FROM page), (SELECT count(*) FROM removed), bound.device_id
	FROM bound
`

// the first page of a pass, and a page after the cursor ($3), each a range of
// the primary key
var sessionStateRetentionDeviceStateFirstSql = strings.NewReplacer(
	"{{cursor}}", "",
).Replace(sessionStateRetentionDeviceStateSqlTemplate)

var sessionStateRetentionDeviceStateResumedSql = strings.NewReplacer(
	"{{cursor}}", "WHERE device_id > $3",
).Replace(sessionStateRetentionDeviceStateSqlTemplate)

func removeOfflineDeviceStateBatch(ctx context.Context, settings *SessionStateRetentionSettings, cutoff time.Time, after *server.Id, batchCount int) (examinedCount int64, removedCount int64, last *server.Id) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		// an attempt that does not commit removed nothing
		examinedCount, removedCount, last = 0, 0, nil
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, settings.BatchLockTimeout.Milliseconds())))
		var row pgx.Row
		if after == nil {
			row = tx.QueryRow(ctx, sessionStateRetentionDeviceStateFirstSql, batchCount, cutoff)
		} else {
			row = tx.QueryRow(ctx, sessionStateRetentionDeviceStateResumedSql, batchCount, cutoff, *after)
		}
		var bound server.Id
		err := row.Scan(&examinedCount, &removedCount, &bound)
		if errors.Is(err, pgx.ErrNoRows) {
			// an empty page: the pass is complete
			return
		}
		server.Raise(err)
		last = &bound
	}, server.TxReadCommitted)
	countRetentionDeletedRows(RetentionTableAuditDeviceState, removedCount)
	return
}
