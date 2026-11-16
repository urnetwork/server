// Queued provider totals drain in bulk, beside the per-task reader.
//
// Settlement stopped queueing provider totals on 2026-11-11 (BILLING-SPLIT.md);
// the drain and the per-task reader stay to consume the rows already queued and
// those older binaries queue during a rollout. The rest of this comment
// describes the queue as it was produced.
//
// Every closed contract with provider payouts queued one ApplyLegacyProviderTotals
// row (queueLegacyProviderTotalsInTx). The per-task reader applies them one claim
// at a time, and each provider's rows apply one cohort at a time: its claim group
// key admits one Run slot per provider, and a cohort holds at most 64 rows. A
// cohort's cycle is a chain of serialized round trips (about three per member to
// claim, one per member to unlock, plus the account and completion
// transactions). On Main on 2026-11-01 four providers had 82% of the rows, a
// provider took a cohort about every 85 ms with 6 to 18 members, and the four
// lanes applied 400 to 600 rows a second while 450 to 850 a second were queued.
// Their rows then filled the oldest-first claim window, so other claims found
// nothing they could take and slept their poll timeout, and the queue grew to
// 27M rows, its oldest 10 hours overdue.
//
// One fleet-wide run-once chain, drain_legacy_provider_totals, now consumes the
// queued rows directly. Each pass reads up to legacyProviderTotalsDrainPassRows
// rows in task id order, which is creation order (ids lead with their creation
// millisecond), so a pass touches few heap pages: on Main 4,096 consecutive rows
// sat on 912 pages. In one transaction under the complete account owner of
// every network it credits, a pass locks the rows it can take, re-reads their
// durable payloads, adds one sum per network and deletes the rows. A row is
// credited exactly when it is removed, so a lost reply, a rollback, a retry or a
// concurrent pass can neither drop nor repeat an allocation. A row whose payload
// is already applied (the per-task reader committed its credit but not its
// completion) is removed without a credit.
//
// The pass never waits for a row: it skips a row another transaction holds or a
// live claim leases, and it leaves an invalid payload. The per-task reader stays
// registered and applies every row the drain leaves, so it is the fallback for
// skipped rows and for a paused or failed chain. Producers schedule each row
// legacyProviderTotalsDeferral ahead, so the per-task reader sees a row only once
// the drain has had that long to take it. Rows from producers built before this
// change are due at once; both readers take them.
//
// The walk never passes ids created in the last legacyProviderTotalsDrainLag: a
// producer transaction that drew its id earlier may still be open, and its row
// would land behind the walk. The chain keeps its position in its own arguments.
//
// provider_totals.yml can pause the drain or change its pace without a deploy:
//
//	drain:
//	  enabled: true         # false stops the drain; the per-task reader applies every row
//	  rows_per_second: 4000 # rows removed per second, 100 to 20000
//
// An absent or unreadable resource, or a value out of bounds, keeps the default.
package model

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The per-task reader sees a queued row only after this, so it does not compete
// with a running drain for rows. It applies them itself when the drain stops.
const legacyProviderTotalsDeferral = 5 * time.Minute

// Rows one pass reads. A pass's statements stay well inside the projection's
// two-second statement bound.
const legacyProviderTotalsDrainPassRows = 1024

// The walk leaves ids younger than this for a later pass, longer than clock skew
// between producers and an open producer transaction.
const legacyProviderTotalsDrainLag = time.Minute

// Passes start at least this far apart, whatever their size.
const legacyProviderTotalsDrainPassInterval = 25 * time.Millisecond

const legacyProviderTotalsDrainDefaultRowsPerSecond = 4000
const legacyProviderTotalsDrainMinRowsPerSecond = 100
const legacyProviderTotalsDrainMaxRowsPerSecond = 20000

// A run stops starting passes after this, well inside its task deadline.
const legacyProviderTotalsDrainRunBudget = 30 * time.Second
const legacyProviderTotalsDrainMaxTime = 2 * time.Minute

// Successor delays: after a run that caught up, after a failed pass, and while
// the drain is paused.
const legacyProviderTotalsDrainIdleDelay = 5 * time.Second
const legacyProviderTotalsDrainErrorDelay = 10 * time.Second
const legacyProviderTotalsDrainPausedDelay = time.Minute

