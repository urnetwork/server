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
// (SettleProberShard), and nothing but its window clients goes before its
// generation drops.
//
// The window clients. Each tunnel of a pass mints a derived client of the
// pass's own client and retires it when the tunnel closes: on Main about 5,500
// a pass, 6.8M a day, each retired about 16 seconds after it was minted. Once
// its pass settled, nothing reads a retired window again: its contracts carry
// their own network ids, so close, expiry, settlement and payout never read
// its row (as for a hosted proxy's windows, network_client_retired_hosted.go),
// and the location fold cannot write its row again once its client is gone.
// The window step (RemoveProberShardWindows, prober_shard_window_clients.go)
// deletes a pass's window clients and the rows keyed by them an hour after its
// deadline, once every contract it funded is resolved and every escrow of its
// balances settled; the pass's own client, identity, contracts and registry
// row stay for its generation. A window held by a connection or a hosted
// resource stays for the drop.
//
// The drop. Oldest generation first:
//  1. the generation's zero-byte contracts, by one walk of the primary key
//     (ProberShardContractWalk): resolved and closed before the cutoff, every
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
//     The walk reads the contract id range of the generation's passes, from
//     the first pass's creation to the last pass's deadline, each widened by
//     ContractIdCreateTimeMargin, once, in id order, and keeps the contracts
//     whose payer is one of the generation's pass networks: the payer of every
//     contract a pass funds, origin or reply companion
//     (validateProberShardPayerInTx). It needs no index but the primary key and
//     reads each contract of the day once, about as many rows a second as
//     contracts are created (OPEN-SOURCE-ID-RETIRE.md, option A). It replaced
//     a read of each client's closed contracts from
//     transfer_contract_open_source_id. Its cursor round trips through the
//     task args, so a walk that outlasts a run resumes where it stopped.
//  2. then, for each pass of a generation whose walk completed, its identity,
//     in ReapProberShard's one transaction, which refuses a shard with an
//     unresolved contract, an unsettled escrow, a pending debit, a connected
//     client or a balance that is not a plain grant.
//  3. and its registry row.
//
// A pass of a generation the walk has not completed waits for it: its
// contracts are found by its network, so its identity could go first, but
// its registry row names the network the walk keeps contracts by. The walk
// serves the oldest dropped generation that still has a pass to drop, so a
// pass that is not due (refused, failed or inside its deadline guard) never
// holds a newer generation's walk, and a generation whose passes all went
// without a walk (the contract step off) is never walked.
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
// code's bounds, paces the walk and the contract deletes, turns the contract
// step off, and paces or turns off the window step, without a deploy. It is
// read before every pass and every walk batch, so setting the mode off stops a
// running drop, and the window step, within one pass or batch. A run that
// cannot read it stops, as if the mode were off. The code's default mode is
// off: a deploy drops nothing until an operator sets the control.
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
	// Contracts of the generation's passes one walk batch classifies and
	// deletes, at most; a batch that reaches it resumes after its last one.
	// Non-positive turns the contract step off.
	ContractBatchSize int
	// Contracts one walk batch reads from the primary key, at most, of any
	// payer: the bound on the work one transaction examines. Non-positive
	// turns the contract step off.
	WalkBatchSize int
	// Contracts of the generation's passes the walk keeps per second while the
	// control sets no rate, and the most the control can set.
	DefaultMaxContractsPerSecond float64
	MaxContractsPerSecond        float64
	// Contracts the walk reads from the primary key per second, of any payer,
	// while the control sets no rate, and the most the control can set.
	DefaultMaxWalkRowsPerSecond float64
	MaxWalkRowsPerSecond        float64
	// Passes dropped per minute while the control sets no rate, and the most
	// the control can set. A pass's identity transaction deletes its clients'
	// rows at once, so this paces those deletes.
	DefaultMaxShardsPerMinute float64
	MaxShardsPerMinute        float64
	// The window step runs while the control does not turn it off.
	DropWindows bool
	// The window step takes a pass once its deadline is this far in the past
	// and every contract it funded is resolved.
	WindowMinDeadlineAge time.Duration
	// A pass still unresolved, or still live, this far past its deadline is
	// passed over: its window clients wait for its generation's drop. Until
	// then the window step waits at it.
	WindowSkipAge time.Duration
	// Window clients one transaction deletes, at most. Non-positive turns the
	// window step off.
	WindowBatchSize int
	// Passes whose window clients the window step deletes per minute while the
	// control sets no rate, and the most the control can set.
	DefaultMaxWindowShardsPerMinute float64
	MaxWindowShardsPerMinute        float64
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
		// about 10% of Main's contracts are a prober's: a batch keeps about 500
		WalkBatchSize: 5000,
		// about 5,500 zero-byte prober contracts a day per pass slot on Main:
		// 200 a second keeps a day's drop to about 11 hours
		DefaultMaxContractsPerSecond: 200,
		MaxContractsPerSecond:        2000,
		// a day's window on Main is about 116M contracts (1,350 created a
		// second): 3,000 a second walks it in about 11 hours
		DefaultMaxWalkRowsPerSecond: 3000,
		MaxWalkRowsPerSecond:        20000,
		// a day's 1,600 passes in under an hour; the first drop's backlog of
		// about 49,000 passes (Main, 2026-11-01) in about 27 hours
		DefaultMaxShardsPerMinute: 30,
		MaxShardsPerMinute:        600,
		DropWindows:               true,
		// a pass settles within about half an hour of its creation on Main,
		// and a contract's lifespan and close linger are about 75 minutes
		// (RetiredHostedNetworkClientReapSettings)
		WindowMinDeadlineAge: time.Hour,
		WindowSkipAge:        6 * time.Hour,
		// a pass on Main has about 5,500 window clients: one transaction
		WindowBatchSize: 10000,
		// Main creates about 1,400 passes a day, one a minute: four keep up
		// with room, at about 380 window deletes a second while behind, and
		// clear the backlog of about 39,000 passes (2026-11-17) in about nine
		// days
		DefaultMaxWindowShardsPerMinute: 4,
		MaxWindowShardsPerMinute:        120,
		ShardPageSize:                   16,
		RegistryBatchSize:               1000,
		RunBudget:                       4 * time.Minute,
		StatementTimeout:                30 * time.Second,
		LockTimeout:                     2 * time.Second,
		ControlTimeout:                  2 * time.Second,
		BlockedRetryInterval:            time.Hour,
		FailedRetryInterval:             time.Minute,
		ResumeInterval:                  15 * time.Second,
		IdleInterval:                    5 * time.Minute,
		TaskMaxTime:                     15 * time.Minute,
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
//   - `maxContractsPerSecond`: contracts of the generation's passes the walk
//     keeps per second, at most the code's; zero is off;
//   - `maxWalkRowsPerSecond`: contracts the walk reads from the primary key per
//     second, of any payer, at most the code's; zero is off;
//   - `maxShardsPerMinute`: passes dropped per minute, at most the code's;
//     zero is off;
//   - `contracts`: on or off, the zero-byte contract step;
//   - `windows`: on or off, the window step (RemoveProberShardWindows), which
//     runs in the mode the other fields set;
//   - `maxWindowShardsPerMinute`: passes whose window clients the window step
//     deletes per minute, at most the code's; zero turns the window step off.
//
// A field that does not parse narrows as far as it can: mode off, the most
// generations, rate zero (which is off), the contract step off, the window
// step off.
const (
	ProberShardRetentionControlFieldMode                     = "mode"
	ProberShardRetentionControlFieldRetainedGenerations      = "retainedGenerations"
	ProberShardRetentionControlFieldMaxContractsPerSecond    = "maxContractsPerSecond"
	ProberShardRetentionControlFieldMaxWalkRowsPerSecond     = "maxWalkRowsPerSecond"
	ProberShardRetentionControlFieldMaxShardsPerMinute       = "maxShardsPerMinute"
	ProberShardRetentionControlFieldContracts                = "contracts"
	ProberShardRetentionControlFieldWindows                  = "windows"
	ProberShardRetentionControlFieldMaxWindowShardsPerMinute = "maxWindowShardsPerMinute"
)

