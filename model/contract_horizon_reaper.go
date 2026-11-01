// Contract row retention on the contract tables as they are today,
// unpartitioned. The horizon is the owner's R (CONTRACT-PARTITION.md
// "Retention": 13 weeks, server.ContractRetention) unless the runtime control
// shortens it, to no less than ContractHorizonFloor (45 days). The floor is
// the reaper's own: nothing else that server.ContractRetention sets moves.
// Once the partition cutover lands, weekly drops retire whole weeks past R;
// until then this reaper deletes the same contracts row by row. A contract is
// deleted only when every one of these holds, at any horizon:
//   - its id time and its close time are both older than the horizon;
//   - it is resolved (outcome IS NOT NULL), so it is closed and no dispute is
//     open: the usage guard makes a set outcome and close time immutable;
//   - every escrow of it is settled;
//   - every sweep of it is discharged: paid by a payment that completed more
//     than CompletedContractExpiration ago and whose retention stamping is
//     done, or unpaid on a contract minted after the earning cutoff, whose
//     provider earns through the subnet pool from the archived usage. A sweep
//     with no payment on an older contract, or one whose payment is canceled,
//     pending or held (server.ContractPaymentRetentionHoldSql), is a legacy
//     obligation the payout planner can still select, or the evidence a
//     recovery reads, and keeps its contract. So does a sweep staged in a
//     payment plan whose assignment is pending.
//
// That last rule is stricter than the partition drop's, which keeps only
// contracts held by a pending payment. It means the reaper never deletes a
// sweep the planner can select, so it never races a plan; contracts kept by it
// age out through the payment completion path or the straggler horizon.
//
// Each contract goes in one transaction with its close reports, closes,
// escrows, sweeps and extender parties. The usage archive trigger copies a
// credit-bearing contract's proof into st_provider_usage_archive in the same
// statement, so the subnet usage readers see it either live or archived, never
// both and never neither. Close evidence keeps its own 91-day retention
// (server.RemoveExpiredCloseEvidence), which takes evidence whose contract is
// gone, so below 91 days a deleted contract's evidence outlives it until then.
//
// A pass walks transfer_contract's primary key from a cursor toward the
// horizon. Each batch takes the next ids from an index-only range of that key,
// then reads and deletes the rows of those ids in every table with bitmap
// scans: each dependent table's key leads with the contract id, and each heap
// page is read once, in block order, with prefetch, rather than one random
// read per row. A batch is one short read committed transaction over at most
// BatchSize contracts, halved when it runs long, and the pass is paced to a
// contracts-per-second rate. The contract rows are locked before their
// dependents are deleted (closes and close reports are written only under
// that lock), with SKIP LOCKED, so the reaper never queues behind a writer.
// The sweeps are then locked, and the payment and plan checks read after both
// locks, so no payout writer can move a sweep between the check and the
// delete.
//
// The run returns its cursor for the next run, and a failed batch hands the
// last committed position to the retry, so a stopped or failed run resumes at
// its last committed batch. A batch is idempotent: rereading a reaped range
// finds nothing to delete. A pass that reaches the horizon starts over from
// the oldest contract once it is RescanInterval old, to pick up contracts that
// were protected when it passed them.
//
// The runtime control, the Redis hash `{contract_horizon}control`, turns the
// reaper on, off or to a dry run, slows it and shortens its horizon, without a
// deploy. It never passes the code's bounds, and it is read before every batch:
// setting the mode off stops a running pass within one batch, and a new horizon
// applies from the next batch. A run that cannot read the control stops, as if
// the mode were off.
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

// The reaper's mode, ordered from narrowest to widest.
type ContractHorizonMode int

const (
	// Reads and deletes nothing.
	ContractHorizonModeOff ContractHorizonMode = 0
	// Reads and classifies each batch read only, deletes nothing, and never
	// advances the stored cursor.
	ContractHorizonModeDryRun ContractHorizonMode = 1
	// Deletes.
	ContractHorizonModeOn ContractHorizonMode = 2
)

// off, dry_run or on. False for any other value.
func ParseContractHorizonMode(value string) (ContractHorizonMode, bool) {
	switch strings.TrimSpace(value) {
	case "off":
		return ContractHorizonModeOff, true
	case "dry_run":
		return ContractHorizonModeDryRun, true
	case "on":
		return ContractHorizonModeOn, true
	default:
		return ContractHorizonModeOff, false
	}
}

// The mode's control value. Any value outside the modes prints off.
func (self ContractHorizonMode) String() string {
	switch self {
	case ContractHorizonModeDryRun:
		return "dry_run"
	case ContractHorizonModeOn:
		return "on"
	default:
		return "off"
	}
}

// The shortest horizon the reaper applies, whatever its settings or control
// say (owner, 2026-11-01). It is the reaper's: server.ContractRetention, which
// the partition drop, close evidence retention and the sn-reconcile floor
// share, stays 91 days. Key history retention follows the reaper's horizon
// while its mode is on (RemoveGoneStClientKeyHistory), over the key history
// guard's own 30-day floor (server.ClientKeyHistoryRetentionFloor), and keeps
// 91 days in a dry run. No reader of the deleted rows reaches
// past 45 days; the furthest, operator block points, reads a block's unpaid
// sweeps only while its reliability is retained, a 7-day block plus
// ClientReliabilityRetention. What keeps a contract the payout planner or a
// recovery can still read is the reaper's row checks, never its horizon.
//
// Below 91 days users see less history: the 90-day provider stats views
// undercount the days past the horizon by the contracts deleted there, and
// the unpaid bytes of /transfer/stats fall as the unpaid sweeps of contracts
// minted after the earning cutoff, which the subnet pool pays, pass it.
const ContractHorizonFloor = 45 * 24 * time.Hour

