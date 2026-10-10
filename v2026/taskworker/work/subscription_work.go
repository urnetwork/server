package work

import (
	"context"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// ForceCloseOpenContractIds already fans the selected contracts out across its
// internal worker pool. Multiple task-level shards each repeated the same
// ordered 100k-row database scan before discarding 7/8 of it in Go, so keep one
// scheduler task and let the existing in-process parallelism do the work.
const DefaultCloseExpiredContractsBlockSize = 1

const (
	// Checkpoint task success before the 30-minute task deadline. A 100,003-row
	// production cohort hit that deadline exactly while transfer_contract was
	// paying down retention/autovacuum write debt; its per-contract commits
	// survived, but the task retried with a Timeout and had to rescan. Retain
	// the 25k per-stream total cap and worker parallelism; the model now
	// checkpoints smaller raw subpages inside its own elapsed-time budget.
	// Open and disputed rows remain independent, with a union up to 50k.
	closeExpiredContractsMaxCount = 25_000
	closeExpiredContractsParallel = 92
)

func closeExpiredContractsFull(closeCount int64) bool {
	return int64(closeExpiredContractsMaxCount/(4*DefaultCloseExpiredContractsBlockSize)) <= closeCount
}

// A rejected dispute does not count as a close. An acknowledged raw cursor
// still advances toward other due rows; only a completed pass may idle.
func closeExpiredContractsRetryDelay(verifiedCloseCount int64, hasMore bool, randomUnit float64) time.Duration {
	randomUnit = max(0, min(randomUnit, math.Nextafter(1, 0)))
	if hasMore || closeExpiredContractsFull(verifiedCloseCount) {
		return 2*time.Second + time.Duration(randomUnit*float64(2*time.Second))
	}
	return time.Minute + time.Duration(randomUnit*float64(4*time.Minute))
}

type CloseExpiredContractsArgs struct {
	BlockSize      int                              `json:"block_size"`
	BlockIndex     int                              `json:"block_index"`
	Cursor         *model.ContractExpiryCursor      `json:"cursor,omitempty"`
	Sweep          *model.ContractExpirySweepCursor `json:"sweep,omitempty"`
	NextExpiration *time.Time                       `json:"next_expiration,omitempty"`
}

type CloseExpiredContractsResult struct {
	Full           bool                             `json:"full"`
	Cursor         *model.ContractExpiryCursor      `json:"cursor,omitempty"`
	Sweep          *model.ContractExpirySweepCursor `json:"sweep,omitempty"`
	NextExpiration *time.Time                       `json:"next_expiration,omitempty"`
}

func ScheduleCloseExpiredContracts(clientSession *session.ClientSession, tx server.PgTx, blockIndex int, delay bool) {
	scheduleCloseExpiredContractsPage(clientSession, tx, blockIndex, delay, nil, nil)
}

func scheduleCloseExpiredContractsPage(clientSession *session.ClientSession, tx server.PgTx, blockIndex int, delay bool, cursor *model.ContractExpiryCursor, sweep *model.ContractExpirySweepCursor) {
	scheduleCloseExpiredContractsPageWithExpiration(clientSession, tx, blockIndex, delay, cursor, sweep, nil)
}

// Cap the idle successor at the observed expiration. RunOnce still retains any
// earlier request. Worker availability and failed visits can delay execution;
// this is a requested wake, not a promise of settlement at that instant.
func scheduleCloseExpiredContractsPageWithExpiration(clientSession *session.ClientSession, tx server.PgTx, blockIndex int, delay bool,
	cursor *model.ContractExpiryCursor, sweep *model.ContractExpirySweepCursor, expiration *time.Time,
) {
	blockSize := DefaultCloseExpiredContractsBlockSize
	blockIndex = blockIndex % blockSize

	now := server.NowUtc()
	runAt := now
	if delay {
		randomDelay := time.Minute + time.Duration(mathrand.Int63n(int64(4*time.Minute)))
		runAt = runAt.Add(randomDelay)
	}
	if expiration != nil && expiration.Before(runAt) {
		runAt = *expiration
		if runAt.Before(now) {
			runAt = now
		}
	}
	if cursor == nil && sweep == nil {
		// The wake consumes this pass's observation. Its successor starts
		// at the head and must not carry an expired hint forever.
		expiration = nil
	}

	task.ScheduleTaskInTx(
		tx,
		CloseExpiredContracts,
		&CloseExpiredContractsArgs{
			BlockSize:      blockSize,
			BlockIndex:     blockIndex,
			Cursor:         cursor,
			Sweep:          sweep,
			NextExpiration: expiration,
		},
		clientSession,
		// legacy key
		task.RunOnce(fmt.Sprintf("close_expired_contracts_%d_%d", blockSize, blockIndex)),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
		task.Priority(task.TaskPriorityFastest),
	)
}

// Allow two default receive-owner lifetimes before synthesizing an absent report.
// The bounded cursor and accounting/intent gates remain authoritative.
func closeExpiredContractsCutoff(now time.Time) time.Time {
	return now.Add(-12 * time.Minute)
}

func CloseExpiredContracts(
	closeExpiredContracts *CloseExpiredContractsArgs,
	clientSession *session.ClientSession,
) (*CloseExpiredContractsResult, error) {
	if closeExpiredContracts.BlockSize == DefaultCloseExpiredContractsBlockSize {
		minTime := closeExpiredContractsCutoff(server.NowUtc())
		sweep := closeExpiredContracts.Sweep
		if sweep == nil && closeExpiredContracts.Cursor != nil {
			sweep = &model.ContractExpirySweepCursor{Historical: closeExpiredContracts.Cursor}
		}
		expiration := closeExpiredContracts.NextExpiration
		if sweep == nil {
			// A classified error may have completed the prior pass too.
			expiration = nil
		}
		c, next, expiration, err := model.ForceCloseOpenContractIdsScheduledPage(
			clientSession.Ctx,
			minTime,
			closeExpiredContractsMaxCount,
			closeExpiredContractsParallel,
			closeExpiredContracts.BlockSize,
			closeExpiredContracts.BlockIndex,
			sweep,
			expiration,
		)
		nextArgs := *closeExpiredContracts
		nextArgs.NextExpiration = expiration
		return closeExpiredContractsSweepPageResult(clientSession.Ctx, &nextArgs, c, next, err)
	}
	// else ignore lingering tasks with older block size
	return &CloseExpiredContractsResult{}, nil
}

// Keep both lanes in the same successful or classified-error checkpoint. The
// legacy cursor remains available to a rollback; it never replaces sweep state.
func closeExpiredContractsSweepPageResult(ctx context.Context, args *CloseExpiredContractsArgs, c int64,
	next *model.ContractExpirySweepCursor, err error,
) (*CloseExpiredContractsResult, error) {
	var historical *model.ContractExpiryCursor
	if next != nil {
		historical = next.Historical
	}
	nextArgs := *args
	nextArgs.Sweep = next
	result, err := closeExpiredContractsPageResult(ctx, &nextArgs, c, historical, err)
	result.Sweep = next
	if err == nil && next != nil {
		// Recent work can outlast its historical pass. Raw continuation
		// still gets a successor even when no close was verified.
		result.Full = true
	}
	return result, err
}

// Completed accounting or row-visit failures can checkpoint their raw position
// while remaining task failures. End of pass resets the cursor so unresolved
// rows return. Interrupted or unattested work keeps the original task args.
func closeExpiredContractsPageResult(ctx context.Context, args *CloseExpiredContractsArgs, c int64, next *model.ContractExpiryCursor, err error) (*CloseExpiredContractsResult, error) {
	// The model alone can attest that every selected row completed and
	// every failure is a verified still-reserved dispute or completed
	// no-payout quarantine. Never infer authority from a mixed error join.
	// Scanning past an owned or recently active head is page progress,
	// not a verified close. Continue the bounded pass without parking it
	// at the idle cadence; completion resets to the oldest head next pass.
	full := closeExpiredContractsFull(c) || next != nil
	if accounting, ok := err.(*model.ForceCloseAccountingError); ok && ctx.Err() == nil &&
		0 <= accounting.VerifiedCloseCount() && 0 <= accounting.AccountingRejectionCount() &&
		0 <= accounting.QuarantinedAccountingRejectionCount() && accounting.QuarantinedAccountingRejectionCount() <= accounting.VerifiedCloseCount() &&
		0 < accounting.AccountingRejectionCount()+accounting.QuarantinedAccountingRejectionCount() &&
		accounting.VerifiedCloseCount()+accounting.AccountingRejectionCount() == c {
		hasMore := next != nil || args.Sweep != nil
		full = hasMore || closeExpiredContractsFull(accounting.VerifiedCloseCount())
		delay := closeExpiredContractsRetryDelay(accounting.VerifiedCloseCount(), hasMore, mathrand.Float64())
		err = task.WithRetryDelayAndArgs(err, delay, &CloseExpiredContractsArgs{
			BlockSize: args.BlockSize, BlockIndex: args.BlockIndex, Cursor: next, Sweep: args.Sweep,
			NextExpiration: args.NextExpiration,
		})
		glog.Infof("[close-expired]completed batch terminal_verified=%d unresolved_accounting=%d quarantined_accounting=%d retry_delay_ms=%d\n",
			accounting.VerifiedCloseCount(), accounting.AccountingRejectionCount(), accounting.QuarantinedAccountingRejectionCount(), delay.Milliseconds())
	} else if visited, ok := err.(*model.ForceCloseVisitError); ok && ctx.Err() == nil &&
		visited.CanCheckpoint() && visited.AttemptedCloseCount() == c {
		// Keep operational error counts, metrics and ordinary backoff. This
		// persists scan work only; it grants no financial completion authority.
		err = task.WithRetryArgs(err, &CloseExpiredContractsArgs{
			BlockSize: args.BlockSize, BlockIndex: args.BlockIndex, Cursor: next, Sweep: args.Sweep,
			NextExpiration: args.NextExpiration,
		})
	}
	return &CloseExpiredContractsResult{
		Full:           full,
		Cursor:         next,
		NextExpiration: args.NextExpiration,
	}, err
}

func CloseExpiredContractsPost(
	closeExpiredContracts *CloseExpiredContractsArgs,
	closeExpiredContractsResult *CloseExpiredContractsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleCloseExpiredContractsPageWithExpiration(clientSession, tx, closeExpiredContracts.BlockIndex, !closeExpiredContractsResult.Full,
		closeExpiredContractsResult.Cursor, closeExpiredContractsResult.Sweep, closeExpiredContractsResult.NextExpiration)
	return nil
}

// Backfill initial transfer balance

type BackfillInitialTransferBalanceArgs struct {
}

type BackfillInitialTransferBalanceResult struct {
}

func ScheduleBackfillInitialTransferBalance(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		BackfillInitialTransferBalance,
		&BackfillInitialTransferBalanceArgs{},
		clientSession,
		task.RunOnce("backfill_initial_transfer_balance"),
		task.RunAt(server.NowUtc().Add(15*time.Minute)),
	)
}

