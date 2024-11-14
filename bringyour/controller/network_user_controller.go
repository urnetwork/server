package controller

import (
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
)

type GetNetworkUserResult struct {
	NetworkUser *model.NetworkUser         `json:"network_user,omitempty"`
	Error       *GetNetworkUserResultError `json:"error,omitempty"`
}

type GetNetworkUserResultError struct {
	Message string `json:"message"`
}

func GetNetworkUser(
	clientSession *session.ClientSession,
) (*GetNetworkUserResult, error) {

	networkUser := model.GetNetworkUser(clientSession.Ctx, clientSession.ByJwt.UserId)
	if networkUser == nil {
		return &GetNetworkUserResult{
			Error: &GetNetworkUserResultError{
				Message: "No user found",
			},
		}, nil
	}

	return &GetNetworkUserResult{
		NetworkUser: networkUser,
	}, nil

}
