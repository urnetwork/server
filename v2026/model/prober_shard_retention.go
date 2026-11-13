// Prober data retention by shard generation. The owner, 2026-11-01: "The probe
// retention window should be sharded and old shards dropped. The retention
// needs to be bounded not cut." Probe volume stays as it is.
//
// The shard key. Every row a prober writes belongs to the private network of
// one probe pass (prober_shard_run, BeginProberShard): the pass's network,
// user, authentication, device and credit; the clients it mints and their
// provide keys, connections, TLS certificates, roles and location reliability
// rows; and every contract it funds. Such a contract's payer is that network,
// and its source (an origin) or its destination (a reply companion) is one of
// the network's clients (validateProberShardPayerInTx). A pass lives at most a
// day and, on Main, 15 minutes.
//
// The window. A generation is the passes created in one UTC day
// (GenerationDuration). The retention keeps the newest RetainedGenerations of
// them, today's included: eight by default, a week of probe history past the
// current day, which spans a weekly payout and an epoch close. When a day
// begins, the oldest kept generation falls out and is dropped as a unit. The
// retention is a fixed number of shards: a pass is never cut short of its
// generation. A pass ends by draining and settling its shard
// (SettleProberShard), and nothing deletes it before its generation drops.
//
// The drop. For each pass of a dropped generation, oldest first:
//  1. its zero-byte contracts: resolved and closed before the cutoff, every
//     escrow settled, no sweep and no destination close with bytes. Such a
//     contract adds nothing to any byte sum: not the daily transfer rollup or
//     the clock backfill (each sums destination close bytes), the subnet usage
//     weight (its archived usage is zero bytes), a payout, a payment plan or an
//     operator block award (each reads sweeps). The rows deleted are the
//     contract horizon reaper's own (contractHorizonDeleteDependentsSql,
//     contractHorizonDeleteContractsSql), and the capture trigger archives a
//     credit-bearing contract's usage in the same statement. On Main 57% of
//     prober contracts are zero-byte. Every contract dropped here is one the
//     horizon reaper deletes at its horizon; this only deletes it sooner.
//  2. its identity, in ReapProberShard's one transaction, which refuses a
//     shard with an unresolved contract, an unsettled escrow, a pending debit,
//     a connected client or a balance that is not a plain grant.
//  3. its registry row.
//
// What a drop keeps, for the retention that owns it:
//   - the contracts that carry bytes, which the transfer rollup can still read
//     until TransferAuditRollupFirstDay passes their close day: the contract
//     horizon reaper deletes them at its horizon;
//   - close evidence, which keeps its own retention and takes evidence whose
//     contract is gone (server.RemoveExpiredCloseEvidence);
//   - provider work session events and heads, whose guards refuse a delete
//     (the C4 partition's retention);
//   - a contract or client a pass's network has lost: once the 30-day client
//     reap removed a client, its contracts are left to the horizon reaper.
//
// Rotation. No probe in flight loses its identity:
//   - a generation drops only once RetainedGenerations-1 later generations have
//     begun, and the control cannot set fewer than MinRetainedGenerations;
//   - a pass drops only once its deadline is MinDeadlineAge in the past, so a
//     pass that began at the end of a day keeps its whole lifetime;
//   - the identity transaction refuses a pass with any unresolved contract,
//     unsettled escrow or connected client, and a refused pass waits
//     BlockedRetryInterval before the next try.
//
// The runtime control, the Redis hash `{prober_retention}control`, turns the
// drop on, off or to a dry run, shortens or lengthens the window within the
// code's bounds, paces the contract deletes and turns the contract step off,
// without a deploy. It is read before every pass, so setting the mode off stops
// a running drop within one pass. A run that cannot read it stops, as if the
// mode were off. The code's default mode is off: a deploy drops nothing until
// an operator sets the control.
package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// The drop's mode, ordered from narrowest to widest.
type ProberShardRetentionMode int

const (
	// Drops nothing and reads nothing.
	ProberShardRetentionModeOff ProberShardRetentionMode = 0
	// Reads the oldest due passes and counts what a drop would delete, in read
	// only transactions, and changes nothing.
	ProberShardRetentionModeDryRun ProberShardRetentionMode = 1
	// Drops.
	ProberShardRetentionModeOn ProberShardRetentionMode = 2
)

// off, dry_run or on. False for any other value.
func ParseProberShardRetentionMode(value string) (ProberShardRetentionMode, bool) {
	switch strings.TrimSpace(value) {
	case "off":
		return ProberShardRetentionModeOff, true
	case "dry_run":
		return ProberShardRetentionModeDryRun, true
	case "on":
		return ProberShardRetentionModeOn, true
	default:
		return ProberShardRetentionModeOff, false
	}
}

// The mode's control value. Any value outside the modes prints off.
func (self ProberShardRetentionMode) String() string {
	switch self {
	case ProberShardRetentionModeDryRun:
		return "dry_run"
	case ProberShardRetentionModeOn:
		return "on"
	default:
		return "off"
	}
}

// Code settings, which bound the runtime control.
type ProberShardRetentionSettings struct {
	// False forces the mode off, whatever the control says.
	Enable bool
	// The mode while the control sets none.
	DefaultMode ProberShardRetentionMode
	// One generation: the passes created in one span of this length, aligned
	// to UTC. It divides a day.
	GenerationDuration time.Duration
	// Generations kept, the current one included, while the control sets none.
	RetainedGenerations int
	// The fewest and the most generations the control can keep.
	MinRetainedGenerations int
	MaxRetainedGenerations int
	// A pass drops only once its deadline is this far in the past.
	MinDeadlineAge time.Duration
	// The contract step runs while the control does not turn it off.
	DropContracts bool
	// Contracts read, classified and deleted per transaction, at most.
	ContractBatchSize int
	// Clients whose contracts one candidate read covers.
	ClientChunkSize int
	// Contracts read per second while the control sets no rate, and the most
	// the control can set.
	DefaultMaxContractsPerSecond float64
	MaxContractsPerSecond        float64
	// Passes dropped per minute while the control sets no rate, and the most
	// the control can set. A pass's identity transaction deletes its clients'
	// rows at once, so this paces those deletes.
	DefaultMaxShardsPerMinute float64
	MaxShardsPerMinute        float64
	// Passes selected and locked together.
	ShardPageSize int
	// Closed registry rows of dropped generations deleted per transaction.
	RegistryBatchSize int
	// Wall clock for one run, pacing included. Well under the task's MaxTime.
	RunBudget time.Duration
	// Bounds each statement of a contract transaction.
	StatementTimeout time.Duration
	// Bounds each lock wait of a contract transaction; the contract locks
	// themselves skip locked rows.
	LockTimeout time.Duration
	// Bounds the control read.
	ControlTimeout time.Duration
	// A pass the identity transaction refused is retried after this.
	BlockedRetryInterval time.Duration
	// A pass whose drop failed is retried after this.
	FailedRetryInterval time.Duration
	// Gap between runs while due passes remain.
	ResumeInterval time.Duration
	// Gap after a run that found nothing due, the drop off or dry run, or its
	// control unreadable.
	IdleInterval time.Duration
	// The task's claim bound.
	TaskMaxTime time.Duration
}