func BackfillInitialTransferBalance(
	backfillInitialTransferBalance *BackfillInitialTransferBalanceArgs,
	clientSession *session.ClientSession,
) (*BackfillInitialTransferBalanceResult, error) {
	networkIds := model.FindNetworksWithoutTransferBalance(clientSession.Ctx)
	if err := backfillInitialTransferBalances(clientSession.Ctx, networkIds, controller.AddRefreshTransferBalance); err != nil {
		return nil, err
	}
	return &BackfillInitialTransferBalanceResult{}, nil
}

// Retain partial progress, but never finish the one-shot task while a grant
// failed. A replay rediscovers only networks still missing their first balance.
func backfillInitialTransferBalances(ctx context.Context, networkIds []server.Id, grant func(context.Context, server.Id) error) error {
	var result error
	for _, networkId := range networkIds {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		if err := grant(ctx, networkId); err != nil {
			result = errors.Join(result, fmt.Errorf("initial balance for network %s: %w", networkId, err))
		}
	}
	return errors.Join(result, ctx.Err())
}

func BackfillInitialTransferBalancePost(
	backfillInitialTransferBalance *BackfillInitialTransferBalanceArgs,
	backfillInitialTransferBalanceResult *BackfillInitialTransferBalanceResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}

type RemoveCompletedContractsArgs struct {
}

