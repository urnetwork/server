package work

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func SchedulePopulateAccountWallets(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		controller.PopulateAccountWallets,
		&controller.PopulateAccountWalletsArgs{},
		clientSession,
		task.RunOnce("populate_circle_account_wallets"),
		task.RunAt(server.NowUtc().Add(24*time.Hour)),
	)
}

func PopulateAccountWalletsPost(
	populateAccountWallets *controller.PopulateAccountWalletsArgs,
	populateAccountWalletsResult *controller.PopulateAccountWalletsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	SchedulePopulateAccountWallets(clientSession, tx)
	return nil
}