func DefaultProberShardRetentionSettings() *ProberShardRetentionSettings {
	return &ProberShardRetentionSettings{
		Enable:      true,
		DefaultMode: ProberShardRetentionModeOff,
		// a UTC day: about 1,600 passes on Main
		GenerationDuration:     24 * time.Hour,
		RetainedGenerations:    8,
		MinRetainedGenerations: 2,
		// the 30 days a prober client was kept before the shards
		MaxRetainedGenerations: 30,
		// a pass lives at most a day (BeginProberShard), 15 minutes on Main
		MinDeadlineAge:    6 * time.Hour,
		DropContracts:     true,
		ContractBatchSize: 500,
		ClientChunkSize:   256,
		// about 5,500 zero-byte prober contracts a day per pass slot on Main:
		// 200 a second keeps a day's drop to about 11 hours
		DefaultMaxContractsPerSecond: 200,
		MaxContractsPerSecond:        2000,
		// a day's 1,600 passes in under an hour; the first drop's backlog of
		// about 49,000 passes (Main, 2026-11-01) in about 27 hours
		DefaultMaxShardsPerMinute: 30,
		MaxShardsPerMinute:        600,
		ShardPageSize:             16,
		RegistryBatchSize:         1000,
		RunBudget:                 4 * time.Minute,
		StatementTimeout:          30 * time.Second,
		LockTimeout:               2 * time.Second,
		ControlTimeout:            2 * time.Second,
		BlockedRetryInterval:      time.Hour,
		FailedRetryInterval:       time.Minute,
		ResumeInterval:            15 * time.Second,
		IdleInterval:              5 * time.Minute,
		TaskMaxTime:               15 * time.Minute,
	}
}

// retained clamped to the code's bounds.
func (self *ProberShardRetentionSettings) clampRetainedGenerations(retained int) int {
	return max(self.MinRetainedGenerations, min(self.MaxRetainedGenerations, retained))
}

// The first create time a retention of retained generations keeps at now: the
// start of the generation retained-1 before the current one. A pass created
// before it is in a dropped generation. Pure.
func ProberShardRetentionCutoff(now time.Time, generationDuration time.Duration, retained int) time.Time {
	current := now.UTC().Truncate(generationDuration)
	return current.Add(-time.Duration(retained-1) * generationDuration)
}

// When the pass created at createTime falls out of a retention of retained
// generations: the start of the generation retained after its own. Pure.
func ProberShardDropTime(createTime time.Time, generationDuration time.Duration, retained int) time.Time {
	return createTime.UTC().Truncate(generationDuration).Add(time.Duration(retained) * generationDuration)
}

// The control hash. A singleton key on the `{prober_retention}` slot.
const proberShardRetentionControlKey = "{prober_retention}control"

// The control's fields, all optional:
//   - `mode`: off, dry_run or on;
//   - `retainedGenerations`: generations kept, the current one included,
//     between the code's fewest and most; a value past either end counts as
//     that end;
//   - `maxContractsPerSecond`: contracts read per second, at most the code's;
//     zero is off;
//   - `maxShardsPerMinute`: passes dropped per minute, at most the code's;
//     zero is off;
//   - `contracts`: on or off, the zero-byte contract step.
//
// A field that does not parse narrows as far as it can: mode off, the most
// generations, rate zero (which is off), the contract step off.
const (
	ProberShardRetentionControlFieldMode                  = "mode"
	ProberShardRetentionControlFieldRetainedGenerations   = "retainedGenerations"
	ProberShardRetentionControlFieldMaxContractsPerSecond = "maxContractsPerSecond"
	ProberShardRetentionControlFieldMaxShardsPerMinute    = "maxShardsPerMinute"
	ProberShardRetentionControlFieldContracts             = "contracts"
)

// The effective control: the code settings as the Redis control narrowed them.
type ProberShardRetentionControl struct {
	// The control key was present.
	Present               bool
	Mode                  ProberShardRetentionMode
	RetainedGenerations   int
	MaxContractsPerSecond float64
	MaxShardsPerMinute    float64
	DropContracts         bool
}

// The control the fields narrow the settings to. Pure: fields is the control
// hash as read, empty when the key is absent.
func NarrowProberShardRetentionControl(fields map[string]string, settings *ProberShardRetentionSettings) *ProberShardRetentionControl {
	control := &ProberShardRetentionControl{
		Present:               0 < len(fields),
		Mode:                  settings.DefaultMode,
		RetainedGenerations:   settings.clampRetainedGenerations(settings.RetainedGenerations),
		MaxContractsPerSecond: min(settings.DefaultMaxContractsPerSecond, settings.MaxContractsPerSecond),
		MaxShardsPerMinute:    min(settings.DefaultMaxShardsPerMinute, settings.MaxShardsPerMinute),
		DropContracts:         settings.DropContracts,
	}
	if value, ok := fields[ProberShardRetentionControlFieldMode]; ok {
		// a value that does not parse is off
		control.Mode, _ = ParseProberShardRetentionMode(value)
	}
	if value, ok := fields[ProberShardRetentionControlFieldRetainedGenerations]; ok {
		retained, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			// keeps the most
			retained = settings.MaxRetainedGenerations
		}
		control.RetainedGenerations = settings.clampRetainedGenerations(retained)
	}
	if value, ok := fields[ProberShardRetentionControlFieldMaxContractsPerSecond]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(rate) {
			control.MaxContractsPerSecond = min(settings.MaxContractsPerSecond, rate)
		} else {
			control.MaxContractsPerSecond = 0
		}
	}
	if value, ok := fields[ProberShardRetentionControlFieldMaxShardsPerMinute]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(rate) {
			control.MaxShardsPerMinute = min(settings.MaxShardsPerMinute, rate)
		} else {
			control.MaxShardsPerMinute = 0
		}
	}
	if value, ok := fields[ProberShardRetentionControlFieldContracts]; ok {
		control.DropContracts = settings.DropContracts && strings.TrimSpace(value) == "on"
	}
	if !settings.Enable || !(0 < control.MaxContractsPerSecond) || !(0 < control.MaxShardsPerMinute) {
		control.Mode = ProberShardRetentionModeOff
	}
	return control
}

