package server

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
)

// The close evidence row retention (RemoveExpiredCloseEvidence): its brake and
// pace, its batch, and where each pass starts.
//
// # The brake
//
// The deletes follow the contract horizon reaper's horizon while the reaper's
// mode is on (db_contract_close_evidence_horizon.go), and they have a brake of
// their own, so they slow or stop without touching the reaper: the Redis hash
// `{close_evidence_retention}control`, read before every batch, so a change
// applies from the next batch. Its fields, all optional:
//   - `mode`: on or off. Off, or any other value, deletes nothing.
//   - `maxRowsPerSecond`: rows deleted per second at most, at most the code's
//     MaxRowsPerSecond; zero, or a value that does not parse, is off.
//
// Without the hash the deletes run at DefaultRowsPerSecond. A run that cannot
// read the hash deletes nothing more in that run. `bringyourctl db
// close-evidence-retention` shows the brake and sets it. The brake is how an
// operator stops the deletes. Restoring the evidence guard's 91-day body
// stops the deletes below 91 days as well (closeEvidenceGuardFloor), but the
// horizon guard migration's monitor contract then pages
// migration-schema-drift.
//
// # The batch
//
// A batch reads the next BatchSize rows in the key order of
// contract_close_original_census, (contract_id, client_id, report_id), below
// the first contract id the retention keeps, and deletes by ctid those
// accepted longer ago than the retention whose contract is resolved or gone.
// The read and the delete are one statement under the batch's own statement
// and lock timeouts, so a slow batch never holds the transaction horizon or
// waits in a lock queue.
//
// # Where a pass starts
//
// A pass walks the removal targets (closeEvidenceRemovalTargets) and ends when
// the last target's batch comes up short. A run resumes the pass in progress.
// A new pass starts at the low water the last pass left: the lowest contract
// id of a row it kept because the row was accepted inside the retention, or,
// with none, the first contract id the last pass's retention kept. Below the
// low water every row was deleted except the evidence of contracts unresolved
// at the time, and what was written there since; the day's first pass (by
// UTC day) starts from the first key and takes those. So a caught-up pass
// reads the rows that came due since the last one, not the deleted range
// again.
const closeEvidenceRetentionControlKey = "{close_evidence_retention}control"

const (
	CloseEvidenceRetentionControlFieldMode             = "mode"
	CloseEvidenceRetentionControlFieldMaxRowsPerSecond = "maxRowsPerSecond"
)

// Tunables of the row retention.
type CloseEvidenceRetentionSettings struct {
	// Rows deleted per second while the brake sets no rate: a cautious start,
	// so the 35-day horizon's first deletes drain under watch. The deletes
	// take a third of the contract task's run, and a run that leaves work
	// continues a minute later, so a rate holds for about three quarters of a
	// day that has a backlog: 1,000 a second is about 65M rows a day. The
	// rows that come due at 35 days are about 18-24M a day from about
	// 2026-11-10, about 60M a day by late November, and 100M a day and more
	// from about 2026-12-06. The brake raises it.
	DefaultRowsPerSecond float64
	// The highest rate the brake can set.
	MaxRowsPerSecond float64
	// Bounds each brake read.
	ControlTimeout time.Duration
	// Rows one batch reads; it deletes the expired ones among them.
	BatchSize int
	// Each batch's statement and lock bounds, the contract horizon reaper's.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

func DefaultCloseEvidenceRetentionSettings() *CloseEvidenceRetentionSettings {
	return &CloseEvidenceRetentionSettings{
		DefaultRowsPerSecond: 1000,
		MaxRowsPerSecond:     5000,
		ControlTimeout:       2 * time.Second,
		BatchSize:            5000,
		StatementTimeout:     30 * time.Second,
		LockTimeout:          2 * time.Second,
	}
}

// The effective brake: the code settings as the Redis hash narrowed them.
type CloseEvidenceRetentionControl struct {
	// the hash was present
	Present bool
	// deletes run
	On bool
	// rows deleted per second at most, while On
	MaxRowsPerSecond float64
}

// The brake the fields narrow the settings to. Pure: fields is the hash as
// read, empty when the key is absent. A field narrows as far as it can: a mode
// other than on is off, and a rate that does not parse is zero, off.
func NarrowCloseEvidenceRetentionControl(fields map[string]string, settings *CloseEvidenceRetentionSettings) *CloseEvidenceRetentionControl {
	control := &CloseEvidenceRetentionControl{
		Present:          0 < len(fields),
		On:               true,
		MaxRowsPerSecond: min(settings.DefaultRowsPerSecond, settings.MaxRowsPerSecond),
	}
	if value, ok := fields[CloseEvidenceRetentionControlFieldMode]; ok {
		control.On = strings.TrimSpace(value) == "on"
	}
	if value, ok := fields[CloseEvidenceRetentionControlFieldMaxRowsPerSecond]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(rate) {
			control.MaxRowsPerSecond = min(settings.MaxRowsPerSecond, rate)
		} else {
			control.MaxRowsPerSecond = 0
		}
	}
	if !(0 < control.MaxRowsPerSecond) {
		control.On = false
	}
	return control
}