// Code settings, which bound the runtime control.
type ContractHorizonSettings struct {
	// False forces the mode off, whatever the control says.
	Enable bool
	// The mode while the control sets none. Off, so a deploy deletes nothing
	// until an operator sets the control.
	DefaultMode ContractHorizonMode
	// How long past its creation and its close a contract is kept while the
	// control sets no horizon, and the longest horizon the control can set.
	// Values below ContractHorizonFloor count as ContractHorizonFloor.
	Horizon time.Duration
	// Contracts read per batch transaction, at most. The control can lower it.
	BatchSize int
	// The adaptive batch size never goes below this.
	MinBatchSize int
	// A batch slower than this halves the next one. One faster than a quarter
	// of it grows the next by half, up to the batch size.
	TargetBatchDuration time.Duration
	// Contracts read per second while the control sets no rate.
	DefaultMaxRowsPerSecond float64
	// The control's rate never exceeds this.
	MaxRowsPerSecond float64
	// Wall clock for one run, pacing included. Well under the task's MaxTime,
	// so a run always returns its cursor.
	RunBudget time.Duration
	// Bounds each statement of a batch.
	StatementTimeout time.Duration
	// Bounds each lock wait of a batch. The contract and sweep locks skip
	// locked rows; this bounds the remaining waits, such as a child row that
	// a writer holds.
	LockTimeout time.Duration
	// Bounds the control read.
	ControlTimeout time.Duration
	// A pass that reached the horizon starts over from the oldest contract
	// once it began this long ago. A rescan rereads every contract still kept
	// below the horizon at the same rate, and most kept contracts leave by the
	// payment completion path (reap_time) or the straggler horizon instead, so
	// it is long.
	RescanInterval time.Duration
	// Gap between runs while a pass has work left.
	ResumeInterval time.Duration
	// Gap after a run that reached the horizon, or that found the reaper off
	// or its control unreadable.
	IdleInterval time.Duration
	// The task's claim bound.
	TaskMaxTime time.Duration
}

func DefaultContractHorizonSettings() *ContractHorizonSettings {
	return &ContractHorizonSettings{
		Enable:      true,
		DefaultMode: ContractHorizonModeOff,
		Horizon:     server.ContractRetention,
		BatchSize:   2000,
		// a 2000-row batch halved four times
		MinBatchSize:        125,
		TargetBatchDuration: 10 * time.Second,
		// about 17M contracts a day: a cautious start while the effect on
		// edge-2's I/O, WAL and vacuum is measured
		DefaultMaxRowsPerSecond: 200,
		// about 173M contracts a day, over twice the newest days' creation
		MaxRowsPerSecond: 2000,
		RunBudget:        10 * time.Minute,
		StatementTimeout: 30 * time.Second,
		LockTimeout:      2 * time.Second,
		ControlTimeout:   2 * time.Second,
		RescanInterval:   30 * 24 * time.Hour,
		ResumeInterval:   15 * time.Second,
		IdleInterval:     5 * time.Minute,
		TaskMaxTime:      30 * time.Minute,
	}
}

// The code's horizon: never below the reaper's floor.
func (self *ContractHorizonSettings) horizon() time.Duration {
	return max(self.Horizon, ContractHorizonFloor)
}

// The control hash. A singleton key on the `{contract_horizon}` slot.
const contractHorizonControlKey = "{contract_horizon}control"

// The control's fields, all optional:
//   - `mode`: off, dry_run or on;
//   - `maxRowsPerSecond`: contracts read per second, at most the code's;
//   - `batchSize`: contracts per batch, between the code's minimum and size;
//   - `horizonDays`: the horizon in whole days, between ContractHorizonFloor
//     (45) and the code's horizon (91); a value past either end counts as that
//     end. Below 91 days users see less history in the provider stats and
//     /transfer/stats (ContractHorizonFloor).
//
// A field that does not parse narrows as far as it can: mode off, rate zero
// (which is off), the minimum batch size, the code's horizon.
const (
	ContractHorizonControlFieldMode             = "mode"
	ContractHorizonControlFieldMaxRowsPerSecond = "maxRowsPerSecond"
	ContractHorizonControlFieldBatchSize        = "batchSize"
	ContractHorizonControlFieldHorizonDays      = "horizonDays"
)

// The effective control: the code settings as the Redis control narrowed them.
type ContractHorizonControl struct {
	// The control key was present.
	Present          bool
	Mode             ContractHorizonMode
	MaxRowsPerSecond float64
	BatchSize        int
	// How long past its creation and its close a contract is kept.
	Horizon time.Duration
}

// The control the fields narrow the settings to. Pure: fields is the control
// hash as read, empty when the key is absent.
func NarrowContractHorizonControl(fields map[string]string, settings *ContractHorizonSettings) *ContractHorizonControl {
	control := &ContractHorizonControl{
		Present:          0 < len(fields),
		Mode:             settings.DefaultMode,
		MaxRowsPerSecond: min(settings.DefaultMaxRowsPerSecond, settings.MaxRowsPerSecond),
		BatchSize:        max(settings.MinBatchSize, settings.BatchSize),
		Horizon:          settings.horizon(),
	}
	if value, ok := fields[ContractHorizonControlFieldMode]; ok {
		// a value that does not parse is off
		control.Mode, _ = ParseContractHorizonMode(value)
	}
	if value, ok := fields[ContractHorizonControlFieldMaxRowsPerSecond]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(rate) {
			control.MaxRowsPerSecond = min(settings.MaxRowsPerSecond, rate)
		} else {
			control.MaxRowsPerSecond = 0
		}
	}
	if value, ok := fields[ContractHorizonControlFieldBatchSize]; ok {
		batchSize, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			batchSize = settings.MinBatchSize
		}
		control.BatchSize = max(settings.MinBatchSize, min(control.BatchSize, batchSize))
	}
	if value, ok := fields[ContractHorizonControlFieldHorizonDays]; ok {
		// a value that does not parse keeps the code's horizon, the longest
		if days, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			control.Horizon = contractHorizonFromDays(days, control.Horizon)
		}
	}
	if !settings.Enable || !(0 < control.MaxRowsPerSecond) {
		control.Mode = ContractHorizonModeOff
	}
	return control
}