// Reads the control and narrows the settings with it. One bounded Redis read,
// never retried. On an error the caller acts as if the mode were off.
func GetProberShardRetentionControl(ctx context.Context, settings *ProberShardRetentionSettings) (control *ProberShardRetentionControl, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.ControlTimeout)
	defer cancel()
	var fields map[string]string
	returnErr = server.RedisWithDeadline(readCtx, func(r server.RedisClient) error {
		var err error
		fields, err = r.HGetAll(readCtx, proberShardRetentionControlKey).Result()
		return err
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return nil, returnErr
	}
	return NarrowProberShardRetentionControl(fields, settings), nil
}

// The generations kept now: the control's, or the code's when the control
// cannot be read. Only for readers that delete nothing.
func proberShardRetainedGenerations(ctx context.Context, settings *ProberShardRetentionSettings) int {
	control, err := GetProberShardRetentionControl(ctx, settings)
	if err != nil {
		return settings.clampRetainedGenerations(settings.RetainedGenerations)
	}
	return control.RetainedGenerations
}

// Sets control fields, keeping the others. For operator tools and tests.
// ctx must carry a deadline.
func SetProberShardRetentionControl(ctx context.Context, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	values := make([]any, 0, 2*len(fields))
	for field, value := range fields {
		values = append(values, field, value)
	}
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.HSet(ctx, proberShardRetentionControlKey, values...).Err()
	}, server.OptRedisNoPreflight())
}

// Deletes the control, so the code settings apply. ctx must carry a deadline.
func RemoveProberShardRetentionControl(ctx context.Context) error {
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.Del(ctx, proberShardRetentionControlKey).Err()
	}, server.OptRedisNoPreflight())
}

// Why a run ended.
const (
	// no pass of a dropped generation is due
	proberShardRetentionStopCaughtUp = "caught_up"
	// the run spent its budget
	proberShardRetentionStopBudget = "budget"
	// the mode is off
	proberShardRetentionStopOff = "off"
	// a dry run read its one page
	proberShardRetentionStopDryRun = "dry_run"
	// the control could not be read
	proberShardRetentionStopControl = "control_unavailable"
	// the task context ended
	proberShardRetentionStopCanceled = "canceled"
	// a statement failed
	proberShardRetentionStopError = "error"
)

// What happened to a pass a run took up.
const (
	// its zero-byte contracts, identity and registry row are gone
	proberShardRetentionShardDropped = "dropped"
	// the identity transaction refused it; retried after BlockedRetryInterval
	proberShardRetentionShardBlocked = "blocked"
	// a step failed; retried by the next run
	proberShardRetentionShardError = "error"
	// the run's budget ended inside its contract step; the next run resumes it
	proberShardRetentionShardDeferred = "deferred"
	// its registry row went before this run reached it: another run dropped it
	proberShardRetentionShardGone = "gone"
	// a dry run read it
	proberShardRetentionShardDryRun = "dry_run"
)

// What happened to a contract the contract step read.
const (
	// deleted with its rows
	proberShardRetentionContractDeleted = "deleted"
	// it carries bytes or has a sweep: the contract horizon reaper's
	proberShardRetentionContractKeptBytes = "kept_bytes"
	// an escrow is not settled
	proberShardRetentionContractKeptUnsettled = "kept_unsettled"
	// another transaction holds it, or it changed since it was read
	proberShardRetentionContractBusy = "busy"
	// a dry run would delete it
	proberShardRetentionContractEligible = "eligible_dry_run"
)

// The tables a drop deletes from, contracts first.
var proberShardRetentionTables = []string{
	"contract_close_report",
	"contract_close",
	"transfer_escrow",
	"transfer_escrow_sweep",
	"contract_extender",
	"transfer_contract",
	"client_tls_certificate",
	"network_client_role",
	"network_client_provider_intent",
	"network_client_connection",
	"network_client_location_reliability",
	"provide_key",
	"network_client",
	"device",
	"transfer_balance",
	"network",
	"network_user_auth_seedphrase",
	"network_user",
	"prober_shard_run",
}

var (
	proberShardRetentionModeGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_prober_retention_mode",
		Help: "The prober shard retention's mode at its last control read: 0 off, 1 dry run, 2 on",
	})
	proberShardRetentionGenerationsGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_prober_retention_retained_generations",
		Help: "Prober shard generations (UTC days of passes) the retention keeps, the current one included, at its last control read",
	})
	proberShardRetentionCutoffGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_prober_retention_cutoff_timestamp_seconds",
		Help: "The first pass create time the prober shard retention keeps; passes created before it are in dropped generations",
	})
	proberShardRetentionOldestGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_prober_retention_oldest_shard_timestamp_seconds",
		Help: "Create time of the oldest prober shard pass still registered and not closed; within a generation of the cutoff while the retention keeps up",
	})
	proberShardRetentionDueGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_prober_retention_due_shards",
		Help: "Registered prober shard passes not closed in dropped generations, at the start of the last run: the retention's backlog",
	})
	proberShardRetentionRunTimestampGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_prober_retention_run_timestamp_seconds",
		Help: "The clock of the last prober shard retention run this process made that read its control and its backlog: the run's start. Each taskworker keeps its own last run's gauges, so readers take the process with the newest clock",
	})
	proberShardRetentionShardsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_shards_total",
		Help: "Prober shard passes of dropped generations the retention took up, by outcome",
	}, []string{"outcome"})
	proberShardRetentionBlockedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_blocked_shards_total",
		Help: "Prober shard passes whose identity drop was refused, by the first reason found",
	}, []string{"reason"})
	proberShardRetentionContractsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_contracts_total",
		Help: "Resolved contracts of dropped prober shard passes the retention read, by outcome",
	}, []string{"outcome"})
	proberShardRetentionArchivedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_archived_contracts_total",
		Help: "Credit-bearing contracts the prober shard retention deleted, each archived to st_provider_usage_archive",
	})
	proberShardRetentionDeletedRowsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_deleted_rows_total",
		Help: "Rows the prober shard retention deleted, by table",
	}, []string{"table"})
	proberShardRetentionBatchDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "urnetwork_prober_retention_batch_duration_seconds",
		Help:    "Duration of one prober shard retention step, by step and outcome",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 18, 30, 60},
	}, []string{"step", "outcome"})
	proberShardRetentionRunCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_runs_total",
		Help: "Prober shard retention runs, by why they ended",
	}, []string{"stop"})
)

