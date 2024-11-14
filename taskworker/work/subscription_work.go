package work

import (
	"time"

	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
	"github.com/urnetwork/server/bringyour/task"
)

// Backfill initial transfer balance

type BackfillInitialTransferBalanceArgs struct {
}

type BackfillInitialTransferBalanceResult struct {
}

func ScheduleBackfillInitialTransferBalance(clientSession *session.ClientSession, tx bringyour.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		BackfillInitialTransferBalance,
		&BackfillInitialTransferBalanceArgs{},
		clientSession,
		task.RunOnce("backfill_initial_transfer_balance"),
		task.RunAt(bringyour.NowUtc().Add(15*time.Minute)),
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
	tx bringyour.PgTx,
) error {
	return nil
}