// days as a horizon between ContractHorizonFloor and ceiling, the code's
// horizon. Clamped while still in days, so that no count of days overflows
// into a short duration. Pure.
func contractHorizonFromDays(days int64, ceiling time.Duration) time.Duration {
	const day = 24 * time.Hour
	switch {
	case days <= int64(ContractHorizonFloor/day):
		return ContractHorizonFloor
	case int64(ceiling/day) < days:
		return ceiling
	default:
		return time.Duration(days) * day
	}
}

// Reads the control and narrows the settings with it. One bounded Redis read,
// never retried. On an error the caller acts as if the mode were off.
func GetContractHorizonControl(ctx context.Context, settings *ContractHorizonSettings) (control *ContractHorizonControl, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.ControlTimeout)
	defer cancel()
	var fields map[string]string
	returnErr = server.RedisWithDeadline(readCtx, func(r server.RedisClient) error {
		var err error
		fields, err = r.HGetAll(readCtx, contractHorizonControlKey).Result()
		return err
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return nil, returnErr
	}
	return NarrowContractHorizonControl(fields, settings), nil
}

// Sets control fields, keeping the others. For operator tools and tests.
// ctx must carry a deadline.
func SetContractHorizonControl(ctx context.Context, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	values := make([]any, 0, 2*len(fields))
	for field, value := range fields {
		values = append(values, field, value)
	}
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.HSet(ctx, contractHorizonControlKey, values...).Err()
	}, server.OptRedisNoPreflight())
}

// Deletes the control, so the code settings apply. ctx must carry a deadline.
func RemoveContractHorizonControl(ctx context.Context) error {
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.Del(ctx, contractHorizonControlKey).Err()
	}, server.OptRedisNoPreflight())
}

// Where a pass resumes. It round trips through the task args.
type ContractHorizonCursor struct {
	// The last contract id the pass committed past. The zero id starts the
	// pass at the oldest contract.
	ContractId server.Id `json:"contract_id"`
	// When the pass started. Zero before the first run.
	PassStartTime time.Time `json:"pass_start_time,omitzero"`
	// The adaptive batch size, carried between runs. Zero starts at the
	// control's batch size.
	BatchSize int `json:"batch_size,omitempty"`
}

// Why a run ended.
const (
	// the pass reached the horizon
	contractHorizonStopHorizon = "horizon"
	// the run spent its budget
	contractHorizonStopBudget = "budget"
	// the mode is off
	contractHorizonStopOff = "off"
	// the control could not be read
	contractHorizonStopControl = "control_unavailable"
	// the task context ended; the batch in flight rolled back
	contractHorizonStopCanceled = "canceled"
	// a batch failed
	contractHorizonStopError = "error"
)

// Why a contract the pass read was kept.
const (
	// open: no outcome and no dispute
	contractHorizonProtectedOpen = "open"
	// no outcome, under dispute
	contractHorizonProtectedUnresolved = "unresolved"
	// resolved, but its close time is not past the horizon
	contractHorizonProtectedRecentClose = "recent_close"
	// another transaction holds the contract or one of its sweeps
	contractHorizonProtectedBusy = "busy"
	// an escrow is not settled
	contractHorizonProtectedUnsettled = "unsettled"
	// a sweep belongs to a payment that holds retention
	contractHorizonProtectedPaymentHold = "payment_hold"
	// a sweep belongs to a payment that is not completed and holds nothing,
	// such as a safely canceled one the planner re-plans, or to none that
	// exists
	contractHorizonProtectedPaymentIncomplete = "payment_incomplete"
	// a sweep's payment completed within CompletedContractExpiration
	contractHorizonProtectedPaymentRecent = "payment_recent"
	// a sweep has no payment, on a contract minted before the earning cutoff
	contractHorizonProtectedUnpaidLegacy = "unpaid_legacy"
	// a sweep is staged in a payment plan whose assignment is pending
	contractHorizonProtectedPlanStaged = "plan_staged"
)

var contractHorizonProtectedReasons = []string{
	contractHorizonProtectedOpen,
	contractHorizonProtectedUnresolved,
	contractHorizonProtectedRecentClose,
	contractHorizonProtectedBusy,
	contractHorizonProtectedUnsettled,
	contractHorizonProtectedPaymentHold,
	contractHorizonProtectedPaymentIncomplete,
	contractHorizonProtectedPaymentRecent,
	contractHorizonProtectedUnpaidLegacy,
	contractHorizonProtectedPlanStaged,
}

// The tables a deleted contract leaves, dependents first.
var contractHorizonTables = []string{
	"contract_close_report",
	"contract_close",
	"transfer_escrow",
	"transfer_escrow_sweep",
	"contract_extender",
	"transfer_contract",
}

// What one run did.
type ContractHorizonReport struct {
	// The mode of the last batch.
	Mode ContractHorizonMode
	// The horizon of the last control read, or the code's before the first:
	// contracts minted and closed before it are past it.
	HorizonTime time.Time
	Batches     int
	// Contracts read.
	Scanned int64
	// Contracts that passed every check. In on mode, the contracts deleted.
	Eligible int64
	// Rows deleted, by table.
	Deleted map[string]int64
	// Credit-bearing contracts deleted, each archived by the capture trigger.
	Archived int64
	// Contracts kept, by reason.
	Protected map[string]int64
	// The pass reached the horizon.
	CaughtUp bool
	// Why the run ended.
	Stop string
}