func init() {
	for _, outcome := range []string{
		proberShardRetentionShardDropped,
		proberShardRetentionShardBlocked,
		proberShardRetentionShardError,
		proberShardRetentionShardDeferred,
		proberShardRetentionShardGone,
		proberShardRetentionShardDryRun,
	} {
		proberShardRetentionShardsCounter.WithLabelValues(outcome)
	}
	for _, reason := range proberShardReapBlockReasons {
		proberShardRetentionBlockedCounter.WithLabelValues(reason)
	}
	for _, outcome := range []string{
		proberShardRetentionContractDeleted,
		proberShardRetentionContractKeptBytes,
		proberShardRetentionContractKeptUnsettled,
		proberShardRetentionContractBusy,
		proberShardRetentionContractEligible,
	} {
		proberShardRetentionContractsCounter.WithLabelValues(outcome)
	}
	for _, table := range proberShardRetentionTables {
		proberShardRetentionDeletedRowsCounter.WithLabelValues(table)
	}
	for _, step := range []string{"select", "contracts", "identity", "registry"} {
		for _, outcome := range []string{"ok", "error"} {
			proberShardRetentionBatchDuration.WithLabelValues(step, outcome)
		}
	}
	for _, stop := range []string{
		proberShardRetentionStopCaughtUp,
		proberShardRetentionStopBudget,
		proberShardRetentionStopOff,
		proberShardRetentionStopDryRun,
		proberShardRetentionStopControl,
		proberShardRetentionStopCanceled,
		proberShardRetentionStopError,
	} {
		proberShardRetentionRunCounter.WithLabelValues(stop)
	}
	prometheus.MustRegister(
		proberShardRetentionModeGauge,
		proberShardRetentionGenerationsGauge,
		proberShardRetentionCutoffGauge,
		proberShardRetentionOldestGauge,
		proberShardRetentionDueGauge,
		proberShardRetentionRunTimestampGauge,
		proberShardRetentionShardsCounter,
		proberShardRetentionBlockedCounter,
		proberShardRetentionContractsCounter,
		proberShardRetentionArchivedCounter,
		proberShardRetentionDeletedRowsCounter,
		proberShardRetentionBatchDuration,
		proberShardRetentionRunCounter,
	)
}

// What one run did.
type ProberShardRetentionReport struct {
	// The mode of the last control read.
	Mode ProberShardRetentionMode
	// The generations kept, and the first pass create time kept, at the last
	// control read.
	RetainedGenerations int
	Cutoff              time.Time
	// Passes taken up, by outcome; refused passes by reason.
	Shards  map[string]int64
	Blocked map[string]int64
	// Contracts read by the contract step, by outcome.
	Contracts map[string]int64
	// Credit-bearing contracts deleted, each archived by the capture trigger.
	Archived int64
	// Rows deleted, by table.
	Deleted map[string]int64
	// Due passes and the oldest pass's create time at the run's start.
	DueShards   int64
	OldestShard *time.Time
	// No pass of a dropped generation was due.
	CaughtUp bool
	// Why the run ended.
	Stop string
}

func newProberShardRetentionReport() *ProberShardRetentionReport {
	return &ProberShardRetentionReport{
		Shards:    map[string]int64{},
		Blocked:   map[string]int64{},
		Contracts: map[string]int64{},
		Deleted:   map[string]int64{},
		// every return sets its stop; a panic counts as an error
		Stop: proberShardRetentionStopError,
	}
}

// The run found nothing it may do: the drop is off or its control unreadable.
func (self *ProberShardRetentionReport) Idle() bool {
	return self.Stop == proberShardRetentionStopOff || self.Stop == proberShardRetentionStopControl
}

func (self *ProberShardRetentionReport) addShard(outcome string) {
	self.Shards[outcome] += 1
	proberShardRetentionShardsCounter.WithLabelValues(outcome).Inc()
}

func (self *ProberShardRetentionReport) addBlocked(reason string) {
	self.Blocked[reason] += 1
	proberShardRetentionBlockedCounter.WithLabelValues(reason).Inc()
}

func (self *ProberShardRetentionReport) addContracts(outcome string, count int64) {
	if count == 0 {
		return
	}
	self.Contracts[outcome] += count
	proberShardRetentionContractsCounter.WithLabelValues(outcome).Add(float64(count))
}

func (self *ProberShardRetentionReport) addDeleted(table string, count int64) {
	if count == 0 {
		return
	}
	self.Deleted[table] += count
	proberShardRetentionDeletedRowsCounter.WithLabelValues(table).Add(float64(count))
}

