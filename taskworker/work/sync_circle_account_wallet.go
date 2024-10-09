package work

import (
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/task"
)

func SchedulePopulateAccountWallets(clientSession *session.ClientSession, tx bringyour.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		controller.PopulateAccountWallets,
		&controller.PopulateAccountWalletsArgs{},
		clientSession,
		task.RunOnce("populate_circle_account_wallets"),
		task.RunAt(time.Now().Add(24*time.Hour)),
	)
}

func PopulateAccountWalletsPost(
	populateAccountWallets *controller.PopulateAccountWalletsArgs,
	populateAccountWalletsResult *controller.PopulateAccountWalletsResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	SchedulePopulateAccountWallets(clientSession, tx)
	return nil
}
