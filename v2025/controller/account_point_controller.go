package controller

import (
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
)

type AccountPointsResult struct {
	AccountPoints []model.AccountPoint `json:"account_points"`
}

func GetAccountPoints(
	session *session.ClientSession,
) (*AccountPointsResult, error) {
	result := model.FetchAccountPoints(session.Ctx, session.ByJwt.NetworkId)

	return &AccountPointsResult{
		AccountPoints: result,
	}, nil
}