// One run: drops the passes of dropped generations, oldest first, until none
// is due, the budget ends, the mode turns off or ctx ends. A failed pass is
// counted and left for the next run; the run goes on to the next pass. A run
// whose control read fails stops before it reads anything.
func RemoveProberShardGenerations(
	ctx context.Context,
	now time.Time,
	settings *ProberShardRetentionSettings,
) (report *ProberShardRetentionReport, returnErr error) {
	run := &proberShardRetentionRun{
		settings: settings,
		now:      now.UTC(),
		runEnd:   time.Now().Add(settings.RunBudget),
		report:   newProberShardRetentionReport(),
	}
	report = run.report
	defer func() {
		proberShardRetentionRunCounter.WithLabelValues(report.Stop).Inc()
		if glog.V(1) {
			glog.Infof(
				"[prober_retention]run mode=%s stop=%s retained=%d cutoff=%s due=%d shards=%v blocked=%v contracts=%v archived=%d deleted=%v\n",
				report.Mode,
				report.Stop,
				report.RetainedGenerations,
				report.Cutoff.Format(time.RFC3339),
				report.DueShards,
				report.Shards,
				report.Blocked,
				report.Contracts,
				report.Archived,
				report.Deleted,
			)
		}
	}()

	control, ok := run.readControl(ctx)
	if !ok {
		return
	}
	if err := run.observeBacklog(ctx); err != nil {
		report.Stop = proberShardRetentionStopError
		returnErr = err
		return
	}
	if control.Mode == ProberShardRetentionModeOff {
		report.Stop = proberShardRetentionStopOff
		return
	}

	for {
		if stop, ok := run.stopped(ctx); ok {
			report.Stop = stop
			return
		}
		dryRun := control.Mode != ProberShardRetentionModeOn
		owners, err := run.selectDueShards(ctx, dryRun)
		if err != nil {
			if ctx.Err() != nil {
				report.Stop = proberShardRetentionStopCanceled
				return
			}
			report.Stop = proberShardRetentionStopError
			returnErr = err
			return
		}
		if len(owners) == 0 {
			if !dryRun {
				if err := run.removeClosedRegistryRows(ctx); err != nil {
					if ctx.Err() != nil {
						report.Stop = proberShardRetentionStopCanceled
						return
					}
					report.Stop = proberShardRetentionStopError
					returnErr = err
					return
				}
			}
			report.CaughtUp = true
			report.Stop = proberShardRetentionStopCaughtUp
			return
		}
		for _, owner := range owners {
			if stop, ok := run.stopped(ctx); ok {
				report.Stop = stop
				return
			}
			control, ok = run.readControl(ctx)
			if !ok {
				return
			}
			if control.Mode == ProberShardRetentionModeOff {
				report.Stop = proberShardRetentionStopOff
				return
			}
			if control.Mode != ProberShardRetentionModeOn {
				run.dryRunShard(ctx, owner, control)
				continue
			}
			shardStart := time.Now()
			run.dropShard(ctx, owner, control)
			paced := time.Duration(float64(time.Minute) / control.MaxShardsPerMinute)
			if !run.pace(ctx, paced-time.Since(shardStart)) {
				report.Stop = proberShardRetentionStopCanceled
				return
			}
		}
		if dryRun {
			// a dry run changes nothing, so its next page would be this one
			report.Stop = proberShardRetentionStopDryRun
			return
		}
	}
}

// do's raise as an error, nil when do returned. The raise is reported as
// server.HandleError reports any.
func proberShardRetentionRecover(do func()) (returnErr error) {
	server.HandleError(do, func(err error) {
		returnErr = err
	})
	return
}

// One run's clock, cutoff, budget and report.
type proberShardRetentionRun struct {
	settings *ProberShardRetentionSettings
	// The run's now, which the cutoff is measured back from.
	now    time.Time
	cutoff time.Time
	runEnd time.Time
	report *ProberShardRetentionReport
}

// Reads the control and applies it to the run. False when the run must stop.
func (self *proberShardRetentionRun) readControl(ctx context.Context) (*ProberShardRetentionControl, bool) {
	control, err := GetProberShardRetentionControl(ctx, self.settings)
	if err != nil {
		self.report.Stop = proberShardRetentionStopControl
		glog.V(1).Infof("[prober_retention]control unavailable, stopping: %s\n", err)
		return nil, false
	}
	self.cutoff = ProberShardRetentionCutoff(self.now, self.settings.GenerationDuration, control.RetainedGenerations)
	self.report.Mode = control.Mode
	self.report.RetainedGenerations = control.RetainedGenerations
	self.report.Cutoff = self.cutoff
	proberShardRetentionModeGauge.Set(float64(control.Mode))
	proberShardRetentionGenerationsGauge.Set(float64(control.RetainedGenerations))
	proberShardRetentionCutoffGauge.Set(float64(self.cutoff.Unix()))
	return control, true
}

// The run's stop, if ctx ended or the budget is spent.
func (self *proberShardRetentionRun) stopped(ctx context.Context) (string, bool) {
	select {
	case <-ctx.Done():
		return proberShardRetentionStopCanceled, true
	default:
	}
	if !time.Now().Before(self.runEnd) {
		return proberShardRetentionStopBudget, true
	}
	return "", false
}