const legacyProviderTotalsDrainResourceName = "provider_totals.yml"

var legacyProviderTotalsDrainRunOnce = task.RunOnce("legacy_provider_totals_drain")

// provider_totals.yml. Absent values keep their defaults.
type legacyProviderTotalsDrainConfig struct {
	Drain *legacyProviderTotalsDrainConfigValues `yaml:"drain"`
}

type legacyProviderTotalsDrainConfigValues struct {
	Enabled       *bool `yaml:"enabled"`
	RowsPerSecond *int  `yaml:"rows_per_second"`
}

type legacyProviderTotalsDrainSettings struct {
	enabled       bool
	rowsPerSecond int
}

// Read when each run starts. Only an explicit enabled: false pauses the drain.
func readLegacyProviderTotalsDrainSettings() legacyProviderTotalsDrainSettings {
	resource, err := server.Config.SimpleResource(legacyProviderTotalsDrainResourceName)
	if err != nil {
		return legacyProviderTotalsDrainSettingsFromConfig(nil)
	}
	var config legacyProviderTotalsDrainConfig
	if err := resource.UnmarshalYamlE(&config); err != nil {
		return legacyProviderTotalsDrainSettingsFromConfig(nil)
	}
	return legacyProviderTotalsDrainSettingsFromConfig(&config)
}

// The defaults, with each value of a readable config that is in bounds.
func legacyProviderTotalsDrainSettingsFromConfig(config *legacyProviderTotalsDrainConfig) legacyProviderTotalsDrainSettings {
	settings := legacyProviderTotalsDrainSettings{enabled: true, rowsPerSecond: legacyProviderTotalsDrainDefaultRowsPerSecond}
	if config == nil || config.Drain == nil {
		return settings
	}
	if config.Drain.Enabled != nil {
		settings.enabled = *config.Drain.Enabled
	}
	if value := config.Drain.RowsPerSecond; value != nil &&
		legacyProviderTotalsDrainMinRowsPerSecond <= *value && *value <= legacyProviderTotalsDrainMaxRowsPerSecond {
		settings.rowsPerSecond = *value
	}
	return settings
}

var legacyProviderTotalsDrainRowsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_totals_drain_rows_total",
	Help: "Queued provider-total allocations the drain read, by outcome after an acknowledged commit: credited (added to the provider account and removed), applied (already credited by the per-task reader, removed without a credit) or left (claimed, held, changed or invalid; the per-task reader keeps them). Credited and applied rows are not moved to finished_task.",
}, []string{"outcome"})

var legacyProviderTotalsDrainPassesCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_provider_totals_drain_passes_total",
	Help: "Provider-total drain passes by result: ok (acknowledged, including passes that found nothing) or error (failed or unacknowledged; nothing is assumed applied).",
}, []string{"result"})

var legacyProviderTotalsDrainPositionGauge = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_provider_totals_drain_position_timestamp_seconds",
	Help: "Creation time of the last queued provider total the drain visited, from its task id. Only the process running the chain updates it; take the max across processes.",
})

func init() {
	prometheus.MustRegister(legacyProviderTotalsDrainRowsCounter, legacyProviderTotalsDrainPassesCounter, legacyProviderTotalsDrainPositionGauge)
}

// One queued row as a pass read it, without locks.
type legacyProviderTotalsDrainCandidate struct {
	taskId      server.Id
	argsJson    string
	releaseTime time.Time
}

// What one acknowledged pass did with the rows it read.
type legacyProviderTotalsDrainOutcome struct {
	credited int
	applied  int
	left     int
}

func (self legacyProviderTotalsDrainOutcome) removed() int {
	return self.credited + self.applied
}

// Task id order is the primary key's order. Rows of other targets are filtered
// on the heap rows the scan already reads for the payload.
const legacyProviderTotalsDrainReadSql = `SELECT task_id,args_json,release_time FROM pending_task
    WHERE task_id>$1 AND task_id<$2 AND function_name=$3
    ORDER BY task_id
    LIMIT $4`

