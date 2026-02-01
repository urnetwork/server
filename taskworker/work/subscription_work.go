package work

import (
	"fmt"
	mathrand "math/rand"
	"time"

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
		task.RunAt(server.NowUtc().Add(1*time.Minute)),
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