// Publishes the backlog: the passes not closed before the cutoff, and the
// oldest pass not closed. The registry holds about a week of passes, so this
// is one small read a run.
func (self *proberShardRetentionRun) observeBacklog(ctx context.Context) error {
	var due int64
	var oldest *time.Time
	err := proberShardRetentionRecover(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE create_time < $1), min(create_time)
				FROM prober_shard_run
				WHERE state <> 'closed'
			`, self.cutoff).Scan(&due, &oldest))
		})
	})
	if err != nil {
		return err
	}
	self.publishBacklog(due, oldest)
	return nil
}

// Reports and publishes the backlog the run read, then the run's clock. The
// clock goes last and only with a backlog, so a reader that takes the process
// with the newest clock reads that run's backlog: a run whose backlog read
// failed leaves the process's previous clock, and its previous backlog with
// it. oldest is nil when every pass is closed; the oldest is then the run's
// clock.
func (self *proberShardRetentionRun) publishBacklog(due int64, oldest *time.Time) {
	self.report.DueShards = due
	self.report.OldestShard = oldest
	proberShardRetentionDueGauge.Set(float64(due))
	if oldest != nil {
		proberShardRetentionOldestGauge.Set(float64(oldest.Unix()))
	} else {
		proberShardRetentionOldestGauge.Set(float64(self.now.Unix()))
	}
	proberShardRetentionRunTimestampGauge.Set(float64(self.now.Unix()))
}

// The due passes of dropped generations, oldest first: not closed, created
// before the cutoff, past their deadline by MinDeadlineAge, and not waiting
// out a refusal or a failure (next_cleanup_time after the run's clock) unless
// settled ('infinity'). In on mode they are locked, skipping any another worker
// holds, and their next cleanup moves FailedRetryInterval past the run's clock,
// so neither this run nor a crashed one takes them up again at once. The run's
// clock is the authority for every comparison and postponement here: in
// production it is the database's within a second. $1 the cutoff, $2 the
// deadline bound, $3 the run's clock, $4 the page size.
const proberShardRetentionDueSqlFormat = `
	SELECT ` + proberShardColumns + `
	FROM prober_shard_run
	WHERE
		state <> 'closed' AND
		create_time < $1 AND
		deadline < $2 AND
		(next_cleanup_time <= $3 OR next_cleanup_time = 'infinity')
	ORDER BY create_time, task_id, epoch
	LIMIT $4
	%s
`

var proberShardRetentionDueSql = fmt.Sprintf(proberShardRetentionDueSqlFormat, "")

var proberShardRetentionDueLockSql = fmt.Sprintf(proberShardRetentionDueSqlFormat, "FOR UPDATE SKIP LOCKED")

func (self *proberShardRetentionRun) selectDueShards(ctx context.Context, dryRun bool) (owners []*ProberShardOwner, returnErr error) {
	start := time.Now()
	defer func() {
		outcome := "ok"
		if returnErr != nil {
			outcome = "error"
		}
		proberShardRetentionBatchDuration.WithLabelValues("select", outcome).Observe(time.Since(start).Seconds())
	}()
	deadlineBound := self.now.Add(-self.settings.MinDeadlineAge)
	returnErr = proberShardRetentionRecover(func() {
		if dryRun {
			server.Db(ctx, func(conn server.PgConn) {
				owners = nil
				rows, err := conn.Query(ctx, proberShardRetentionDueSql, self.cutoff, deadlineBound, self.now, self.settings.ShardPageSize)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						owner, err := scanProberShard(rows)
						server.Raise(err)
						owners = append(owners, owner)
					}
				})
			})
			return
		}
		server.Tx(ctx, func(tx server.PgTx) {
			owners = nil
			rows, err := tx.Query(ctx, proberShardRetentionDueLockSql, self.cutoff, deadlineBound, self.now, self.settings.ShardPageSize)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					owner, err := scanProberShard(rows)
					server.Raise(err)
					owners = append(owners, owner)
				}
			})
			retryTime := self.now.Add(self.settings.FailedRetryInterval)
			server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
				for _, owner := range owners {
					batch.Queue(`UPDATE prober_shard_run SET next_cleanup_time=$3 WHERE task_id=$1 AND epoch=$2`, owner.Key.TaskId, owner.Key.Epoch, retryTime)
				}
			})
		}, server.TxReadCommitted, server.OptNoRetry())
	})
	return
}

// Drops one pass: its zero-byte contracts, then its identity, then its
// registry row. A refused identity drop leaves the pass for
// BlockedRetryInterval and a failed step for FailedRetryInterval, both after
// the run's clock.
func (self *proberShardRetentionRun) dropShard(ctx context.Context, owner *ProberShardOwner, control *ProberShardRetentionControl) {
	outcome := proberShardRetentionShardError
	defer func() {
		self.report.addShard(outcome)
	}()
	fail := func(step string, err error) {
		glog.Infof("[prober_retention]shard %d task %s epoch %s %s: %s\n", owner.Key.ShardIndex, owner.Key.TaskId, owner.Key.Epoch, step, err)
		// a step may have stamped the database clock; keep the retry after
		// the run's clock, so this run does not take the pass up again
		if postponeErr := self.postponeShard(context.WithoutCancel(ctx), owner.Key, self.settings.FailedRetryInterval); postponeErr != nil {
			glog.Infof("[prober_retention]shard %s postpone: %s\n", owner.Key.Epoch, postponeErr)
		}
	}

	// a pass that crashed is still live: fence it before anything is read
	if err := DrainProberShard(ctx, owner.Key); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = proberShardRetentionShardGone
			return
		}
		fail("drain", err)
		return
	}
	if err := settleReportedProberShardContracts(ctx, owner.Key); err != nil {
		fail("settle", err)
		return
	}
	if control.DropContracts {
		complete, err := self.dropShardContracts(ctx, owner, control)
		if err != nil {
			fail("contracts", err)
			return
		}
		if !complete {
			// its identity waits for the rest of its contracts: once its clients
			// are gone, they cannot be found
			outcome = proberShardRetentionShardDeferred
			return
		}
	}

	start := time.Now()
	reap, err := reapProberShard(ctx, owner.Key)
	if err != nil {
		proberShardRetentionBatchDuration.WithLabelValues("identity", "error").Observe(time.Since(start).Seconds())
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = proberShardRetentionShardGone
			return
		}
		fail("identity", err)
		return
	}
	proberShardRetentionBatchDuration.WithLabelValues("identity", "ok").Observe(time.Since(start).Seconds())
	for table, count := range reap.deleted {
		self.report.addDeleted(table, count)
	}
	if !reap.closed {
		if reap.blocked != "" {
			self.report.addBlocked(reap.blocked)
		}
		if err := self.postponeShard(ctx, owner.Key, self.settings.BlockedRetryInterval); err != nil {
			glog.Infof("[prober_retention]shard %s postpone: %s\n", owner.Key.Epoch, err)
			return
		}
		outcome = proberShardRetentionShardBlocked
		return
	}

	removed, err := self.removeRegistryRow(ctx, owner.Key)
	if err != nil {
		fail("registry", err)
		return
	}
	self.report.addDeleted("prober_shard_run", removed)
	outcome = proberShardRetentionShardDropped
}

// The pass's next cleanup, interval after the run's clock.
func (self *proberShardRetentionRun) postponeShard(ctx context.Context, key ProberShardKey, interval time.Duration) error {
	return proberShardRetentionRecover(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE prober_shard_run
				SET next_cleanup_time=$3
				WHERE task_id=$1 AND epoch=$2 AND state<>'closed'`, key.TaskId, key.Epoch, self.now.Add(interval)))
		}, server.TxReadCommitted, server.OptNoRetry())
	})
}

// The pass's registry row, once ReapProberShard closed it. A closed row fences
// a replay of its task epoch, and an epoch is never replayed after its
// execution ends: by its generation's drop that is days past.
func (self *proberShardRetentionRun) removeRegistryRow(ctx context.Context, key ProberShardKey) (removed int64, returnErr error) {
	start := time.Now()
	returnErr = proberShardRetentionRecover(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(ctx, `DELETE FROM prober_shard_run WHERE task_id=$1 AND epoch=$2 AND state='closed' AND create_time < $3`, key.TaskId, key.Epoch, self.cutoff))
			removed = tag.RowsAffected()
		}, server.TxReadCommitted, server.OptNoRetry())
	})
	outcome := "ok"
	if returnErr != nil {
		outcome = "error"
	}
	proberShardRetentionBatchDuration.WithLabelValues("registry", outcome).Observe(time.Since(start).Seconds())
	return
}

