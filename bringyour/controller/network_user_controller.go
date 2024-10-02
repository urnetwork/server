package controller

import (
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
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

type NetworkUserUpdateArgs struct {
	NetworkName string `json:"network_name"`
	UserName    string `json:"username"`
}

type NetworkUserUpdateError struct {
	Message string `json:"message"`
}

type NetworkUserUpdateResult struct {
	Error *NetworkUserUpdateError `json:"error,omitempty"`
}

func UpdateNetworkUser(
	args *NetworkUserUpdateArgs,
	clientSession *session.ClientSession,
) (*NetworkUserUpdateResult, error) {

	// get the current network name
	network := model.GetNetwork(clientSession)

	if network.NetworkName != args.NetworkName {
		// update the network name
		result, err := model.NetworkUpdate(
			model.NetworkUpdateArgs{NetworkName: args.NetworkName},
			clientSession,
		)
		if err != nil {
			return nil, err
		}

		if result.Error != nil {
			return &NetworkUserUpdateResult{
				Error: &NetworkUserUpdateError{
					Message: result.Error.Message,
				},
			}, nil
		}
	}

	// update the network_user username
	model.NetworkUserUpdate(args.UserName, clientSession)

	return &NetworkUserUpdateResult{}, nil
}