// The brake's fields as stored, empty when the hash is absent. One bounded
// Redis read, never retried.
func GetCloseEvidenceRetentionControlFields(ctx context.Context, settings *CloseEvidenceRetentionSettings) (fields map[string]string, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.ControlTimeout)
	defer cancel()
	returnErr = RedisWithDeadline(readCtx, func(r RedisClient) error {
		var err error
		fields, err = r.HGetAll(readCtx, closeEvidenceRetentionControlKey).Result()
		return err
	}, OptRedisNoPreflight())
	return
}

// Reads the brake and narrows the settings with it. On an error the caller
// deletes nothing more.
func GetCloseEvidenceRetentionControl(ctx context.Context, settings *CloseEvidenceRetentionSettings) (*CloseEvidenceRetentionControl, error) {
	fields, err := GetCloseEvidenceRetentionControlFields(ctx, settings)
	if err != nil {
		return nil, err
	}
	return NarrowCloseEvidenceRetentionControl(fields, settings), nil
}

// Sets brake fields, keeping the others. For operator tools and tests. ctx
// must carry a deadline.
func SetCloseEvidenceRetentionControl(ctx context.Context, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	values := make([]any, 0, 2*len(fields))
	for field, value := range fields {
		values = append(values, field, value)
	}
	return RedisWithDeadline(ctx, func(r RedisClient) error {
		return r.HSet(ctx, closeEvidenceRetentionControlKey, values...).Err()
	}, OptRedisNoPreflight())
}

// Deletes the brake, so the code settings apply. ctx must carry a deadline.
func RemoveCloseEvidenceRetentionControl(ctx context.Context) error {
	return RedisWithDeadline(ctx, func(r RedisClient) error {
		return r.Del(ctx, closeEvidenceRetentionControlKey).Err()
	}, OptRedisNoPreflight())
}

// Paces the deletes to a ceiling of rows per second, so a backlog that comes
// due at once drains at a rate the primary absorbs. A batch that removed n
// rows at r a second starts the next n/r seconds after it began, at the
// soonest. A slow batch earns no credit, so the loop never bursts to catch up.
type closeEvidenceRetentionPace struct {
	// when the batch now ending began
	batchStart time.Time
}

func newCloseEvidenceRetentionPace() *closeEvidenceRetentionPace {
	return &closeEvidenceRetentionPace{batchStart: time.Now()}
}