// The closed registry rows of dropped generations that an earlier cleanup
// closed, in bounded batches until none is left or the budget ends.
func (self *proberShardRetentionRun) removeClosedRegistryRows(ctx context.Context) error {
	for {
		if _, ok := self.stopped(ctx); ok {
			return nil
		}
		start := time.Now()
		var removed int64
		err := proberShardRetentionRecover(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				tag := server.RaisePgResult(tx.Exec(ctx, `
					DELETE FROM prober_shard_run
					WHERE (task_id, epoch) IN (
						SELECT task_id, epoch FROM prober_shard_run
						WHERE state = 'closed' AND create_time < $1
						LIMIT $2
					) AND state = 'closed' AND create_time < $1
				`, self.cutoff, self.settings.RegistryBatchSize))
				removed = tag.RowsAffected()
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		proberShardRetentionBatchDuration.WithLabelValues("registry", outcome).Observe(time.Since(start).Seconds())
		if err != nil {
			return err
		}
		self.report.addDeleted("prober_shard_run", removed)
		if removed < int64(self.settings.RegistryBatchSize) {
			return nil
		}
	}
}

// The clients whose contracts belong to the pass: every client of its network
// and, once its identity transaction committed, the retired cohort it kept.
const proberShardRetentionClientsSql = `
	SELECT client_id FROM network_client WHERE network_id = $1
	UNION
	SELECT unnest($2::uuid[])
`

// The next batch of the pass's resolved contracts after the cursor, closed
// before the cutoff, in id order: the origins of its clients, from the closed
// range of each client in transfer_contract_open_source_id, and the reply
// companions to them, from the destination index. Each arm reads one client's
// own contracts behind OFFSET 0 and tests the payer, outcome and close time
// outside it: inside, the payer test would let the planner read the payer's
// whole history from transfer_contract_open_payer_network_id_transfer_byte_count
// once per client, the read and the index this retention replaces. The payer
// test keeps the batch inside the pass's own contracts. $1 the clients, $2 the
// pass's network, $3 the cutoff, $4 the cursor, $5 the batch size.
const proberShardRetentionContractWindowSql = `
	WITH client AS MATERIALIZED (
		SELECT DISTINCT client_id FROM unnest($1::uuid[]) AS client(client_id)
	), contract AS MATERIALIZED (
		SELECT origin.contract_id
		FROM client
		CROSS JOIN LATERAL (
			SELECT contract_id, payer_network_id, outcome, close_time
			FROM transfer_contract
			WHERE open = false AND source_id = client.client_id
			OFFSET 0
		) AS origin
		WHERE
			origin.payer_network_id = $2 AND
			origin.outcome IS NOT NULL AND
			origin.close_time < $3 AND
			$4 < origin.contract_id
		UNION
		SELECT companion.contract_id
		FROM client
		CROSS JOIN LATERAL (
			SELECT contract_id, payer_network_id, outcome, close_time
			FROM transfer_contract
			WHERE destination_id = client.client_id
			OFFSET 0
		) AS companion
		WHERE
			companion.payer_network_id = $2 AND
			companion.outcome IS NOT NULL AND
			companion.close_time < $3 AND
			$4 < companion.contract_id
	)
	SELECT ARRAY(SELECT contract_id FROM contract ORDER BY contract_id LIMIT $5)
`

// The statements after the window name their rows by the window's ids; a
// bitmap reads each heap page once, as the contract horizon reaper does
// (contractHorizonBitmapSettings).
var proberShardRetentionBitmapSettings = contractHorizonBitmapSettings

// Classifies the window's contracts after locking them, skipping any another
// transaction holds and rereading each row's latest version. A contract is
// deleted only when it still matches the window, carries no destination
// bytes, has no sweep and every escrow is settled. Closes and close reports are
// written only under the contract lock, and sweeps only by the settlement that
// set the outcome, so the classification holds until commit. $1 the window,
// $2 the pass's network, $3 the cutoff, $4 the destination party.
const proberShardRetentionClassifySqlFormat = `
	WITH locked AS MATERIALIZED (
		SELECT contract_id
		FROM transfer_contract
		WHERE
			contract_id = ANY($1::uuid[]) AND
			payer_network_id = $2 AND
			outcome IS NOT NULL AND
			close_time < $3
		%s
	), classified AS MATERIALIZED (
		SELECT
			locked.contract_id,
			EXISTS (
				SELECT 1 FROM contract_close
				WHERE
					contract_close.contract_id = locked.contract_id AND
					contract_close.party = $4 AND
					contract_close.used_transfer_byte_count <> 0
			) OR EXISTS (
				SELECT 1 FROM transfer_escrow_sweep
				WHERE transfer_escrow_sweep.contract_id = locked.contract_id
			) AS carries_bytes,
			EXISTS (
				SELECT 1 FROM transfer_escrow
				WHERE
					transfer_escrow.contract_id = locked.contract_id AND
					transfer_escrow.settled IS NOT TRUE
			) AS unsettled
		FROM locked
	)
	SELECT
		(SELECT count(*) FROM locked),
		(SELECT count(*) FROM classified WHERE carries_bytes),
		(SELECT count(*) FROM classified WHERE NOT carries_bytes AND unsettled),
		ARRAY(
			SELECT contract_id FROM classified
			WHERE NOT carries_bytes AND NOT unsettled
			ORDER BY contract_id
		)
`

var proberShardRetentionClassifySql = fmt.Sprintf(proberShardRetentionClassifySqlFormat, "")

var proberShardRetentionClassifyLockSql = fmt.Sprintf(proberShardRetentionClassifySqlFormat, "FOR UPDATE OF transfer_contract SKIP LOCKED")

var errProberShardRetentionDeleteChanged = errors.New("prober shard retention deleted a different number of contracts than it locked")

// The pass's zero-byte contracts, in batches paced to the control's rate. A
// batch is one short transaction: the window read, the locked classification
// and the deletes. The cursor moves past every contract a batch read, kept or
// deleted, so a pass is read once. complete is false when the run's budget or
// ctx ended first; the next run reads the pass again from its start.
func (self *proberShardRetentionRun) dropShardContracts(ctx context.Context, owner *ProberShardOwner, control *ProberShardRetentionControl) (complete bool, returnErr error) {
	clientIds, err := self.shardClients(ctx, owner)
	if err != nil {
		return false, err
	}
	for chunk := range slices.Chunk(clientIds, self.settings.ClientChunkSize) {
		cursor := server.Id{}
		for {
			if _, ok := self.stopped(ctx); ok {
				return false, ctx.Err()
			}
			start := time.Now()
			batch, err := self.removeContractBatch(ctx, owner, chunk, cursor, false)
			duration := time.Since(start)
			if err != nil {
				proberShardRetentionBatchDuration.WithLabelValues("contracts", "error").Observe(duration.Seconds())
				return false, err
			}
			proberShardRetentionBatchDuration.WithLabelValues("contracts", "ok").Observe(duration.Seconds())
			self.report.addContracts(proberShardRetentionContractDeleted, batch.deleted["transfer_contract"])
			self.report.addContracts(proberShardRetentionContractKeptBytes, batch.keptBytes)
			self.report.addContracts(proberShardRetentionContractKeptUnsettled, batch.keptUnsettled)
			self.report.addContracts(proberShardRetentionContractBusy, batch.busy)
			for _, table := range proberShardRetentionTables {
				self.report.addDeleted(table, batch.deleted[table])
			}
			self.report.Archived += batch.archived
			proberShardRetentionArchivedCounter.Add(float64(batch.archived))
			if batch.read < int64(self.settings.ContractBatchSize) {
				break
			}
			cursor = batch.lastId
			if !self.pace(ctx, contractHorizonPaceDelay(batch.read, control.MaxContractsPerSecond, duration)) {
				return false, ctx.Err()
			}
		}
	}
	return true, nil
}

// Waits delay, or until the run budget ends. False when ctx ended first.
func (self *proberShardRetentionRun) pace(ctx context.Context, delay time.Duration) bool {
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

func (self *proberShardRetentionRun) shardClients(ctx context.Context, owner *ProberShardOwner) (clientIds []server.Id, returnErr error) {
	returnErr = proberShardRetentionRecover(func() {
		server.Db(ctx, func(conn server.PgConn) {
			clientIds = []server.Id{}
			retired := append([]server.Id{owner.ClientId}, owner.RetiredClientIds...)
			rows, err := conn.Query(ctx, proberShardRetentionClientsSql, owner.NetworkId, retired)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var clientId server.Id
					server.Raise(rows.Scan(&clientId))
					clientIds = append(clientIds, clientId)
				}
			})
		})
	})
	slices.SortFunc(clientIds, func(a, b server.Id) int { return a.Cmp(b) })
	return
}

