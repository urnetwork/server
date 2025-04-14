package work

import (
	"time"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
	"github.com/urnetwork/server/v2025/task"
)

type CloseExpiredContractsArgs struct {
}

type CloseExpiredContractsResult struct {
}

func ScheduleCloseExpiredContracts(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		CloseExpiredContracts,
		&CloseExpiredContractsArgs{},
		clientSession,
		task.RunOnce("close_expired_contracts"),
		task.RunAt(time.Now().Add(1*time.Hour)),
	)
}

func CloseExpiredContracts(
	closeExpiredContracts *CloseExpiredContractsArgs,
	clientSession *session.ClientSession,
) (*CloseExpiredContractsResult, error) {
	err := model.ForceCloseOpenContractIds(clientSession.Ctx, 1*time.Hour)

	return &CloseExpiredContractsResult{}, err
}

func CloseExpiredContractsPost(
	closeExpiredContracts *CloseExpiredContractsArgs,
	closeExpiredContractsResult *CloseExpiredContractsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleCloseExpiredContracts(clientSession, tx)
	return nil
}