type RemoveCompletedContractsResult struct {
}

func ScheduleRemoveCompletedContracts(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveCompletedContracts,
		&RemoveCompletedContractsArgs{},
		clientSession,
		task.RunOnce("remove_completed_contracts"),
		// every 30 minutes: RemoveCompletedContracts drains each eligible set in
		// bounded batches per run (see removeContractBatches), so retention keeps
		// up without a high cadence -- the batched anti-join reapers no longer
		// re-scan the whole old-closed contract set every minute.
		task.RunAt(server.NowUtc().Add(30*time.Minute)),
		task.MaxTime(30*time.Minute),
	)
}

func RemoveCompletedContracts(
	removeCompletedContracts *RemoveCompletedContractsArgs,
	clientSession *session.ClientSession,
) (*RemoveCompletedContractsResult, error) {
	minTime := server.NowUtc().Add(-7 * 24 * time.Hour)
	model.RemoveCompletedContracts(clientSession.Ctx, minTime)
	return &RemoveCompletedContractsResult{}, nil
}

func RemoveCompletedContractsPost(
	removeCompletedContracts *RemoveCompletedContractsArgs,
	removeCompletedContractsResult *RemoveCompletedContractsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveCompletedContracts(clientSession, tx)
	return nil
}

// SweepOrphanContractData is the low-cadence safety net for
// contract_close/transfer_escrow/transfer_escrow_sweep rows whose contract no
// longer exists. RemoveCompletedContracts cascades dependents together with
// the contract deletes on every run, so this only catches orphans from
// interrupted statements or older releases.
//
// A pass over these tables is far too big to run end to end: contract_close
// alone is ~1.5B rows, so probing all three against transfer_contract takes
// hours. The pass is therefore SPREAD: each run pages a bounded
// sweepOrphanContractMaxRowCount rows and returns where it stopped, Post hands
// that cursor to the next run, and a full pass completes over days.
//
// The row budget is the load-bearing part. Before 2026-08-11 a run simply paged
// until the task deadline killed it, which meant it never returned normally, so
// Post never ran, so the weekly cadence was never re-armed and the chain fell
// back to error-retry — restarting the pass from row zero every time. It had
// never completed a single pass, was running ~63% of wall clock, and cost 7.6%
// of all db time re-walking the same prefix of contract_close to find zero
// orphans. Keep the budget well under MaxTime so a run always COMPLETES.

