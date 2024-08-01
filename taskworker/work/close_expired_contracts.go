package work

import (
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/task"
)

type CloseExpiredContractsArgs struct {
}

type CloseExpiredContractsResult struct {
}

func ScheduleCloseExpiredContracts(clientSession *session.ClientSession, tx bringyour.PgTx) {
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
	expiredContracts := model.GetExpiredTransferContracts(clientSession.Ctx)

	for _, expiredContract := range expiredContracts {

		var targetId *bringyour.Id
		if expiredContract.Party == model.ContractPartySource {
			targetId = &expiredContract.DestinationId
		} else if expiredContract.Party == model.ContractPartyDestination {
			targetId = &expiredContract.SourceId
		}

		if targetId == nil {
			continue
		}

		// forcing the contract to be closed
		// due to lack of response from the counterparty
		err := model.CloseContract(
			clientSession.Ctx,
			expiredContract.ContractId,
			*targetId,
			expiredContract.UsedTransferByteCount,
			false,
		)
		if err != nil {
			return nil, err
		}
	}

	return &CloseExpiredContractsResult{}, nil
}

func CloseExpiredContractsPost(
	closeExpiredContracts *CloseExpiredContractsArgs,
	closeExpiredContractsResult *CloseExpiredContractsResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	ScheduleCloseExpiredContracts(clientSession, tx)
	return nil
}
