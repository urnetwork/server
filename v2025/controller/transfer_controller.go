package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TransferStats(session *session.ClientSession) (*model.TransferStats, error) {
	return model.GetTransferStats(session.Ctx, session.ByJwt.NetworkId), nil
}