// The effective control: the code settings as the Redis control narrowed them.
type ProberShardRetentionControl struct {
	// The control key was present.
	Present               bool
	Mode                  ProberShardRetentionMode
	RetainedGenerations   int
	MaxContractsPerSecond float64
	MaxWalkRowsPerSecond  float64
	MaxShardsPerMinute    float64
	DropContracts         bool
	// The window step runs, in the control's mode, at most
	// MaxWindowShardsPerMinute passes a minute.
	DropWindows              bool
	MaxWindowShardsPerMinute float64
}

// The control the fields narrow the settings to. Pure: fields is the control
// hash as read, empty when the key is absent.
func NarrowProberShardRetentionControl(fields map[string]string, settings *ProberShardRetentionSettings) *ProberShardRetentionControl {
	control := &ProberShardRetentionControl{
		Present:               0 < len(fields),
		Mode:                  settings.DefaultMode,
		RetainedGenerations:   settings.clampRetainedGenerations(settings.RetainedGenerations),
		MaxContractsPerSecond: min(settings.DefaultMaxContractsPerSecond, settings.MaxContractsPerSecond),
		MaxWalkRowsPerSecond:  min(settings.DefaultMaxWalkRowsPerSecond, settings.MaxWalkRowsPerSecond),
		MaxShardsPerMinute:    min(settings.DefaultMaxShardsPerMinute, settings.MaxShardsPerMinute),
		// a batch with no room reads or keeps nothing
		DropContracts:            settings.DropContracts && 0 < settings.ContractBatchSize && 0 < settings.WalkBatchSize,
		DropWindows:              settings.DropWindows && 0 < settings.WindowBatchSize,
		MaxWindowShardsPerMinute: min(settings.DefaultMaxWindowShardsPerMinute, settings.MaxWindowShardsPerMinute),
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
	if value, ok := fields[ProberShardRetentionControlFieldMaxWalkRowsPerSecond]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(rate) {
			control.MaxWalkRowsPerSecond = min(settings.MaxWalkRowsPerSecond, rate)
		} else {
			control.MaxWalkRowsPerSecond = 0
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
		control.DropContracts = control.DropContracts && strings.TrimSpace(value) == "on"
	}
	if value, ok := fields[ProberShardRetentionControlFieldWindows]; ok {
		control.DropWindows = control.DropWindows && strings.TrimSpace(value) == "on"
	}
	if value, ok := fields[ProberShardRetentionControlFieldMaxWindowShardsPerMinute]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(rate) {
			control.MaxWindowShardsPerMinute = min(settings.MaxWindowShardsPerMinute, rate)
		} else {
			control.MaxWindowShardsPerMinute = 0
		}
	}
	// a rate of zero turns only the window step off: the drop does not read it
	if !(0 < control.MaxWindowShardsPerMinute) {
		control.DropWindows = false
	}
	if !settings.Enable || !(0 < control.MaxContractsPerSecond) || !(0 < control.MaxWalkRowsPerSecond) || !(0 < control.MaxShardsPerMinute) {
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

// Where the walk of the dropped generations' contracts resumes. It round trips
// through the task args, as the contract horizon reaper's cursor does; the
// zero value walks the oldest dropped generation that has a pass to drop from
// its start. A lost walk only reads again: it never skips a generation it did
// not complete.
type ProberShardContractWalk struct {
	// The newest generation, by its UTC start, whose walk completed. Every
	// older generation was walked or had no pass left to walk when this one
	// began. Zero before the first walk completes.
	CompleteThrough time.Time `json:"complete_through,omitzero"`
	// The generation whose walk is in progress, by its UTC start. Zero when no
	// walk is.
	Generation time.Time `json:"generation,omitzero"`
	// The contract id range [Lower, Upper) the walk reads, fixed when it
	// starts: ContractIdCreateTimeRange of the first pass's creation and the
	// last pass's deadline, which holds every contract the generation's passes
	// funded.
	Lower server.Id `json:"lower"`
	Upper server.Id `json:"upper"`
	// The last contract id the walk committed past. The zero id starts at
	// Lower.
	Cursor server.Id `json:"cursor"`
}

// The first pass create time whose generation is not walked: the end of the
// newest generation whose walk completed, or the zero time before any did.
// Pure.
func (self ProberShardContractWalk) walkedEnd(generationDuration time.Duration) time.Time {
	if self.CompleteThrough.IsZero() {
		return time.Time{}
	}
	return self.CompleteThrough.Add(generationDuration)
}

// Whether the generation of the pass created at createTime was walked. Pure.
func (self ProberShardContractWalk) walked(createTime time.Time, generationDuration time.Duration) bool {
	return createTime.Before(self.walkedEnd(generationDuration))
}

// Why a run ended.
const (
	// no pass of a dropped generation is due, and no generation is due a walk
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
	// its generation's contract walk has not completed; the pass waits for it
	proberShardRetentionShardDeferred = "deferred"
	// its registry row went before this run reached it: another run dropped it
	proberShardRetentionShardGone = "gone"
	// a dry run read it
	proberShardRetentionShardDryRun = "dry_run"
)

// What happened to a contract of the generation's passes the walk kept.
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
	proberShardRetentionWalkedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_prober_retention_walked_contracts_total",
		Help: "Contracts of any payer the prober shard retention's walk read from the primary key over a dropped generation's contract id range, dry runs included; it keeps those of the generation's passes",
	})
	proberShardRetentionWalkCursorGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_prober_retention_walk_cursor_timestamp_seconds",
		Help: "Creation time the prober shard retention's contract walk has read up to: its cursor's id millisecond, the start of a generation's range when its walk begins and the end once it completes. Each taskworker keeps its own last walk's value",
	})
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
	for _, step := range []string{"select", "contracts", "identity", "registry", proberShardRetentionStepWindows} {
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
		proberShardRetentionWalkedCounter,
		proberShardRetentionWalkCursorGauge,
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
	// Contracts of the generations' passes the walk kept, by outcome.
	Contracts map[string]int64
	// Contracts of any payer the walk read from the primary key.
	Walked int64
	// Credit-bearing contracts deleted, each archived by the capture trigger.
	Archived int64
	// Rows deleted, by table.
	Deleted map[string]int64
	// Due passes and the oldest pass's create time at the run's start.
	DueShards   int64
	OldestShard *time.Time
	// No pass of a dropped generation was due, and no generation was due a
	// walk.
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

func (self *ProberShardRetentionReport) addWalked(count int64) {
	if count == 0 {
		return
	}
	self.Walked += count
	proberShardRetentionWalkedCounter.Add(float64(count))
}

// One run: drops the due passes of dropped generations whose contract walk
// completed, oldest first; once none is due, walks the next generation's
// contracts from walk, and drops its passes when the walk completes; until
// nothing is due, the budget ends, the mode turns off or ctx ends. With the
// contract step off, every due pass drops and nothing is walked. A failed
// pass is counted and left for the next run; the run goes on to the next
// pass. A run whose control read fails stops before it reads anything. next
// is where the next run's walk resumes: every batch before a failed one
// committed, so it is also the retry's.
func RemoveProberShardGenerations(
	ctx context.Context,
	now time.Time,
	walk ProberShardContractWalk,
	settings *ProberShardRetentionSettings,
) (report *ProberShardRetentionReport, next ProberShardContractWalk, returnErr error) {
	run := &proberShardRetentionRun{
		settings: settings,
		now:      now.UTC(),
		runEnd:   time.Now().Add(settings.RunBudget),
		report:   newProberShardRetentionReport(),
		walk:     walk,
	}
	report = run.report
	defer func() {
		next = run.walk
		proberShardRetentionRunCounter.WithLabelValues(report.Stop).Inc()
		if glog.V(1) {
			glog.Infof(
				"[prober_retention]run mode=%s stop=%s retained=%d cutoff=%s due=%d shards=%v blocked=%v contracts=%v walked=%d walk=%s/%s archived=%d deleted=%v\n",
				report.Mode,
				report.Stop,
				report.RetainedGenerations,
				report.Cutoff.Format(time.RFC3339),
				report.DueShards,
				report.Shards,
				report.Blocked,
				report.Contracts,
				report.Walked,
				run.walk.Generation.Format(time.RFC3339),
				run.walk.Cursor,
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
	if control.Mode != ProberShardRetentionModeOn {
		returnErr = run.dryRun(ctx, control)
		return
	}

	// a run that stops on an error stops at the first failed read
	fail := func(err error) {
		if ctx.Err() != nil {
			report.Stop = proberShardRetentionStopCanceled
			return
		}
		report.Stop = proberShardRetentionStopError
		returnErr = err
	}
	for {
		if stop, ok := run.stopped(ctx); ok {
			report.Stop = stop
			return
		}
		owners, err := run.selectDueShards(ctx, false, run.walkedEnd(control))
		if err != nil {
			fail(err)
			return
		}
		if 0 < len(owners) {
			for _, owner := range owners {
				if stop, ok := run.stopped(ctx); ok {
					report.Stop = stop
					return
				}
				control, ok = run.readControl(ctx)
				if !ok {
					return
				}
				switch control.Mode {
				case ProberShardRetentionModeOff:
					report.Stop = proberShardRetentionStopOff
					return
				case ProberShardRetentionModeDryRun:
					// the next run reads the drop it would make
					report.Stop = proberShardRetentionStopDryRun
					return
				}
				shardStart := time.Now()
				run.dropShard(ctx, owner, control)
				paced := time.Duration(float64(time.Minute) / control.MaxShardsPerMinute)
				if !run.pace(ctx, paced-time.Since(shardStart)) {
					report.Stop = proberShardRetentionStopCanceled
					return
				}
			}
			continue
		}

		// no pass of a walked generation is due: walk the next generation
		if control.DropContracts {
			walking, _, latest, err := run.walkNextGeneration(ctx, control)
			if err != nil {
				fail(err)
				return
			}
			if latest == nil {
				// the walk set the stop
				return
			}
			control = latest
			if walking {
				// complete, its passes are due now; or the contract step
				// turned off, and every due pass drops without a walk
				continue
			}
		}
		if err := run.removeClosedRegistryRows(ctx); err != nil {
			fail(err)
			return
		}
		report.CaughtUp = true
		report.Stop = proberShardRetentionStopCaughtUp
		return
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

// One run's clock, cutoff, budget, walk and report.
type proberShardRetentionRun struct {
	settings *ProberShardRetentionSettings
	// The run's now, which the cutoff is measured back from.
	now    time.Time
	cutoff time.Time
	runEnd time.Time
	report *ProberShardRetentionReport
	// Where the walk stands, as of its last committed batch.
	walk ProberShardContractWalk
	// The pass networks of walkPassesGeneration's walk, read once a run.
	walkPassesGeneration time.Time
	walkNetworkIds       []server.Id
}

// The first create time of the passes the contract step has not walked, or
// nil when the step is off and every due pass drops without a walk.
func (self *proberShardRetentionRun) walkedEnd(control *ProberShardRetentionControl) *time.Time {
	if !control.DropContracts {
		return nil
	}
	walkedEnd := self.walk.walkedEnd(self.settings.GenerationDuration)
	return &walkedEnd
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
// production it is the database's within a second. A pass whose generation's
// contract walk has not completed is not due while the contract step runs: it
// waits for the walk without being taken up. $1 the cutoff, $2 the deadline
// bound, $3 the run's clock, $4 the page size, $5 the first create time not
// walked, or null with the contract step off.
const proberShardRetentionDueSqlFormat = `
	SELECT ` + proberShardColumns + `, create_time
	FROM prober_shard_run
	WHERE
		state <> 'closed' AND
		create_time < $1 AND
		deadline < $2 AND
		(next_cleanup_time <= $3 OR next_cleanup_time = 'infinity') AND
		($5::timestamp IS NULL OR create_time < $5::timestamp)
	ORDER BY create_time, task_id, epoch
	LIMIT $4
	%s
`

var proberShardRetentionDueSql = fmt.Sprintf(proberShardRetentionDueSqlFormat, "")

var proberShardRetentionDueLockSql = fmt.Sprintf(proberShardRetentionDueSqlFormat, "FOR UPDATE SKIP LOCKED")

// A due pass and its create time, which names its generation.
type proberShardRetentionDue struct {
	*ProberShardOwner
	createTime time.Time
}

// One due row: the pass's columns, then its create time.
func scanProberShardRetentionDue(row interface{ Scan(...any) error }) (*proberShardRetentionDue, error) {
	due := &proberShardRetentionDue{}
	owner, err := scanProberShard(proberShardRetentionDueRow{row: row, createTime: &due.createTime})
	due.ProberShardOwner = owner
	return due, err
}

// Scans the pass's columns and then its create time into createTime.
type proberShardRetentionDueRow struct {
	row        interface{ Scan(...any) error }
	createTime *time.Time
}

func (self proberShardRetentionDueRow) Scan(dest ...any) error {
	return self.row.Scan(append(dest, self.createTime)...)
}

// The due passes, the first page of them. walkedEnd bounds the create times
// the contract step has walked; nil reads every due pass.
func (self *proberShardRetentionRun) selectDueShards(ctx context.Context, dryRun bool, walkedEnd *time.Time) (owners []*proberShardRetentionDue, returnErr error) {
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
				rows, err := conn.Query(ctx, proberShardRetentionDueSql, self.cutoff, deadlineBound, self.now, self.settings.ShardPageSize, walkedEnd)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						owner, err := scanProberShardRetentionDue(rows)
						server.Raise(err)
						owners = append(owners, owner)
					}
				})
			})
			return
		}
		server.Tx(ctx, func(tx server.PgTx) {
			owners = nil
			rows, err := tx.Query(ctx, proberShardRetentionDueLockSql, self.cutoff, deadlineBound, self.now, self.settings.ShardPageSize, walkedEnd)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					owner, err := scanProberShardRetentionDue(rows)
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

// Drops one pass whose generation's contract walk completed: its identity,
// then its registry row. A refused identity drop leaves the pass for
// BlockedRetryInterval and a failed step for FailedRetryInterval, both after
// the run's clock.
func (self *proberShardRetentionRun) dropShard(ctx context.Context, owner *proberShardRetentionDue, control *ProberShardRetentionControl) {
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
	if control.DropContracts && !self.walk.walked(owner.createTime, self.settings.GenerationDuration) {
		// the contract step turned on after the page was read: its identity
		// waits for its generation's walk, which keeps contracts by the
		// network its registry row names
		outcome = proberShardRetentionShardDeferred
		return
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

// The oldest pass of the next generation due a walk: not closed, created
// before the cutoff in a generation the walk has not completed, with its
// deadline MinDeadlineAge past, as a due pass's. Null when there is none. The
// registry holds about a week of passes. $1 the cutoff, $2 the deadline bound,
// $3 the first create time not walked.
const proberShardRetentionWalkNextSql = `
	SELECT min(create_time)
	FROM prober_shard_run
	WHERE
		state <> 'closed' AND
		create_time < $1 AND
		deadline < $2 AND
		$3 <= create_time
`

// The passes a generation's walk keeps contracts of: every pass created in the
// generation, in any state. Their networks are the payers, and their creation
// and deadlines bound the contract ids. $1 and $2 the generation's start and
// end.
const proberShardRetentionWalkPassesSql = `
	SELECT network_id, create_time, deadline, state
	FROM prober_shard_run
	WHERE
		$1 <= create_time AND
		create_time < $2
`

// The walk's next contracts in id order: a range of the primary key from the
// cursor, its index entries only, of every payer. $1 the walk's lower bound,
// $2 the cursor, $3 its upper bound, $4 the batch size.
const proberShardRetentionWalkWindowSql = `
	SELECT ARRAY(
		SELECT contract_id
		FROM transfer_contract
		WHERE $1 <= contract_id AND $2 < contract_id AND contract_id < $3
		ORDER BY contract_id
		LIMIT $4
	)
`

// The window's contracts of the generation's passes, resolved and closed
// before the cutoff, in id order, at most a batch: read by the window's ids
// under the bitmap settings, so each heap page is read once. $1 the window,
// $2 the generation's pass networks, $3 the cutoff, $4 the batch size.
const proberShardRetentionWalkMatchSql = `
	SELECT ARRAY(
		SELECT contract_id
		FROM transfer_contract
		WHERE
			contract_id = ANY($1::uuid[]) AND
			payer_network_id = ANY($2::uuid[]) AND
			outcome IS NOT NULL AND
			close_time < $3
		ORDER BY contract_id
		LIMIT $4
	)
`

// The statements after the window name their rows by the window's ids; a
// bitmap reads each heap page once, as the contract horizon reaper does
// (contractHorizonBitmapSettings).
var proberShardRetentionBitmapSettings = contractHorizonBitmapSettings

// Classifies the batch's contracts after locking them, skipping any another
// transaction holds and rereading each row's latest version. A contract is
// deleted only when it still matches the batch, carries no destination
// bytes, has no sweep and every escrow is settled. Closes and close reports are
// written only under the contract lock, and sweeps only by the settlement that
// set the outcome, so the classification holds until commit. $1 the batch,
// $2 the generation's pass networks, $3 the cutoff, $4 the destination party.
const proberShardRetentionClassifySqlFormat = `
	WITH locked AS MATERIALIZED (
		SELECT contract_id
		FROM transfer_contract
		WHERE
			contract_id = ANY($1::uuid[]) AND
			payer_network_id = ANY($2::uuid[]) AND
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

// The generation the next walk serves, by its UTC start: the generation of the
// oldest pass due a walk (proberShardRetentionWalkNextSql). False when there is
// none.
func (self *proberShardRetentionRun) nextWalkGeneration(ctx context.Context) (generation time.Time, ok bool, returnErr error) {
	var first *time.Time
	returnErr = proberShardRetentionRecover(func() {
		server.Db(ctx, func(conn server.PgConn) {
			first = nil
			server.Raise(conn.QueryRow(
				ctx,
				proberShardRetentionWalkNextSql,
				self.cutoff,
				self.now.Add(-self.settings.MinDeadlineAge),
				self.walk.walkedEnd(self.settings.GenerationDuration),
			).Scan(&first))
		})
	})
	if returnErr != nil || first == nil {
		return
	}
	return first.UTC().Truncate(self.settings.GenerationDuration), true, nil
}

// The pass networks of the generation's walk, read once a run, and the
// contract id range of its passes (proberShardRetentionWalkPassesSql). guarded
// is true while a pass of the generation, not closed, is inside its deadline
// guard (MinDeadlineAge): it may still fund a contract, so the walk waits for
// it, and so do the newer generations' walks. The range is empty when the
// generation has no pass.
func (self *proberShardRetentionRun) walkPasses(ctx context.Context, generation time.Time) (networkIds []server.Id, lower server.Id, upper server.Id, guarded bool, returnErr error) {
	deadlineBound := self.now.Add(-self.settings.MinDeadlineAge)
	var first, last time.Time
	returnErr = proberShardRetentionRecover(func() {
		server.Db(ctx, func(conn server.PgConn) {
			networkIds, first, last, guarded = nil, time.Time{}, time.Time{}, false
			rows, err := conn.Query(
				ctx,
				proberShardRetentionWalkPassesSql,
				generation,
				generation.Add(self.settings.GenerationDuration),
			)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var networkId server.Id
					var createTime, deadline time.Time
					var state string
					server.Raise(rows.Scan(&networkId, &createTime, &deadline, &state))
					networkIds = append(networkIds, networkId)
					if first.IsZero() || createTime.Before(first) {
						first = createTime
					}
					if last.IsZero() || last.Before(deadline) {
						last = deadline
					}
					if state != "closed" && !deadline.Before(deadlineBound) {
						guarded = true
					}
				}
			})
		})
	})
	if returnErr != nil || guarded || len(networkIds) == 0 {
		return nil, server.Id{}, server.Id{}, guarded, returnErr
	}
	self.walkPassesGeneration = generation
	self.walkNetworkIds = networkIds
	lower, upper = ContractIdCreateTimeRange(first, last)
	return
}

// Walks the next generation due a walk from where the walk stands, in batches
// paced to the control's rates, until its walk completes, the budget ends, the
// mode or the contract step turns off, or ctx ends. walking is false when no
// generation is due a walk. latest is the control the walk read last, and nil
// when the walk stopped the run, whose stop it set. A walk the contract step
// turned off returns walking and not complete, and resumes where it stopped
// once the step is on again.
func (self *proberShardRetentionRun) walkNextGeneration(
	ctx context.Context,
	control *ProberShardRetentionControl,
) (walking bool, complete bool, latest *ProberShardRetentionControl, returnErr error) {
	latest = control
	generation, ok, err := self.nextWalkGeneration(ctx)
	if err != nil || !ok {
		return false, false, latest, err
	}
	networkIds := self.walkNetworkIds
	if !self.walkPassesGeneration.Equal(generation) || !self.walk.Generation.Equal(generation) {
		var lower, upper server.Id
		var guarded bool
		networkIds, lower, upper, guarded, err = self.walkPasses(ctx, generation)
		if err != nil || guarded {
			// a pass of the generation may still fund a contract
			return false, false, latest, err
		}
		if !self.walk.Generation.Equal(generation) {
			// a new walk, or the next generation is no longer the one in
			// progress: its passes went, or an older generation's pass is due
			self.walk = ProberShardContractWalk{
				CompleteThrough: self.walk.CompleteThrough,
				Generation:      generation,
				Lower:           lower,
				Upper:           upper,
			}
			proberShardRetentionWalkCursorGauge.Set(float64(server.ContractIdTime(lower).Unix()))
		}
	}
	finish := func() {
		completeThrough := generation
		if completeThrough.Before(self.walk.CompleteThrough) {
			completeThrough = self.walk.CompleteThrough
		}
		if self.walk.Upper != (server.Id{}) {
			proberShardRetentionWalkCursorGauge.Set(float64(server.ContractIdTime(self.walk.Upper).Unix()))
		}
		self.walk = ProberShardContractWalk{CompleteThrough: completeThrough}
	}
	if len(networkIds) == 0 {
		// its passes went between the reads: nothing is left to keep
		finish()
		return true, true, latest, nil
	}
	for {
		if stop, ok := self.stopped(ctx); ok {
			self.report.Stop = stop
			return true, false, nil, nil
		}
		latest, ok = self.readControl(ctx)
		if !ok {
			return true, false, nil, nil
		}
		switch {
		case latest.Mode == ProberShardRetentionModeOff:
			self.report.Stop = proberShardRetentionStopOff
			return true, false, nil, nil
		case latest.Mode == ProberShardRetentionModeDryRun:
			// the next run reads the drop it would make
			self.report.Stop = proberShardRetentionStopDryRun
			return true, false, nil, nil
		case !latest.DropContracts:
			return true, false, latest, nil
		}
		start := time.Now()
		batch, err := self.removeWalkBatch(ctx, self.walk, networkIds, false)
		duration := time.Since(start)
		if err != nil {
			proberShardRetentionBatchDuration.WithLabelValues("contracts", "error").Observe(duration.Seconds())
			return true, false, latest, err
		}
		proberShardRetentionBatchDuration.WithLabelValues("contracts", "ok").Observe(duration.Seconds())
		self.report.addWalked(batch.walked)
		self.report.addContracts(proberShardRetentionContractDeleted, batch.deleted["transfer_contract"])
		self.report.addContracts(proberShardRetentionContractKeptBytes, batch.keptBytes)
		self.report.addContracts(proberShardRetentionContractKeptUnsettled, batch.keptUnsettled)
		self.report.addContracts(proberShardRetentionContractBusy, batch.busy)
		for _, table := range proberShardRetentionTables {
			self.report.addDeleted(table, batch.deleted[table])
		}
		self.report.Archived += batch.archived
		proberShardRetentionArchivedCounter.Add(float64(batch.archived))
		// the batch committed: the walk moves past it
		self.walk.Cursor = batch.cursor
		if batch.complete {
			finish()
			return true, true, latest, nil
		}
		proberShardRetentionWalkCursorGauge.Set(float64(server.ContractIdTime(self.walk.Cursor).Unix()))
		delay := max(
			contractHorizonPaceDelay(batch.walked, latest.MaxWalkRowsPerSecond, duration),
			contractHorizonPaceDelay(batch.read, latest.MaxContractsPerSecond, duration),
		)
		if !self.pace(ctx, delay) {
			self.report.Stop = proberShardRetentionStopCanceled
			return true, false, nil, nil
		}
	}
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

// One walk batch's reads and deletes.
type proberShardRetentionWalkBatch struct {
	// contracts of any payer the window read from the primary key
	walked int64
	// where the next batch starts: the last contract read, or the last one
	// kept when the batch kept a whole ContractBatchSize
	cursor server.Id
	// no contract of the walk's range is left past the cursor
	complete bool
	// contracts of the generation's passes the batch kept
	read int64
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

// One walk batch transaction from the walk's cursor: the window of the
// primary key, the contracts of the generation's passes in it, their locked
// classification and the deletes. A dry batch reads only, in a read only
// transaction, and takes no lock.
func (self *proberShardRetentionRun) removeWalkBatch(
	ctx context.Context,
	walk ProberShardContractWalk,
	networkIds []server.Id,
	dry bool,
) (batch *proberShardRetentionWalkBatch, returnErr error) {
	options := []any{server.TxReadCommitted, server.TxLockTimeout(self.settings.LockTimeout)}
	if dry {
		options = append(options, pgx.ReadOnly)
	}
	returnErr = proberShardRetentionRecover(func() {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// a retried transaction starts over
			batch = &proberShardRetentionWalkBatch{cursor: walk.Cursor, deleted: map[string]int64{}}
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, self.settings.StatementTimeout.Milliseconds())))
			// plan each statement for its actual ids
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_custom_plan`))

			var windowIds []server.Id
			server.Raise(tx.QueryRow(
				ctx,
				proberShardRetentionWalkWindowSql,
				walk.Lower,
				walk.Cursor,
				walk.Upper,
				self.settings.WalkBatchSize,
			).Scan(&windowIds))
			batch.walked = int64(len(windowIds))
			if batch.walked == 0 {
				batch.complete = true
				return
			}
			batch.cursor = windowIds[len(windowIds)-1]
			for _, setting := range proberShardRetentionBitmapSettings {
				server.RaisePgResult(tx.Exec(ctx, setting))
			}

			var matchedIds []server.Id
			server.Raise(tx.QueryRow(
				ctx,
				proberShardRetentionWalkMatchSql,
				windowIds,
				networkIds,
				self.cutoff,
				self.settings.ContractBatchSize,
			).Scan(&matchedIds))
			batch.read = int64(len(matchedIds))
			if 0 < batch.read && batch.read == int64(self.settings.ContractBatchSize) {
				// more of the window may match past the batch's last
				batch.cursor = matchedIds[len(matchedIds)-1]
			} else {
				batch.complete = batch.walked < int64(self.settings.WalkBatchSize)
			}
			if batch.read == 0 {
				return
			}

			classifySql := proberShardRetentionClassifyLockSql
			if dry {
				classifySql = proberShardRetentionClassifySql
			}
			var lockedCount int64
			server.Raise(tx.QueryRow(
				ctx,
				classifySql,
				matchedIds,
				networkIds,
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

// A dry run: the first page of due passes, of any generation, each counted as
// taken up, and a read only walk of the next generation due one, from where
// the walk stands, paced as a walk and bounded by the budget, counting what
// the drop would delete and keep. It changes nothing, the walk included, so
// the next dry run reads the same.
func (self *proberShardRetentionRun) dryRun(ctx context.Context, control *ProberShardRetentionControl) (returnErr error) {
	defer func() {
		if returnErr != nil && ctx.Err() != nil {
			self.report.Stop = proberShardRetentionStopCanceled
			returnErr = nil
		}
	}()
	owners, err := self.selectDueShards(ctx, true, nil)
	if err != nil {
		self.report.Stop = proberShardRetentionStopError
		return err
	}
	for range owners {
		self.report.addShard(proberShardRetentionShardDryRun)
	}
	walking := false
	if control.DropContracts {
		walking, err = self.dryWalk(ctx, control)
		if err != nil {
			self.report.Stop = proberShardRetentionStopError
			return err
		}
	}
	if len(owners) == 0 && !walking {
		self.report.CaughtUp = true
		self.report.Stop = proberShardRetentionStopCaughtUp
		return nil
	}
	self.report.Stop = proberShardRetentionStopDryRun
	return nil
}

// A read only walk of the next generation due one: from the walk's cursor
// when the walk in progress is that generation's, else from its start. False
// when no generation is due a walk.
func (self *proberShardRetentionRun) dryWalk(ctx context.Context, control *ProberShardRetentionControl) (walking bool, returnErr error) {
	generation, ok, err := self.nextWalkGeneration(ctx)
	if err != nil || !ok {
		return false, err
	}
	networkIds, lower, upper, guarded, err := self.walkPasses(ctx, generation)
	if err != nil || guarded {
		return false, err
	}
	if len(networkIds) == 0 {
		return true, nil
	}
	walk := self.walk
	if !walk.Generation.Equal(generation) {
		walk = ProberShardContractWalk{
			CompleteThrough: walk.CompleteThrough,
			Generation:      generation,
			Lower:           lower,
			Upper:           upper,
		}
	}
	for {
		if _, ok := self.stopped(ctx); ok {
			return true, nil
		}
		start := time.Now()
		batch, err := self.removeWalkBatch(ctx, walk, networkIds, true)
		duration := time.Since(start)
		if err != nil {
			return true, err
		}
		self.report.addWalked(batch.walked)
		self.report.addContracts(proberShardRetentionContractEligible, int64(len(batch.eligibleIds)))
		self.report.addContracts(proberShardRetentionContractKeptBytes, batch.keptBytes)
		self.report.addContracts(proberShardRetentionContractKeptUnsettled, batch.keptUnsettled)
		if batch.complete {
			return true, nil
		}
		walk.Cursor = batch.cursor
		delay := max(
			contractHorizonPaceDelay(batch.walked, control.MaxWalkRowsPerSecond, duration),
			contractHorizonPaceDelay(batch.read, control.MaxContractsPerSecond, duration),
		)
		if !self.pace(ctx, delay) {
			return true, ctx.Err()
		}
	}
}
