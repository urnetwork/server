package controller

import (
	"fmt"

	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

func GetNetworkUser(
	clientSession *session.ClientSession,
) (*model.NetworkUser, error) {

	networkUser := model.GetNetworkUser(clientSession.Ctx, clientSession.ByJwt.UserId)
	if networkUser == nil {
		return nil, fmt.Errorf("no user found")
	}

	return networkUser, nil

}