// After a batch that removed count rows, waits until the loop is back under
// rate rows a second. A rate of zero or less never waits. False when ctx ends
// first or the wait would reach deadline: the caller then stops with work
// left, for its next run.
func (self *closeEvidenceRetentionPace) WaitAfter(ctx context.Context, count int64, rate float64, deadline time.Time) bool {
	defer func() {
		self.batchStart = time.Now()
	}()
	if !(0 < rate) {
		return true
	}
	due := self.batchStart.Add(time.Duration(float64(count) / rate * float64(time.Second)))
	wait := time.Until(due)
	if wait <= 0 {
		return true
	}
	if !due.Before(deadline) {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
		return true
	}
}

// Why a run's deletes stopped.
const (
	// the pass is complete
	CloseEvidenceRemovalStopDone = "done"
	// the run's deadline, or its pace, ends it with work left
	CloseEvidenceRemovalStopBudget = "budget"
	// the brake is off, or sets no rate
	CloseEvidenceRemovalStopBrakeOff = "brake_off"
	// the brake could not be read
	CloseEvidenceRemovalStopBrakeUnread = "brake_unread"
	CloseEvidenceRemovalStopCanceled    = "canceled"
	// the targets, the retention or a batch failed; the next run retries
	CloseEvidenceRemovalStopError = "error"
)

// One run of the row retention.
type CloseEvidenceRemovalReport struct {
	Removed int64
	// the retention the run applied: ctx's horizon, never less than the
	// installed guard admits (closeEvidenceRetentionIn); zero when the run
	// stopped before reading it
	Retention time.Duration
	Stop      string
	// the run started a pass from the first key
	FullPass bool
}

// Where the row retention resumes, kept in the contract task's arguments.
type CloseEvidenceRemovalCursor struct {
	// The pass in progress, when Leaf or ContractId is set: the target it was
	// deleting from (the table itself while it is plain) and the last key it
	// read there. A cursor from before 2026-11-02 holds only a contract id,
	// which resumes at that contract's first row.
	Leaf       string `json:"leaf,omitempty"`
	ContractId *Id    `json:"contract_id,omitempty"`
	ClientId   *Id    `json:"client_id,omitempty"`
	ReportId   *Id    `json:"report_id,omitempty"`
	// the contract id every target of the pass in progress starts at; nil is
	// the first key
	PassFloor *Id `json:"pass_floor,omitempty"`
	// the lowest contract id of a row the pass in progress kept because it
	// was accepted inside the retention
	PassLowWater *Id `json:"pass_low_water,omitempty"`
	// where the next pass starts, unless it is the day's first; nil starts it
	// from the first key
	LowWater *Id `json:"low_water,omitempty"`
	// the UTC day (YYYY-MM-DD) of the last pass that started from the first key
	FullPassDay string `json:"full_pass_day,omitempty"`
}

// A pass is in progress.
func (self CloseEvidenceRemovalCursor) InProgress() bool {
	return self.Leaf != "" || self.ContractId != nil
}

// A row's key in contract_close_original_census order.
type closeEvidenceKey struct {
	contractId Id
	clientId   Id
	reportId   Id
}

// The key just before every row of contractId: a batch reads after it.
func closeEvidenceKeyBefore(contractId *Id) closeEvidenceKey {
	if contractId == nil {
		return closeEvidenceKey{}
	}
	return closeEvidenceKey{contractId: *contractId}
}

// Where a run starts in targets: the target's index and the key the run reads
// after there, with the cursor of the pass it runs. A pass in progress resumes,
// at its target's next key; when its target is no longer one (dropped, or no
// longer kept by a straggler), at the first target from the pass's floor.
// Otherwise a new pass starts at the first target: from the low water, or
// from the first key when there is none or the day's first pass is due.
func (self CloseEvidenceRemovalCursor) startIn(targets []string, now time.Time) (start int, after closeEvidenceKey, pass CloseEvidenceRemovalCursor) {
	if self.InProgress() {
		pass = self
		leaf := self.Leaf
		if leaf == "" {
			leaf = closeEvidenceTable
		}
		start = slices.Index(targets, leaf)
		if start < 0 {
			return 0, closeEvidenceKeyBefore(self.PassFloor), pass
		}
		after = closeEvidenceKeyBefore(self.ContractId)
		if self.ClientId != nil && self.ReportId != nil {
			after.clientId = *self.ClientId
			after.reportId = *self.ReportId
		}
		return
	}
	day := now.UTC().Format(time.DateOnly)
	pass = CloseEvidenceRemovalCursor{FullPassDay: self.FullPassDay}
	if self.LowWater == nil || self.FullPassDay != day {
		pass.FullPassDay = day
		return 0, closeEvidenceKey{}, pass
	}
	lowWater := *self.LowWater
	pass.PassFloor = &lowWater
	return 0, closeEvidenceKeyBefore(&lowWater), pass
}

var closeEvidenceRetentionRemovedCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Name: "urnetwork_close_evidence_retention_removed_rows_total",
		Help: "Close evidence rows the row retention deleted",
	},
)

var closeEvidenceRetentionSecondsGauge = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name: "urnetwork_close_evidence_retention_seconds",
		Help: "The close evidence retention the last row retention run applied: the contract horizon while the reaper's mode is on, never less than the installed guard admits, 35 to 91 days",
	},
)

func init() {
	prometheus.MustRegister(closeEvidenceRetentionRemovedCounter, closeEvidenceRetentionSecondsGauge)
}

// Deletes close evidence accepted longer ago than its retention, ctx's contract
// horizon bounded by the installed guard (closeEvidenceRetentionIn), whose
// contract is resolved or gone. A contract the horizon reaper deleted is gone,
// so its evidence goes once it is as old; so does the evidence of a resolved
// contract the reaper keeps, since the reaper's keep rules do not apply here.
// Only a transaction that opts in may delete evidence, and the evidence guard
// admits only rows older than CloseEvidenceRetentionFloor, the shortest
// retention. Once the evidence is partitioned the pass deletes from the leaves
// the drop cannot take (closeEvidenceRemovalTargets): the DEFAULT partition,
// the expired weeks a straggler keeps, and the legacy partition, one leaf
// after another; a week the drop will take, and a week not yet expired, are
// left alone. The brake, the pace, the batch and where a pass starts are
// described above.
func RemoveExpiredCloseEvidence(ctx context.Context, now time.Time, cursor CloseEvidenceRemovalCursor, deadline time.Time) (report CloseEvidenceRemovalReport, next CloseEvidenceRemovalCursor, more bool) {
	return removeExpiredCloseEvidenceWith(ctx, now, cursor, deadline, DefaultCloseEvidenceRetentionSettings())
}

// RemoveExpiredCloseEvidence with settings in place of the code's.
func removeExpiredCloseEvidenceWith(ctx context.Context, now time.Time, cursor CloseEvidenceRemovalCursor, deadline time.Time, settings *CloseEvidenceRetentionSettings) (report CloseEvidenceRemovalReport, next CloseEvidenceRemovalCursor, more bool) {
	removal := &closeEvidenceRemoval{
		settings: settings,
		pace:     newCloseEvidenceRetentionPace(),
		deadline: deadline,
	}
	return removal.run(ctx, now, cursor)
}

// One run of the row retention. Not safe for concurrent use.
type closeEvidenceRemoval struct {
	settings *CloseEvidenceRetentionSettings
	pace     *closeEvidenceRetentionPace
	deadline time.Time
}