// The run found nothing to do: the reaper is off or its control unreadable.
func (self *ContractHorizonReport) Idle() bool {
	return self.Stop == contractHorizonStopOff || self.Stop == contractHorizonStopControl
}

func (self *ContractHorizonReport) add(batch *contractHorizonBatch) {
	self.Batches += 1
	self.Scanned += batch.scanned
	self.Eligible += int64(len(batch.eligibleIds))
	self.Archived += batch.archived
	for table, count := range batch.deleted {
		self.Deleted[table] += count
	}
	for reason, count := range batch.protected {
		self.Protected[reason] += count
	}
}

var (
	contractHorizonDeletedRowsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_deleted_rows_total",
		Help: "Rows the contract horizon reaper deleted, by table",
	}, []string{"table"})
	contractHorizonArchivedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_archived_contracts_total",
		Help: "Credit-bearing contracts the contract horizon reaper deleted, each archived to st_provider_usage_archive",
	})
	contractHorizonScannedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_scanned_contracts_total",
		Help: "Contracts the contract horizon reaper read, by mode",
	}, []string{"mode"})
	contractHorizonEligibleCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_eligible_contracts_total",
		Help: "Contracts past the horizon that passed every check, by mode; in on mode each was deleted",
	}, []string{"mode"})
	contractHorizonProtectedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_protected_contracts_total",
		Help: "Contracts the contract horizon reaper read and kept, by reason",
	}, []string{"reason"})
	contractHorizonBatchDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "urnetwork_contract_horizon_batch_duration_seconds",
		Help:    "Duration of one contract horizon reaper batch transaction, by mode and outcome",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 18, 30, 60},
	}, []string{"mode", "outcome"})
	contractHorizonBatchSizeGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_batch_size",
		Help: "Contracts per batch the contract horizon reaper last used",
	})
	contractHorizonCursorGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_cursor_timestamp_seconds",
		Help: "Mint time of the last contract id the contract horizon reaper's pass committed past",
	})
	contractHorizonHorizonGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_horizon_timestamp_seconds",
		Help: "The horizon of the contract horizon reaper's last control read: contracts minted and closed before it are past it",
	})
	contractHorizonHorizonSecondsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_horizon_seconds",
		Help: "How long past its creation and its close a contract is kept, as the contract horizon reaper's last control read set it: 45 to 91 days",
	})
	contractHorizonLagGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_lag_seconds",
		Help: "How far the contract horizon reaper's pass is behind its horizon; it restarts with each rescan",
	})
	contractHorizonPassStartGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_pass_start_timestamp_seconds",
		Help: "When the contract horizon reaper's current pass started",
	})
	contractHorizonModeGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_horizon_mode",
		Help: "The contract horizon reaper's mode at its last control read: 0 off, 1 dry run, 2 on",
	})
	contractHorizonRunCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_contract_horizon_runs_total",
		Help: "Contract horizon reaper runs, by why they ended",
	}, []string{"stop"})
)

func init() {
	for _, table := range contractHorizonTables {
		contractHorizonDeletedRowsCounter.WithLabelValues(table)
	}
	for _, mode := range []ContractHorizonMode{ContractHorizonModeDryRun, ContractHorizonModeOn} {
		contractHorizonScannedCounter.WithLabelValues(mode.String())
		contractHorizonEligibleCounter.WithLabelValues(mode.String())
	}
	for _, reason := range contractHorizonProtectedReasons {
		contractHorizonProtectedCounter.WithLabelValues(reason)
	}
	for _, stop := range []string{
		contractHorizonStopHorizon,
		contractHorizonStopBudget,
		contractHorizonStopOff,
		contractHorizonStopControl,
		contractHorizonStopCanceled,
		contractHorizonStopError,
	} {
		contractHorizonRunCounter.WithLabelValues(stop)
	}
	prometheus.MustRegister(
		contractHorizonDeletedRowsCounter,
		contractHorizonArchivedCounter,
		contractHorizonScannedCounter,
		contractHorizonEligibleCounter,
		contractHorizonProtectedCounter,
		contractHorizonBatchDuration,
		contractHorizonBatchSizeGauge,
		contractHorizonCursorGauge,
		contractHorizonHorizonGauge,
		contractHorizonHorizonSecondsGauge,
		contractHorizonLagGauge,
		contractHorizonPassStartGauge,
		contractHorizonModeGauge,
		contractHorizonRunCounter,
	)
}

// Observes each committed batch, after its commit. Nil in production; tests
// use it to stop a run at an exact batch boundary.
var contractHorizonBatchCommitted func(batch int, last server.Id)

