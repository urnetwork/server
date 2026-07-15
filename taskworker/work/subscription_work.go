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

const DefaultCloseExpiredContractsBlockSize = 8

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
		n := 100000
		c, err := model.ForceCloseOpenContractIds(
			clientSession.Ctx,
			minTime,
			n,
			92,
			closeExpiredContracts.BlockSize,
			closeExpiredContracts.BlockIndex,
		)
		full := int64(n/(4*DefaultCloseExpiredContractsBlockSize)) <= c
		return &CloseExpiredContractsResult{
			Full: full,
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
// interrupted statements or older releases. Each pass is a full anti-join scan
// of the dependent tables, which is why it runs daily and not on the retention
// cadence.

type SweepOrphanContractDataArgs struct {
}

type SweepOrphanContractDataResult struct {
	RemovedCount int64 `json:"removed_count"`
}

func ScheduleSweepOrphanContractData(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SweepOrphanContractData,
		&SweepOrphanContractDataArgs{},
		clientSession,
		task.RunOnce("sweep_orphan_contract_data"),
		// weekly, anchored off-peak (~10:00 UTC): the bounded cursor sweep pages
		// entire large child tables per pass (measured ~2% of db time for the
		// provide_key slices alone) while finding ~zero orphans in steady state
		// -- a weekly safety net is plenty, and `bringyourctl db sweep-orphans`
		// covers on-demand cleanup
		task.RunAt(nextWeeklyOffPeak(server.NowUtc())),
		task.MaxTime(4*time.Hour),
	)
}

func SweepOrphanContractData(
	sweepOrphanContractData *SweepOrphanContractDataArgs,
	clientSession *session.ClientSession,
) (*SweepOrphanContractDataResult, error) {
	// Re-enabled 2026-07-14 on the bounded cursor implementation (daily safety
	// net for orphans left by crashes mid-delete or older releases; the reap_time
	// reaper cascades dependents with the contract delete, so steady state finds
	// ~nothing). The model fn pages each child table by primary key in sliceSize
	// batches, one maintenance tx per slice -- unlike the previous
	// NOT EXISTS ... LIMIT form, which full-scanned each driver table when
	// orphans were rare (prod incident 2026-07-14).
	sliceSize := 50000
	removedCount := model.SweepOrphanContractData(clientSession.Ctx, sliceSize)
	return &SweepOrphanContractDataResult{
		RemovedCount: removedCount,
	}, nil
}

func SweepOrphanContractDataPost(
	sweepOrphanContractData *SweepOrphanContractDataArgs,
	sweepOrphanContractDataResult *SweepOrphanContractDataResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSweepOrphanContractData(clientSession, tx)
	return nil
}

// Reconcile net escrow
//
// The redis net escrow counter is an approximate mirror with no ttl and no
// other reconciliation, so leaked reservations (dropped settle posts, crashes,
// quarantined closes) accumulate over the life of a balance and eventually
// surface as spurious "Insufficient balance" errors. This periodically resets
// each active balance's counter to the postgres source of truth.

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