func (self *closeEvidenceRemoval) run(ctx context.Context, now time.Time, cursor CloseEvidenceRemovalCursor) (report CloseEvidenceRemovalReport, next CloseEvidenceRemovalCursor, more bool) {
	next = cursor
	targets, err := closeEvidenceRemovalTargets(ctx, now)
	if err != nil {
		glog.Errorf("[cp]close evidence retention: %s\n", err)
		report.Stop = CloseEvidenceRemovalStopError
		return
	}
	err = partitionRegistryRun(func() {
		MaintenanceDb(ctx, func(conn PgConn) {
			report.Retention = closeEvidenceRetentionIn(ctx, conn)
		})
	})
	if err != nil {
		glog.Errorf("[cp]close evidence retention: %s\n", err)
		report.Stop = CloseEvidenceRemovalStopError
		return
	}
	closeEvidenceRetentionSecondsGauge.Set(report.Retention.Seconds())
	upper := ContractIdFloor(now.Add(-report.Retention))

	start, after, pass := cursor.startIn(targets, now)
	report.FullPass = !cursor.InProgress() && pass.PassFloor == nil
	for index := start; index < len(targets); index += 1 {
		if index != start {
			after = closeEvidenceKeyBefore(pass.PassFloor)
		}
		leafDone := false
		for !leafDone {
			select {
			case <-ctx.Done():
				report.Stop = CloseEvidenceRemovalStopCanceled
				return report, pass.at(targets[index], after), true
			default:
			}
			// a run past its deadline starts no batch
			if !time.Now().Before(self.deadline) {
				report.Stop = CloseEvidenceRemovalStopBudget
				return report, pass.at(targets[index], after), true
			}
			// the brake, before every batch
			brake, err := GetCloseEvidenceRetentionControl(ctx, self.settings)
			if err != nil {
				glog.V(1).Infof("[cp]close evidence retention control unread, deleting nothing more this run: %s\n", err)
				report.Stop = CloseEvidenceRemovalStopBrakeUnread
				return report, pass.at(targets[index], after), false
			}
			if !brake.On {
				report.Stop = CloseEvidenceRemovalStopBrakeOff
				return report, pass.at(targets[index], after), false
			}
			batch, err := removeCloseEvidenceBatch(ctx, targets[index], after, upper, report.Retention, self.settings)
			if err != nil {
				glog.Errorf("[cp]close evidence retention of %s: %s\n", targets[index], err)
				report.Stop = CloseEvidenceRemovalStopError
				return report, pass.at(targets[index], after), false
			}
			report.Removed += batch.removed
			closeEvidenceRetentionRemovedCounter.Add(float64(batch.removed))
			if batch.lowWater != nil && (pass.PassLowWater == nil || batch.lowWater.Less(*pass.PassLowWater)) {
				lowWater := *batch.lowWater
				pass.PassLowWater = &lowWater
			}
			if batch.scanned < int64(self.settings.BatchSize) {
				leafDone = true
				continue
			}
			after = batch.last
			if !self.pace.WaitAfter(ctx, batch.removed, brake.MaxRowsPerSecond, self.deadline) || !time.Now().Before(self.deadline) {
				report.Stop = CloseEvidenceRemovalStopBudget
				return report, pass.at(targets[index], after), true
			}
		}
	}
	report.Stop = CloseEvidenceRemovalStopDone
	return report, pass.complete(upper), false
}

// The cursor of this pass, in progress at after in leaf.
func (self CloseEvidenceRemovalCursor) at(leaf string, after closeEvidenceKey) CloseEvidenceRemovalCursor {
	contractId, clientId, reportId := after.contractId, after.clientId, after.reportId
	self.Leaf = leaf
	self.ContractId = &contractId
	self.ClientId = &clientId
	self.ReportId = &reportId
	return self
}

// The cursor after this pass completed below upper, the first contract id its
// retention kept: the next pass starts at the pass's low water, or at upper.
func (self CloseEvidenceRemovalCursor) complete(upper Id) CloseEvidenceRemovalCursor {
	lowWater := upper
	if self.PassLowWater != nil && self.PassLowWater.Less(upper) {
		lowWater = *self.PassLowWater
	}
	return CloseEvidenceRemovalCursor{LowWater: &lowWater, FullPassDay: self.FullPassDay}
}

// One batch's outcome.
type closeEvidenceBatch struct {
	removed int64
	scanned int64
	// the last key read, when scanned is positive
	last closeEvidenceKey
	// the lowest contract id of a row read that was accepted inside the
	// retention; nil when none was
	lowWater *Id
}