// One contract batch's reads and deletes.
type proberShardRetentionContractBatch struct {
	// contracts the window read, and the last of them
	read   int64
	lastId server.Id
	// contracts that carry bytes or a sweep
	keptBytes int64
	// contracts with an unsettled escrow
	keptUnsettled int64
	// contracts another transaction held or that no longer match
	busy int64
	// contracts that passed every check, deleted unless dry
	eligibleIds []server.Id
	deleted     map[string]int64
	archived    int64
}

// One batch transaction. A dry batch reads only, in a read only transaction,
// and takes no lock.
func (self *proberShardRetentionRun) removeContractBatch(
	ctx context.Context,
	owner *ProberShardOwner,
	clientIds []server.Id,
	cursor server.Id,
	dry bool,
) (batch *proberShardRetentionContractBatch, returnErr error) {
	options := []any{server.TxReadCommitted, server.TxLockTimeout(self.settings.LockTimeout)}
	if dry {
		options = append(options, pgx.ReadOnly)
	}
	returnErr = proberShardRetentionRecover(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// a retried transaction starts over
			batch = &proberShardRetentionContractBatch{deleted: map[string]int64{}}
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, self.settings.StatementTimeout.Milliseconds())))
			// plan each statement for its actual ids
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`))

			var windowIds []server.Id
			server.Raise(tx.QueryRow(
				ctx,
				proberShardRetentionContractWindowSql,
				clientIds,
				owner.NetworkId,
				self.cutoff,
				cursor,
				self.settings.ContractBatchSize,
			).Scan(&windowIds))
			batch.read = int64(len(windowIds))
			if batch.read == 0 {
				return
			}
			batch.lastId = windowIds[len(windowIds)-1]
			for _, setting := range proberShardRetentionBitmapSettings {
				server.RaisePgResult(tx.Exec(ctx, setting))
			}

			classifySql := proberShardRetentionClassifyLockSql
			if dry {
				classifySql = proberShardRetentionClassifySql
			}
			var lockedCount int64
			server.Raise(tx.QueryRow(
				ctx,
				classifySql,
				windowIds,
				owner.NetworkId,
				self.cutoff,
				ContractPartyDestination,
			).Scan(
				&lockedCount,
				&batch.keptBytes,
				&batch.keptUnsettled,
				&batch.eligibleIds,
			))
			batch.busy = batch.read - lockedCount
			if dry || len(batch.eligibleIds) == 0 {
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
				panic(errProberShardRetentionDeleteChanged)
			}
			batch.deleted["contract_close_report"] = reportCount
			batch.deleted["contract_close"] = closeCount
			batch.deleted["transfer_escrow"] = escrowCount
			batch.deleted["transfer_escrow_sweep"] = sweepCount
			batch.deleted["contract_extender"] = extenderCount
			batch.deleted["transfer_contract"] = contractCount
		}, options...)
	})
	if returnErr != nil {
		batch = nil
	}
	return
}

// A dry run's read of one due pass: what its contract step would delete and
// how many clients its identity drop would remove. Reads only.
func (self *proberShardRetentionRun) dryRunShard(ctx context.Context, owner *ProberShardOwner, control *ProberShardRetentionControl) {
	outcome := proberShardRetentionShardError
	defer func() {
		self.report.addShard(outcome)
	}()
	clientIds, err := self.shardClients(ctx, owner)
	if err != nil {
		glog.Infof("[prober_retention]dry run shard %s: %s\n", owner.Key.Epoch, err)
		return
	}
	if control.DropContracts {
		for chunk := range slices.Chunk(clientIds, self.settings.ClientChunkSize) {
			cursor := server.Id{}
			for {
				if _, ok := self.stopped(ctx); ok {
					return
				}
				batch, err := self.removeContractBatch(ctx, owner, chunk, cursor, true)
				if err != nil {
					glog.Infof("[prober_retention]dry run shard %s contracts: %s\n", owner.Key.Epoch, err)
					return
				}
				self.report.addContracts(proberShardRetentionContractEligible, int64(len(batch.eligibleIds)))
				self.report.addContracts(proberShardRetentionContractKeptBytes, batch.keptBytes)
				self.report.addContracts(proberShardRetentionContractKeptUnsettled, batch.keptUnsettled)
				if batch.read < int64(self.settings.ContractBatchSize) {
					break
				}
				cursor = batch.lastId
			}
		}
	}
	outcome = proberShardRetentionShardDryRun
}