// Holding every lock until commit, the pass re-reads the exact payloads. A
// live claim's lease or another transaction's lock leaves the row to its owner.
const legacyProviderTotalsDrainLockSql = `SELECT task_id,args_json FROM pending_task
    WHERE task_id=ANY($1) AND function_name=$2 AND release_time<=$3
    ORDER BY task_id
    FOR UPDATE SKIP LOCKED`

const legacyProviderTotalsDrainRemoveSql = `DELETE FROM pending_task WHERE task_id=ANY($1) AND function_name=$2`

// Reads the rows after the position, up to the newest id older than the lag.
func readLegacyProviderTotalsDrainCandidates(ctx context.Context, after server.Id, now time.Time, limit int) (candidates []legacyProviderTotalsDrainCandidate, returnErr error) {
	before := server.ContractIdFloor(now.Add(-legacyProviderTotalsDrainLag))
	returnErr = runLegacyProviderTotalsTx(ctx, func(tx server.PgTx) error {
		candidates = nil
		rows, err := tx.Query(ctx, legacyProviderTotalsDrainReadSql, after, before,
			task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), limit)
		if err != nil {
			return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
		}
		defer rows.Close()
		for rows.Next() {
			var candidate legacyProviderTotalsDrainCandidate
			if err := rows.Scan(&candidate.taskId, &candidate.argsJson, &candidate.releaseTime); err != nil {
				return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
			}
			candidates = append(candidates, candidate)
		}
		return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, rows.Err())
	})
	if returnErr != nil {
		candidates = nil
	}
	return
}

// Runs a pass's transaction under the complete account owner of networkIds, or
// as an ordinary projection transaction when the pass credits no account.
type legacyProviderTotalsDrainRunner func(ctx context.Context, networkIds []server.Id, apply func(server.PgTx) error) error

func runLegacyProviderTotalsDrainTx(ctx context.Context, networkIds []server.Id, apply func(server.PgTx) error) error {
	if len(networkIds) == 0 {
		return runLegacyProviderTotalsTx(ctx, apply)
	}
	return runLegacyProviderTotalsOwnedTx(ctx, &legacyProviderOwnership{networkIds: networkIds}, apply)
}

func consumeLegacyProviderTotals(ctx context.Context, candidates []legacyProviderTotalsDrainCandidate, now time.Time) (legacyProviderTotalsDrainOutcome, error) {
	return consumeLegacyProviderTotalsWithRunner(ctx, candidates, now, runLegacyProviderTotalsDrainTx)
}

// Credits and removes the candidates the pass can take, in one transaction. The
// unlocked read only chooses the rows and their owners; amounts and the applied
// marker come from the locked rows. The outcome is zero unless the transaction
// is acknowledged.
func consumeLegacyProviderTotalsWithRunner(ctx context.Context, candidates []legacyProviderTotalsDrainCandidate, now time.Time,
	run legacyProviderTotalsDrainRunner) (outcome legacyProviderTotalsDrainOutcome, returnErr error) {
	admittedNetworkIds := map[server.Id]bool{}
	taskIds := make([]server.Id, 0, len(candidates))
	for _, candidate := range candidates {
		if now.Before(candidate.releaseTime) {
			// A live claim's lease: its owner applies and completes the row.
			continue
		}
		payload, err := decodeLegacyProviderTotals(candidate.argsJson)
		if err != nil {
			// The per-task reader reports an invalid payload as its own error.
			continue
		}
		if !payload.Applied {
			for _, total := range payload.Totals {
				admittedNetworkIds[total.NetworkId] = true
			}
		}
		taskIds = append(taskIds, candidate.taskId)
	}
	if len(taskIds) == 0 {
		return legacyProviderTotalsDrainOutcome{left: len(candidates)}, nil
	}
	networkIds := make([]server.Id, 0, len(admittedNetworkIds))
	for networkId := range admittedNetworkIds {
		networkIds = append(networkIds, networkId)
	}
	slices.SortFunc(networkIds, func(a, b server.Id) int { return a.Cmp(b) })
	returnErr = run(ctx, networkIds, func(tx server.PgTx) error {
		outcome = legacyProviderTotalsDrainOutcome{}
		if 0 < len(networkIds) && !server.TxOwnsKeys(tx, accountBalanceOwnershipKeys(networkIds)) {
			return withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, errors.New("legacy provider total drain owners were not admitted"))
		}
		locked, err := lockLegacyProviderTotalsDrainRowsInTx(ctx, tx, taskIds, admittedNetworkIds, now)
		if err != nil {
			return err
		}
		if len(locked.removedTaskIds) == 0 {
			return nil
		}
		if err := writeLegacyProviderTotalsAndRemoveInTx(ctx, tx, locked.credits, locked.removedTaskIds); err != nil {
			return err
		}
		outcome = legacyProviderTotalsDrainOutcome{credited: locked.credited, applied: locked.applied}
		return nil
	})
	if returnErr != nil {
		return legacyProviderTotalsDrainOutcome{}, returnErr
	}
	outcome.left = len(candidates) - outcome.removed()
	return outcome, nil
}

