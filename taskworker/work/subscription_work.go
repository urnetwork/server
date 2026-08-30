package work

import (
	"fmt"
	mathrand "math/rand"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
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
	// survived, but the task retried with a Timeout and had to rescan. A 25k
	// cohort keeps the same worker parallelism while committing scheduler
	// progress four times as often.
	closeExpiredContractsMaxCount = 25_000
	closeExpiredContractsParallel = 92
)

func closeExpiredContractsFull(closeCount int64) bool {
	return int64(closeExpiredContractsMaxCount/(4*DefaultCloseExpiredContractsBlockSize)) <= closeCount
}

type CloseExpiredContractsArgs struct {
	BlockSize  int `json:"block_size"`
	BlockIndex int `json:"block_index"`
}

type CloseExpiredContractsResult struct {
	Full bool `json:"full"`
}

func ScheduleCloseExpiredContracts(clientSession *session.ClientSession, tx server.PgTx, blockIndex int, delay bool) {
	// runAt := func() time.Time {
	// 	now := server.NowUtc()
	// 	year, month, day := now.Date()
	// 	hour, minute, _ := now.Clock()
	// 	return time.Date(year, month, day, hour, minute + 1, 0, 0, time.UTC)
	// }()

	blockSize := DefaultCloseExpiredContractsBlockSize
	blockIndex = blockIndex % blockSize

	runAt := server.NowUtc()
	if delay {
		randomDelay := time.Minute + time.Duration(mathrand.Int63n(int64(4*time.Minute)))
		runAt = runAt.Add(randomDelay)
	}

	task.ScheduleTaskInTx(
		tx,
		CloseExpiredContracts,
		&CloseExpiredContractsArgs{
			BlockSize:  blockSize,
			BlockIndex: blockIndex,
		},
		clientSession,
		// legacy key
		task.RunOnce(fmt.Sprintf("close_expired_contracts_%d_%d", blockSize, blockIndex)),
		task.RunAt(runAt),
		task.MaxTime(30*time.Minute),
		task.Priority(task.TaskPriorityFastest),
	)
}

func CloseExpiredContracts(
	closeExpiredContracts *CloseExpiredContractsArgs,
	clientSession *session.ClientSession,
) (*CloseExpiredContractsResult, error) {
	if closeExpiredContracts.BlockSize == DefaultCloseExpiredContractsBlockSize {
		minTime := server.NowUtc().Add(-5 * time.Minute)
		c, err := model.ForceCloseOpenContractIds(
			clientSession.Ctx,
			minTime,
			closeExpiredContractsMaxCount,
			closeExpiredContractsParallel,
			closeExpiredContracts.BlockSize,
			closeExpiredContracts.BlockIndex,
		)
		return &CloseExpiredContractsResult{
			Full: closeExpiredContractsFull(c),
		}, err
	}
	// else ignore lingering tasks with older block size
	return &CloseExpiredContractsResult{}, nil
}

func CloseExpiredContractsPost(
	closeExpiredContracts *CloseExpiredContractsArgs,
	closeExpiredContractsResult *CloseExpiredContractsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleCloseExpiredContracts(clientSession, tx, closeExpiredContracts.BlockIndex, !closeExpiredContractsResult.Full)
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
	for _, networkId := range networkIds {
		// add initial transfer balance
		controller.AddRefreshTransferBalance(clientSession.Ctx, networkId)
	}
	return &BackfillInitialTransferBalanceResult{}, nil
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
	driftByNetworkId, balanceCount := model.ReconcileNetEscrow(clientSession.Ctx, true)

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