type SweepOrphanContractDataArgs struct {
	// where the previous run stopped; the zero value starts a fresh pass
	Cursor model.SweepOrphanCursor `json:"cursor"`
}

type SweepOrphanContractDataResult struct {
	RemovedCount int64 `json:"removed_count"`
	// where this run stopped, handed back by Post as the next run's start
	Cursor model.SweepOrphanCursor `json:"cursor"`
	// every table was fully paged, so the next run starts a fresh pass
	Done bool `json:"done"`
}

const (
	sweepOrphanContractSliceSize = 50000
	// rows paged per run: ~10M rows is a couple of minutes of slices, so a full
	// ~2.6B-row pass lands over roughly a week of resume runs
	sweepOrphanContractMaxRowCount = 10 * 1000 * 1000
	// gap between the resume runs of one pass
	sweepOrphanContractResumeTimeout = 30 * time.Minute
	// bounds a stranded claim if a worker dies mid-run (SIGNALS 12.3); with the
	// row budget sized as above a run is minutes, so this is pure headroom
	sweepOrphanContractMaxTime = 30 * time.Minute
)

// ScheduleSweepOrphanContractData starts a fresh pass at the next weekly slot.
func ScheduleSweepOrphanContractData(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSweepOrphanContractData(
		clientSession,
		tx,
		model.SweepOrphanCursor{},
		// weekly, anchored off-peak (~10:00 UTC): steady state finds ~zero
		// orphans (RemoveCompletedContracts cascades dependents inline), so a
		// weekly pass is plenty and `bringyourctl db sweep-orphans` covers
		// on-demand cleanup
		nextWeeklyOffPeak(server.NowUtc()),
	)
}

func scheduleSweepOrphanContractData(
	clientSession *session.ClientSession,
	tx server.PgTx,
	cursor model.SweepOrphanCursor,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		SweepOrphanContractData,
		&SweepOrphanContractDataArgs{
			Cursor: cursor,
		},
		clientSession,
		task.RunOnce("sweep_orphan_contract_data"),
		task.RunAt(runAt),
		task.MaxTime(sweepOrphanContractMaxTime),
	)
}