// The locked rows a pass removes: one credit per network in network order, and
// how many rows were credited or were already applied.
type legacyProviderTotalsDrainLocked struct {
	credits        []legacyProviderTotal
	removedTaskIds []server.Id
	credited       int
	applied        int
}

// Locks the rows the pass can take and sums their unapplied allocations per
// network. A row that changed beyond the admitted owners stays.
func lockLegacyProviderTotalsDrainRowsInTx(ctx context.Context, tx server.PgTx, taskIds []server.Id, admittedNetworkIds map[server.Id]bool,
	now time.Time) (locked legacyProviderTotalsDrainLocked, returnErr error) {
	rows, err := tx.Query(ctx, legacyProviderTotalsDrainLockSql, taskIds, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), now)
	if err != nil {
		return locked, withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
	}
	defer rows.Close()
	sums := map[server.Id]*legacyProviderTotal{}
	for rows.Next() {
		var taskId server.Id
		var data string
		if err := rows.Scan(&taskId, &data); err != nil {
			return legacyProviderTotalsDrainLocked{}, withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
		}
		payload, err := decodeLegacyProviderTotals(data)
		if err != nil {
			continue
		}
		if payload.Applied {
			locked.removedTaskIds = append(locked.removedTaskIds, taskId)
			locked.applied++
			continue
		}
		if slices.ContainsFunc(payload.Totals, func(total legacyProviderTotal) bool { return !admittedNetworkIds[total.NetworkId] }) {
			continue
		}
		for _, total := range payload.Totals {
			sum := sums[total.NetworkId]
			if sum == nil {
				sum = &legacyProviderTotal{NetworkId: total.NetworkId}
				sums[total.NetworkId] = sum
			}
			if math.MaxInt64-sum.Bytes < total.Bytes || math.MaxInt64-sum.Revenue < total.Revenue {
				return legacyProviderTotalsDrainLocked{}, withLegacyProviderTotalsPhase(legacyProviderTotalsAllocation, errors.New("legacy provider total drain overflow"))
			}
			sum.Bytes += total.Bytes
			sum.Revenue += total.Revenue
		}
		locked.removedTaskIds = append(locked.removedTaskIds, taskId)
		locked.credited++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return legacyProviderTotalsDrainLocked{}, withLegacyProviderTotalsPhase(legacyProviderTotalsPendingRead, err)
	}
	for _, sum := range sums {
		locked.credits = append(locked.credits, *sum)
	}
	slices.SortFunc(locked.credits, func(a, b legacyProviderTotal) int { return a.NetworkId.Cmp(b.NetworkId) })
	return locked, nil
}

// Credits in pages of at most legacyProviderTotalsWriteBatchLimit statements;
// the last page also removes every consumed row. The rows are locked, so the
// removal must match each one.
func writeLegacyProviderTotalsAndRemoveInTx(ctx context.Context, tx server.PgTx, credits []legacyProviderTotal, taskIds []server.Id) error {
	for offset := 0; offset == 0 || offset < len(credits); offset += legacyProviderTotalsWriteBatchLimit {
		end := min(offset+legacyProviderTotalsWriteBatchLimit, len(credits))
		page := credits[offset:end]
		if err := writeLegacyProviderTotalsDrainPageInTx(ctx, tx, page, end == len(credits), taskIds); err != nil {
			return err
		}
		for _, credit := range page {
			observeAccountBalanceWriteForTest(ctx, tx, credit.NetworkId)
		}
	}
	return nil
}

