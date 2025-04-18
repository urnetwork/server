package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type NetworkPointsResult struct {
	NetworkPoints []model.NetworkPoint `json:"network_points"`
}

func GetNetworkPoints(
	session *session.ClientSession,
) (*NetworkPointsResult, error) {
	result := model.FetchNetworkPoints(session.Ctx, session.ByJwt.NetworkId)

	return &NetworkPointsResult{
		NetworkPoints: result,
	}, nil
}