func SweepOrphanContractData(
	sweepOrphanContractData *SweepOrphanContractDataArgs,
	clientSession *session.ClientSession,
) (*SweepOrphanContractDataResult, error) {
	// the model fn pages each child table by primary key in sliceSize batches,
	// one maintenance tx per slice -- unlike the pre-2026-07-14
	// NOT EXISTS ... LIMIT form, which full-scanned each driver table when
	// orphans were rare (prod incident 2026-07-14)
	removedCount, cursor, done := model.SweepOrphanContractData(
		clientSession.Ctx,
		sweepOrphanContractData.Cursor,
		sweepOrphanContractMaxRowCount,
		sweepOrphanContractSliceSize,
	)
	return &SweepOrphanContractDataResult{
		RemovedCount: removedCount,
		Cursor:       cursor,
		Done:         done,
	}, nil
}

func SweepOrphanContractDataPost(
	sweepOrphanContractData *SweepOrphanContractDataArgs,
	sweepOrphanContractDataResult *SweepOrphanContractDataResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if sweepOrphanContractDataResult.Done {
		// pass complete; start the next one on the weekly cadence
		ScheduleSweepOrphanContractData(clientSession, tx)
		return nil
	}
	// mid-pass: resume from where this run stopped
	scheduleSweepOrphanContractData(
		clientSession,
		tx,
		sweepOrphanContractDataResult.Cursor,
		server.NowUtc().Add(sweepOrphanContractResumeTimeout),
	)
	return nil
}

// Reconcile net escrow
//
// The redis net escrow counter is an approximate, expiring mirror. Dropped
// mirror posts can still create drift within its lifetime, so this periodically
// compares each active balance with PostgreSQL and applies an additive
// correction. Page-local source snapshots and additive corrections are
// required; see model.ReconcileNetEscrow and SIGNALS.md §5.11.

type ReconcileNetEscrowArgs struct {
}

type ReconcileNetEscrowResult struct {
}

func ScheduleReconcileNetEscrow(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		ReconcileNetEscrow,
		&ReconcileNetEscrowArgs{},
		clientSession,
		task.RunOnce("reconcile_net_escrow"),
		task.RunAt(server.NowUtc().Add(5*time.Minute)),
		task.MaxTime(30*time.Minute),
	)
}

func ReconcileNetEscrow(
	reconcileNetEscrow *ReconcileNetEscrowArgs,
	clientSession *session.ClientSession,
) (*ReconcileNetEscrowResult, error) {
	driftByNetworkId, balanceCount := model.ReconcileCachedNetEscrow(clientSession.Ctx)

	overReserved := model.ByteCount(0)
	underReserved := model.ByteCount(0)
	for _, drift := range driftByNetworkId {
		if 0 < drift {
			overReserved += drift
		} else {
			underReserved += -drift
		}
	}
	glog.Infof(
		"[sm]reconcile net escrow: %d balances, %d networks drifted, over-reserved %s, under-reserved %s\n",
		balanceCount,
		len(driftByNetworkId),
		model.ByteCountHumanReadable(overReserved),
		model.ByteCountHumanReadable(underReserved),
	)
	return &ReconcileNetEscrowResult{}, nil
}

func ReconcileNetEscrowPost(
	reconcileNetEscrow *ReconcileNetEscrowArgs,
	reconcileNetEscrowResult *ReconcileNetEscrowResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleReconcileNetEscrow(clientSession, tx)
	return nil
}

type CleanupExpiredPaymentIntentsArgs struct {
}

type CleanupExpiredPaymentIntentsResult struct {
}

func ScheduleCleanupExpiredPaymentIntents(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		CleanupExpiredPaymentIntents,
		&CleanupExpiredPaymentIntentsArgs{},
		clientSession,
		// legacy key
		task.RunOnce("cleanup_expired_payment_intents"),
		task.RunAt(server.NowUtc().Add(15*time.Minute)),
		task.MaxTime(30*time.Minute),
	)
}

func CleanupExpiredPaymentIntents(
	cleanupExpiredPaymentIntents *CleanupExpiredPaymentIntentsArgs,
	clientSession *session.ClientSession,
) (*CleanupExpiredPaymentIntentsResult, error) {
	minTime := server.NowUtc().Add(-60 * time.Minute)
	err := model.CleanupExpiredPaymentIntents(
		clientSession.Ctx,
		minTime,
	)

	return &CleanupExpiredPaymentIntentsResult{}, err
}

func CleanupExpiredPaymentIntentsPost(
	cleanupExpiredPaymentIntents *CleanupExpiredPaymentIntentsArgs,
	cleanupExpiredPaymentIntentsResult *CleanupExpiredPaymentIntentsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleCleanupExpiredPaymentIntents(clientSession, tx)
	return nil
}