func writeLegacyProviderTotalsDrainPageInTx(ctx context.Context, tx server.PgTx, credits []legacyProviderTotal, last bool, taskIds []server.Id) (returnErr error) {
	batch := &pgx.Batch{}
	for _, credit := range credits {
		batch.Queue(legacyProviderTotalWriteSql, credit.NetworkId, credit.Bytes, credit.Revenue)
	}
	if last {
		batch.Queue(legacyProviderTotalsDrainRemoveSql, taskIds, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName())
	}
	results := tx.SendBatch(ctx, batch)
	phase := legacyProviderTotalsAccountWrite
	defer func() {
		if err := results.Close(); err != nil && !errors.Is(returnErr, err) {
			closeErr := withLegacyProviderTotalsPhase(phase, err)
			if returnErr == nil {
				returnErr = closeErr
			} else {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()
	for range credits {
		if _, err := results.Exec(); err != nil {
			return withLegacyProviderTotalsPhase(phase, err)
		}
	}
	if last {
		phase = legacyProviderTotalsDrainRemove
		tag, err := results.Exec()
		if err != nil {
			return withLegacyProviderTotalsPhase(phase, err)
		}
		if tag.RowsAffected() != int64(len(taskIds)) {
			return withLegacyProviderTotalsPhase(phase, errors.New("legacy provider total drain lost a locked row"))
		}
	}
	return nil
}

// Spaces passes to the configured rate of removed rows.
type legacyProviderTotalsDrainPace struct {
	rowsPerSecond int
	nextPass      time.Time
}

// Charges a pass that started at passStart and removed rows, then waits until
// the next pass may start or ctx ends.
func (self *legacyProviderTotalsDrainPace) AwaitNextPass(ctx context.Context, passStart time.Time, rows int) {
	if self.nextPass.Before(passStart) {
		self.nextPass = passStart
	}
	self.nextPass = self.nextPass.Add(max(legacyProviderTotalsDrainPassInterval,
		time.Duration(rows)*time.Second/time.Duration(max(1, self.rowsPerSecond))))
	wait := time.Until(self.nextPass)
	if wait <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// After is the id of the last row a pass visited; zero starts at the oldest row.
type DrainLegacyProviderTotalsArgs struct {
	After server.Id `json:"after"`
}

type DrainLegacyProviderTotalsResult struct {
	Enabled  bool      `json:"enabled"`
	Next     server.Id `json:"next"`
	Passes   int       `json:"passes"`
	Credited int       `json:"credited"`
	Applied  int       `json:"applied"`
	Left     int       `json:"left"`
	// The run stopped with rows still to read; the successor starts at once.
	More  bool   `json:"more,omitempty"`
	Error string `json:"error,omitempty"`
}

// Reads and consumes passes from args.After until a pass comes up short, the
// budget ends or more says to stop. A failed pass keeps its position.
func drainLegacyProviderTotals(ctx context.Context, args *DrainLegacyProviderTotalsArgs, rowsPerSecond int, more func() bool) *DrainLegacyProviderTotalsResult {
	result := &DrainLegacyProviderTotalsResult{Enabled: true, Next: args.After}
	pace := &legacyProviderTotalsDrainPace{rowsPerSecond: rowsPerSecond}
	for more() {
		passStart := time.Now()
		now := server.NowUtc()
		candidates, err := readLegacyProviderTotalsDrainCandidates(ctx, result.Next, now, legacyProviderTotalsDrainPassRows)
		var outcome legacyProviderTotalsDrainOutcome
		if err == nil {
			outcome, err = consumeLegacyProviderTotals(ctx, candidates, now)
		}
		if err != nil {
			legacyProviderTotalsDrainPassesCounter.WithLabelValues("error").Inc()
			result.Error = err.Error()
			result.More = false
			return result
		}
		legacyProviderTotalsDrainPassesCounter.WithLabelValues("ok").Inc()
		legacyProviderTotalsDrainRowsCounter.WithLabelValues("credited").Add(float64(outcome.credited))
		legacyProviderTotalsDrainRowsCounter.WithLabelValues("applied").Add(float64(outcome.applied))
		legacyProviderTotalsDrainRowsCounter.WithLabelValues("left").Add(float64(outcome.left))
		result.Passes++
		result.Credited += outcome.credited
		result.Applied += outcome.applied
		result.Left += outcome.left
		if 0 < len(candidates) {
			result.Next = candidates[len(candidates)-1].taskId
			legacyProviderTotalsDrainPositionGauge.Set(float64(server.ContractIdTime(result.Next).UnixMilli()) / 1000)
		}
		result.More = len(candidates) == legacyProviderTotalsDrainPassRows
		if !result.More {
			return result
		}
		pace.AwaitNextPass(ctx, passStart, outcome.removed())
	}
	return result
}

// One budgeted run from the retained position.
func DrainLegacyProviderTotals(args *DrainLegacyProviderTotalsArgs, clientSession *session.ClientSession) (*DrainLegacyProviderTotalsResult, error) {
	settings := readLegacyProviderTotalsDrainSettings()
	if !settings.enabled {
		return &DrainLegacyProviderTotalsResult{Next: args.After}, nil
	}
	started := time.Now()
	result := drainLegacyProviderTotals(clientSession.Ctx, args, settings.rowsPerSecond, func() bool {
		return clientSession.Ctx.Err() == nil && time.Since(started) < legacyProviderTotalsDrainRunBudget
	})
	if err := clientSession.Ctx.Err(); err != nil {
		// Committed passes are complete on their own; the retry starts over
		// from the unchanged position and skips the rows they removed.
		return nil, err
	}
	if result.Error != "" {
		glog.Infof("[provider-totals-drain]pass failed after %d passes: %s\n", result.Passes, result.Error)
	}
	glog.Infof("[provider-totals-drain]passes=%d credited=%d applied=%d left=%d more=%t\n",
		result.Passes, result.Credited, result.Applied, result.Left, result.More)
	return result, nil
}

// The successor starts at once while rows remain, after the error delay when a
// pass failed, after the paused delay while the drain is paused, and otherwise
// after the idle delay, from the position this run reached.
func DrainLegacyProviderTotalsPost(_ *DrainLegacyProviderTotalsArgs, result *DrainLegacyProviderTotalsResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	runAt := server.NowUtc()
	switch {
	case !result.Enabled:
		runAt = runAt.Add(legacyProviderTotalsDrainPausedDelay)
	case result.Error != "":
		runAt = runAt.Add(legacyProviderTotalsDrainErrorDelay)
	case !result.More:
		runAt = runAt.Add(legacyProviderTotalsDrainIdleDelay)
	}
	scheduleDrainLegacyProviderTotalsInTx(clientSession, tx, &DrainLegacyProviderTotalsArgs{After: result.Next}, runAt)
	return nil
}

func scheduleDrainLegacyProviderTotalsInTx(clientSession *session.ClientSession, tx server.PgTx, args *DrainLegacyProviderTotalsArgs, runAt time.Time) {
	task.ScheduleTaskInTx(tx, DrainLegacyProviderTotals, args, clientSession,
		legacyProviderTotalsDrainRunOnce, task.RunAt(runAt), task.MaxTime(legacyProviderTotalsDrainMaxTime))
}

// The chain's queue key, for a startup transaction that admits it.
func LegacyProviderTotalsDrainOwnershipKey() server.PgOwnershipKey {
	return task.RunOnceOwnershipKey(legacyProviderTotalsDrainRunOnce)
}

// Startup arms the chain at the oldest row; a pending run keeps its position.
func ScheduleDrainLegacyProviderTotals(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleDrainLegacyProviderTotalsInTx(clientSession, tx, &DrainLegacyProviderTotalsArgs{}, server.NowUtc())
}

// A failed run retries within a minute from its unchanged position.
func NewLegacyProviderTotalsDrainTaskTarget() task.Target {
	return task.WithErrorRetryCap(task.NewTaskTargetWithPost(DrainLegacyProviderTotals, DrainLegacyProviderTotalsPost), time.Minute)
}