// One run: batches from cursor toward the horizon of now, until the horizon,
// the budget, the mode or ctx stops it. next is where the following run
// resumes, also when err is set: every batch before the failed one committed.
// A canceled run is not an error; its batch in flight rolled back.
func RemoveContractsPastHorizon(
	ctx context.Context,
	now time.Time,
	cursor ContractHorizonCursor,
	settings *ContractHorizonSettings,
) (report *ContractHorizonReport, next ContractHorizonCursor, returnErr error) {
	reaper := &contractHorizonReaper{
		settings:             settings,
		now:                  now.UTC(),
		paymentDischargeTime: now.UTC().Add(-CompletedContractExpiration),
		runEnd:               time.Now().Add(settings.RunBudget),
	}
	// the code's horizon until the control is read
	reaper.setHorizon(settings.horizon())
	report = &ContractHorizonReport{
		HorizonTime: reaper.horizonTime,
		Deleted:     map[string]int64{},
		Protected:   map[string]int64{},
		// every return sets its stop; a panic counts as an error
		Stop: contractHorizonStopError,
	}
	next = cursor
	if next.PassStartTime.IsZero() {
		next.PassStartTime = now.UTC()
	}
	contractHorizonPassStartGauge.Set(float64(next.PassStartTime.Unix()))
	defer func() {
		contractHorizonRunCounter.WithLabelValues(report.Stop).Inc()
		if glog.V(1) {
			glog.Infof(
				"[contract_horizon]run mode=%s stop=%s batches=%d scanned=%d eligible=%d deleted=%v archived=%d protected=%v caught_up=%t horizon_days=%g horizon=%s\n",
				report.Mode,
				report.Stop,
				report.Batches,
				report.Scanned,
				report.Eligible,
				report.Deleted,
				report.Archived,
				report.Protected,
				report.CaughtUp,
				reaper.horizon.Hours()/24,
				report.HorizonTime.Format(time.RFC3339),
			)
		}
	}()

	// The planner's legacy selection reaches unpaid sweeps below this bound.
	// Without the boundary nothing is deleted.
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		report.Stop = contractHorizonStopError
		returnErr = err
		return
	}
	reaper.unpaidLegacyBound = paymentPlanUnpaidContractIdBound(transition)

	// A dry run reads ahead of the stored cursor without advancing it; an on
	// batch always starts at the stored cursor, so switching modes mid-run
	// skips nothing.
	dryRunPosition := next.ContractId
	for {
		select {
		case <-ctx.Done():
			report.Stop = contractHorizonStopCanceled
			return
		default:
		}
		if !time.Now().Before(reaper.runEnd) {
			report.Stop = contractHorizonStopBudget
			return
		}
		control, err := GetContractHorizonControl(ctx, settings)
		if err != nil {
			report.Stop = contractHorizonStopControl
			glog.V(1).Infof("[contract_horizon]control unavailable, stopping: %s\n", err)
			return
		}
		report.Mode = control.Mode
		contractHorizonModeGauge.Set(float64(control.Mode))
		reaper.setHorizon(control.Horizon)
		report.HorizonTime = reaper.horizonTime
		reaper.observeHorizon(next.ContractId)
		if control.Mode == ContractHorizonModeOff {
			report.Stop = contractHorizonStopOff
			return
		}

		batchSize := next.BatchSize
		if batchSize <= 0 || control.BatchSize < batchSize {
			batchSize = control.BatchSize
		}
		start := next.ContractId
		if control.Mode == ContractHorizonModeDryRun {
			start = dryRunPosition
		}
		batchStart := time.Now()
		batch, err := reaper.removeBatch(ctx, start, batchSize, control.Mode)
		batchDuration := time.Since(batchStart)
		if err != nil {
			contractHorizonBatchDuration.WithLabelValues(control.Mode.String(), "error").Observe(batchDuration.Seconds())
			if ctx.Err() != nil {
				report.Stop = contractHorizonStopCanceled
				return
			}
			report.Stop = contractHorizonStopError
			returnErr = err
			return
		}
		contractHorizonBatchDuration.WithLabelValues(control.Mode.String(), "ok").Observe(batchDuration.Seconds())
		report.add(batch)
		reaper.observe(batch, control.Mode)

		if batch.scanned == 0 {
			report.CaughtUp = true
			report.Stop = contractHorizonStopHorizon
			if control.Mode == ContractHorizonModeOn && settings.RescanInterval <= now.Sub(next.PassStartTime) {
				// revisit what this pass kept
				next = ContractHorizonCursor{
					PassStartTime: now.UTC(),
					BatchSize:     next.BatchSize,
				}
			}
			return
		}
		if control.Mode == ContractHorizonModeOn {
			next.ContractId = batch.lastId
			dryRunPosition = batch.lastId
			reaper.observeCursor(next.ContractId)
			if contractHorizonBatchCommitted != nil {
				contractHorizonBatchCommitted(report.Batches, batch.lastId)
			}
		} else {
			dryRunPosition = batch.lastId
		}
		next.BatchSize = nextContractHorizonBatchSize(batchSize, batchDuration, control.BatchSize, settings)
		contractHorizonBatchSizeGauge.Set(float64(next.BatchSize))

		if !reaper.pace(ctx, contractHorizonPaceDelay(batch.scanned, control.MaxRowsPerSecond, batchDuration)) {
			report.Stop = contractHorizonStopCanceled
			return
		}
	}
}

// The size of the batch after one of size that took duration: halved when it
// ran past the target, grown by half when it took under a quarter of it, and
// kept between the minimum and ceiling. Pure.
func nextContractHorizonBatchSize(size int, duration time.Duration, ceiling int, settings *ContractHorizonSettings) int {
	if settings.TargetBatchDuration < duration {
		size = size / 2
	} else if duration < settings.TargetBatchDuration/4 {
		size = size + max(1, size/2)
	}
	return max(settings.MinBatchSize, min(ceiling, size))
}

// The wait after a batch of scanned contracts that took duration, so that the
// pass reads at most rate contracts a second. Pure.
func contractHorizonPaceDelay(scanned int64, rate float64, duration time.Duration) time.Duration {
	if !(0 < rate) {
		return 0
	}
	paced := time.Duration(float64(scanned) / rate * float64(time.Second))
	return max(0, paced-duration)
}

// One run's clock, horizon, payout boundaries and budget. The horizon follows
// the control from one batch to the next; the clock stays the run's.
type contractHorizonReaper struct {
	settings *ContractHorizonSettings
	// The run's now, which the horizon is measured back from.
	now time.Time
	// How long past its creation and its close a contract is kept.
	horizon     time.Duration
	horizonTime time.Time
	horizonId   server.Id
	// The first contract id whose unpaid sweeps the planner never selects;
	// nil without an earning cutoff, when every unpaid sweep is a legacy
	// obligation.
	unpaidLegacyBound *server.Id
	// A sweep's completed payment discharges it once it completed before this.
	paymentDischargeTime time.Time
	runEnd               time.Time
}