// One batch: reads the next rows of leaf after the key, below upper, and
// deletes the expired ones by ctid, in one statement. $1-$3 the key, $4 upper,
// $5 the rows to read, $6 the retention in seconds. Both name ONLY the target.
// A run reads its targets once, so a cutover during the run can leave a batch
// naming the partitioned parent, where a ctid is not unique: a plain scan
// would read every leaf, and the delete would take the row at each ctid in
// every leaf, whatever its age or contract. ONLY reads no row of a parent, so
// that target ends and the next run reads the leaves; on a leaf or the plain
// table it changes nothing.
const closeEvidenceRetentionBatchSql = `
	WITH scanned AS MATERIALIZED (
		SELECT
			evidence.ctid AS row_ctid,
			evidence.contract_id,
			evidence.client_id,
			evidence.report_id,
			evidence.accepted_at < (now() AT TIME ZONE 'UTC') - make_interval(secs => $6) AS aged
		FROM ONLY public.%[1]s evidence
		WHERE
			(evidence.contract_id, evidence.client_id, evidence.report_id) > ($1::uuid, $2::uuid, $3::uuid) AND
			evidence.contract_id < $4::uuid
		ORDER BY evidence.contract_id, evidence.client_id, evidence.report_id
		LIMIT $5
	), classified AS MATERIALIZED (
		SELECT
			scanned.row_ctid,
			scanned.contract_id,
			scanned.aged,
			EXISTS (
				SELECT 1 FROM transfer_contract
				WHERE transfer_contract.contract_id = scanned.contract_id AND transfer_contract.outcome IS NULL
			) AS unresolved
		FROM scanned
	), removed AS (
		DELETE FROM ONLY public.%[1]s evidence
		WHERE evidence.ctid = ANY (ARRAY(SELECT classified.row_ctid FROM classified WHERE classified.aged AND NOT classified.unresolved))
		RETURNING 1
	)
	SELECT
		(SELECT count(*) FROM removed),
		(SELECT count(*) FROM scanned),
		last.contract_id,
		last.client_id,
		last.report_id,
		(SELECT classified.contract_id FROM classified WHERE NOT classified.aged ORDER BY classified.contract_id LIMIT 1)
	FROM (SELECT 1) AS one
	LEFT JOIN LATERAL (
		SELECT scanned.contract_id, scanned.client_id, scanned.report_id
		FROM scanned
		ORDER BY scanned.contract_id DESC, scanned.client_id DESC, scanned.report_id DESC
		LIMIT 1
	) AS last ON true
`

func removeCloseEvidenceBatch(ctx context.Context, leaf string, after closeEvidenceKey, upper Id, retention time.Duration, settings *CloseEvidenceRetentionSettings) (batch closeEvidenceBatch, err error) {
	err = contractPartitionTx(ctx, func(tx PgTx) {
		batch = closeEvidenceBatch{}
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", settings.StatementTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", settings.LockTimeout.Milliseconds())))
		RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.close_evidence_retention = 'on'`))
		var lastContractId, lastClientId, lastReportId, lowWater *Id
		Raise(tx.QueryRow(
			ctx,
			fmt.Sprintf(closeEvidenceRetentionBatchSql, quoteIdentifier(leaf)),
			after.contractId,
			after.clientId,
			after.reportId,
			upper,
			settings.BatchSize,
			float64(closeEvidenceRetentionSeconds(retention)),
		).Scan(
			&batch.removed,
			&batch.scanned,
			&lastContractId,
			&lastClientId,
			&lastReportId,
			&lowWater,
		))
		if lastContractId != nil && lastClientId != nil && lastReportId != nil {
			batch.last = closeEvidenceKey{contractId: *lastContractId, clientId: *lastClientId, reportId: *lastReportId}
		}
		batch.lowWater = lowWater
	})
	return
}
