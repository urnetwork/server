package controller

import (
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

func TransferStats(session *session.ClientSession) (*model.TransferStats, error) {
	return model.GetTransferStats(session.Ctx, session.ByJwt.NetworkId), nil
}
