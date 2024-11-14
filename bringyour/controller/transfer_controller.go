package controller

import (
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
)

func TransferStats(session *session.ClientSession) (*model.TransferStats, error) {
	return model.GetTransferStats(session.Ctx, session.ByJwt.NetworkId), nil
}