// Waits delay, or until the run budget ends. False when ctx ended first.
func (self *contractHorizonReaper) pace(ctx context.Context, delay time.Duration) bool {
	delay = min(delay, time.Until(self.runEnd))
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (self *contractHorizonReaper) observe(batch *contractHorizonBatch, mode ContractHorizonMode) {
	contractHorizonScannedCounter.WithLabelValues(mode.String()).Add(float64(batch.scanned))
	contractHorizonEligibleCounter.WithLabelValues(mode.String()).Add(float64(len(batch.eligibleIds)))
	for reason, count := range batch.protected {
		contractHorizonProtectedCounter.WithLabelValues(reason).Add(float64(count))
	}
	for table, count := range batch.deleted {
		contractHorizonDeletedRowsCounter.WithLabelValues(table).Add(float64(count))
	}
	contractHorizonArchivedCounter.Add(float64(batch.archived))
}

// The next batch reads the contracts minted and closed more than horizon
// before the run's now.
func (self *contractHorizonReaper) setHorizon(horizon time.Duration) {
	self.horizon = horizon
	self.horizonTime = self.now.Add(-horizon)
	self.horizonId = server.ContractIdFloor(self.horizonTime)
}

// Publishes the horizon a control read set, and the stored cursor's lag
// behind it.
func (self *contractHorizonReaper) observeHorizon(cursor server.Id) {
	contractHorizonHorizonGauge.Set(float64(self.horizonTime.Unix()))
	contractHorizonHorizonSecondsGauge.Set(self.horizon.Seconds())
	if cursor != (server.Id{}) {
		self.observeCursor(cursor)
	}
}

func (self *contractHorizonReaper) observeCursor(contractId server.Id) {
	cursorTime := server.ContractIdTime(contractId)
	contractHorizonCursorGauge.Set(float64(cursorTime.Unix()))
	contractHorizonLagGauge.Set(max(0, self.horizonTime.Sub(cursorTime).Seconds()))
}

// One batch's reads and deletes.
type contractHorizonBatch struct {
	// contracts read
	scanned int64
	// the last contract id read
	lastId server.Id
	// contracts that passed every check, deleted in on mode
	eligibleIds []server.Id
	protected   map[string]int64
	deleted     map[string]int64
	archived    int64
}

// The next batchSize contract ids after start and below the horizon, in id
// order: an index-only range of the primary key, which reads the heap only for
// pages vacuum has not marked all-visible. $1 start, $2 the horizon id, $3 the
// batch size.
const contractHorizonWindowSql = `
	SELECT ARRAY(
		SELECT contract_id
		FROM transfer_contract
		WHERE $1 < contract_id AND contract_id < $2
		ORDER BY contract_id
		LIMIT $3
	)
`

// The statements after the window name their rows by the batch's ids. A plain
// index scan would read each row's heap page synchronously, about one random
// read per row: rows written together are spread about one to a page (Main,
// 2026-10-30, stEpochProviderUsageWholeWindowSettings). A bitmap scan reads
// each page once, in block order, with asynchronous prefetch. The window
// itself keeps the default plan: an index-only scan needs index scans on.
var contractHorizonBitmapSettings = []string{
	`SET LOCAL enable_indexscan = off`,
	`SET LOCAL enable_seqscan = off`,
	`SET LOCAL effective_io_concurrency = 32`,
}

// The row checks of the window's contracts: resolved and closed before the
// horizon. In on mode the contracts that pass are locked, skipping any another
// transaction holds; the lock rereads each row's latest version. $1 the window
// ids, $2 the horizon time.
const contractHorizonScanSqlFormat = `
	WITH scanned AS MATERIALIZED (
		SELECT contract_id, open, outcome, close_time
		FROM transfer_contract
		WHERE contract_id = ANY($1::uuid[])
	), candidate AS MATERIALIZED (
		SELECT contract_id
		FROM scanned
		WHERE outcome IS NOT NULL AND close_time < $2
	)%s
	SELECT
		(SELECT count(*) FROM scanned WHERE outcome IS NULL AND open),
		(SELECT count(*) FROM scanned WHERE outcome IS NULL AND NOT open),
		(SELECT count(*) FROM scanned WHERE outcome IS NOT NULL AND (close_time IS NULL OR $2 <= close_time)),
		(SELECT count(*) FROM candidate),
		ARRAY(SELECT contract_id FROM %s ORDER BY contract_id)
`

// The row lock of the on mode, after which the dependents' writers wait.
const contractHorizonLockSql = `, locked AS MATERIALIZED (
		SELECT transfer_contract.contract_id
		FROM transfer_contract
		WHERE transfer_contract.contract_id = ANY(ARRAY(SELECT contract_id FROM candidate))
			AND transfer_contract.outcome IS NOT NULL
			AND transfer_contract.close_time < $2
		FOR UPDATE OF transfer_contract SKIP LOCKED
	)`

var contractHorizonScanSql = fmt.Sprintf(contractHorizonScanSqlFormat, "", "candidate")

var contractHorizonScanLockSql = fmt.Sprintf(contractHorizonScanSqlFormat, contractHorizonLockSql, "locked")

// Locks the sweeps of the locked contracts, skipping any another transaction
// holds, and returns the contracts with a sweep it could not lock. A planner
// that assigns one of the locked sweeps waits for this transaction, and then
// finds it gone.
const contractHorizonSweepLockSql = `
	WITH locked_sweep AS MATERIALIZED (
		SELECT contract_id
		FROM transfer_escrow_sweep
		WHERE contract_id = ANY($1::uuid[])
		FOR UPDATE SKIP LOCKED
	)
	SELECT
		(SELECT count(*) FROM locked_sweep),
		ARRAY(
			SELECT sweep.contract_id
			FROM transfer_escrow_sweep AS sweep
			WHERE sweep.contract_id = ANY($1::uuid[])
			GROUP BY sweep.contract_id
			HAVING (SELECT count(*) FROM locked_sweep WHERE locked_sweep.contract_id = sweep.contract_id) < count(*)
			ORDER BY sweep.contract_id
		)
`

// The checks that read other tables, in a snapshot taken after the locks:
// every escrow settled, every sweep discharged, none staged in a pending plan.
// Each reads the candidates' rows together, by their ids. A NULL flag counts
// against deletion. $1 the candidate contract ids, $2 the first contract id
// past the earning cutoff (paymentPlanUnpaidContractIdBound), NULL without a
// cutoff, $3 the newest completion time that discharges a sweep.
var contractHorizonHoldSql = `
	WITH unsettled AS MATERIALIZED (
		SELECT DISTINCT contract_id
		FROM transfer_escrow
		WHERE contract_id = ANY($1::uuid[]) AND settled IS NOT TRUE
	), sweep AS MATERIALIZED (
		SELECT
			transfer_escrow_sweep.contract_id,
			bool_or(account_payment.payment_id IS NOT NULL AND (` + server.ContractPaymentRetentionHoldSql + `) IS TRUE) AS payment_hold,
			bool_or(transfer_escrow_sweep.payment_id IS NOT NULL AND account_payment.completed IS NOT TRUE) AS payment_incomplete,
			bool_or(account_payment.completed IS TRUE AND (account_payment.complete_time IS NULL OR $3 <= account_payment.complete_time)) AS payment_recent,
			bool_or(transfer_escrow_sweep.payment_id IS NULL AND ($2::uuid IS NULL OR transfer_escrow_sweep.contract_id < $2::uuid)) AS unpaid_legacy
		FROM transfer_escrow_sweep
		LEFT JOIN account_payment ON
			account_payment.payment_id = transfer_escrow_sweep.payment_id
		WHERE transfer_escrow_sweep.contract_id = ANY($1::uuid[])
		GROUP BY transfer_escrow_sweep.contract_id
	), staged AS MATERIALIZED (
		SELECT DISTINCT payment_plan_sweep.contract_id
		FROM payment_plan_sweep
		INNER JOIN payment_plan_assignment ON
			payment_plan_assignment.payment_plan_id = payment_plan_sweep.payment_plan_id
		WHERE
			payment_plan_sweep.contract_id = ANY($1::uuid[]) AND
			payment_plan_assignment.complete_time IS NULL
	), classified AS MATERIALIZED (
		SELECT
			candidate.contract_id,
			unsettled.contract_id IS NOT NULL AS unsettled,
			coalesce(sweep.payment_hold, false) AS payment_hold,
			coalesce(sweep.payment_incomplete, false) AS payment_incomplete,
			coalesce(sweep.payment_recent, false) AS payment_recent,
			coalesce(sweep.unpaid_legacy, false) AS unpaid_legacy,
			staged.contract_id IS NOT NULL AS plan_staged
		FROM unnest($1::uuid[]) AS candidate(contract_id)
		LEFT JOIN unsettled ON unsettled.contract_id = candidate.contract_id
		LEFT JOIN sweep ON sweep.contract_id = candidate.contract_id
		LEFT JOIN staged ON staged.contract_id = candidate.contract_id
	)
	SELECT
		(SELECT count(*) FROM classified WHERE unsettled),
		(SELECT count(*) FROM classified WHERE NOT unsettled AND payment_hold),
		(SELECT count(*) FROM classified WHERE NOT (unsettled OR payment_hold) AND payment_incomplete),
		(SELECT count(*) FROM classified WHERE NOT (unsettled OR payment_hold OR payment_incomplete) AND payment_recent),
		(SELECT count(*) FROM classified WHERE NOT (unsettled OR payment_hold OR payment_incomplete OR payment_recent) AND unpaid_legacy),
		(SELECT count(*) FROM classified WHERE NOT (unsettled OR payment_hold OR payment_incomplete OR payment_recent OR unpaid_legacy) AND plan_staged),
		ARRAY(
			SELECT contract_id FROM classified
			WHERE NOT (unsettled OR payment_hold OR payment_incomplete OR payment_recent OR unpaid_legacy OR plan_staged)
			ORDER BY contract_id
		)
`

// Deletes the dependents of the eligible contracts. Each table's key leads
// with the contract id, so each delete is one ordered index range walk.
const contractHorizonDeleteDependentsSql = `
	WITH deleted_report AS (
		DELETE FROM contract_close_report WHERE contract_id = ANY($1::uuid[]) RETURNING 1
	), deleted_close AS (
		DELETE FROM contract_close WHERE contract_id = ANY($1::uuid[]) RETURNING 1
	), deleted_escrow AS (
		DELETE FROM transfer_escrow WHERE contract_id = ANY($1::uuid[]) RETURNING 1
	), deleted_sweep AS (
		DELETE FROM transfer_escrow_sweep WHERE contract_id = ANY($1::uuid[]) RETURNING 1
	), deleted_extender AS (
		DELETE FROM contract_extender WHERE contract_id = ANY($1::uuid[]) RETURNING 1
	)
	SELECT
		(SELECT count(*) FROM deleted_report),
		(SELECT count(*) FROM deleted_close),
		(SELECT count(*) FROM deleted_escrow),
		(SELECT count(*) FROM deleted_sweep),
		(SELECT count(*) FROM deleted_extender)
`

// Deletes the eligible contracts. The capture trigger archives each
// credit-bearing one (the outcomes of ProviderUsageArchiveCaptureFunctionBodySql).
const contractHorizonDeleteContractsSql = `
	WITH deleted AS (
		DELETE FROM transfer_contract
		WHERE contract_id = ANY($1::uuid[])
		RETURNING outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination') AS archived
	)
	SELECT count(*), count(*) FILTER (WHERE archived) FROM deleted
`

var errContractHorizonDeleteChanged = errors.New("contract horizon reaper deleted a different number of contracts than it locked")

// One batch transaction. In dry run mode it reads only, in a read only
// transaction, and takes no lock.
func (self *contractHorizonReaper) removeBatch(
	ctx context.Context,
	start server.Id,
	batchSize int,
	mode ContractHorizonMode,
) (batch *contractHorizonBatch, returnErr error) {
	dryRun := mode != ContractHorizonModeOn
	options := []any{server.TxReadCommitted, server.TxLockTimeout(self.settings.LockTimeout)}
	if dryRun {
		options = append(options, pgx.ReadOnly)
	}
	if r := server.HandleError(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// a retried transaction starts over
			batch = &contractHorizonBatch{
				protected: map[string]int64{},
				deleted:   map[string]int64{},
			}
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, self.settings.StatementTimeout.Milliseconds())))
			// plan each statement for its actual ids
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`))

			var windowIds []server.Id
			server.Raise(tx.QueryRow(ctx, contractHorizonWindowSql, start, self.horizonId, batchSize).Scan(&windowIds))
			batch.scanned = int64(len(windowIds))
			if batch.scanned == 0 {
				return
			}
			batch.lastId = windowIds[len(windowIds)-1]
			for _, setting := range contractHorizonBitmapSettings {
				server.RaisePgResult(tx.Exec(ctx, setting))
			}

			scanSql := contractHorizonScanLockSql
			if dryRun {
				scanSql = contractHorizonScanSql
			}
			var openCount, unresolvedCount, recentCloseCount, candidateCount int64
			var heldIds []server.Id
			server.Raise(tx.QueryRow(ctx, scanSql, windowIds, self.horizonTime).Scan(
				&openCount,
				&unresolvedCount,
				&recentCloseCount,
				&candidateCount,
				&heldIds,
			))
			batch.protected[contractHorizonProtectedOpen] = openCount
			batch.protected[contractHorizonProtectedUnresolved] = unresolvedCount
			batch.protected[contractHorizonProtectedRecentClose] = recentCloseCount
			busyCount := candidateCount - int64(len(heldIds))

			if !dryRun && 0 < len(heldIds) {
				var lockedSweepCount int64
				var busySweepIds []server.Id
				server.Raise(tx.QueryRow(ctx, contractHorizonSweepLockSql, heldIds).Scan(&lockedSweepCount, &busySweepIds))
				if 0 < len(busySweepIds) {
					busyCount += int64(len(busySweepIds))
					heldIds = withoutContractIds(heldIds, busySweepIds)
				}
			}
			batch.protected[contractHorizonProtectedBusy] = busyCount
			if len(heldIds) == 0 {
				return
			}

			var unsettledCount, paymentHoldCount, paymentIncompleteCount, paymentRecentCount, unpaidLegacyCount, planStagedCount int64
			server.Raise(tx.QueryRow(
				ctx,
				contractHorizonHoldSql,
				heldIds,
				self.unpaidLegacyBound,
				self.paymentDischargeTime,
			).Scan(
				&unsettledCount,
				&paymentHoldCount,
				&paymentIncompleteCount,
				&paymentRecentCount,
				&unpaidLegacyCount,
				&planStagedCount,
				&batch.eligibleIds,
			))
			batch.protected[contractHorizonProtectedUnsettled] = unsettledCount
			batch.protected[contractHorizonProtectedPaymentHold] = paymentHoldCount
			batch.protected[contractHorizonProtectedPaymentIncomplete] = paymentIncompleteCount
			batch.protected[contractHorizonProtectedPaymentRecent] = paymentRecentCount
			batch.protected[contractHorizonProtectedUnpaidLegacy] = unpaidLegacyCount
			batch.protected[contractHorizonProtectedPlanStaged] = planStagedCount
			if dryRun || len(batch.eligibleIds) == 0 {
				return
			}

			var reportCount, closeCount, escrowCount, sweepCount, extenderCount int64
			server.Raise(tx.QueryRow(ctx, contractHorizonDeleteDependentsSql, batch.eligibleIds).Scan(
				&reportCount,
				&closeCount,
				&escrowCount,
				&sweepCount,
				&extenderCount,
			))
			var contractCount int64
			server.Raise(tx.QueryRow(ctx, contractHorizonDeleteContractsSql, batch.eligibleIds).Scan(
				&contractCount,
				&batch.archived,
			))
			if contractCount != int64(len(batch.eligibleIds)) {
				// every eligible contract is locked by this transaction
				panic(errContractHorizonDeleteChanged)
			}
			batch.deleted["contract_close_report"] = reportCount
			batch.deleted["contract_close"] = closeCount
			batch.deleted["transfer_escrow"] = escrowCount
			batch.deleted["transfer_escrow_sweep"] = sweepCount
			batch.deleted["contract_extender"] = extenderCount
			batch.deleted["transfer_contract"] = contractCount
		}, options...)
	}); r != nil {
		batch = nil
		if err, ok := r.(error); ok {
			returnErr = err
		} else {
			returnErr = fmt.Errorf("%v", r)
		}
	}
	return
}

// contractIds without the ids in removeIds. Both are in id order.
func withoutContractIds(contractIds []server.Id, removeIds []server.Id) []server.Id {
	kept := make([]server.Id, 0, len(contractIds))
	i := 0
	for _, contractId := range contractIds {
		for i < len(removeIds) && removeIds[i].Less(contractId) {
			i += 1
		}
		if i < len(removeIds) && removeIds[i] == contractId {
			continue
		}
		kept = append(kept, contractId)
	}
	return kept
}
