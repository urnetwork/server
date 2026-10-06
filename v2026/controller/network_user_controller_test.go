package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestGetNetworkUser(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"

		networkIdB := server.NewId()
		// clientIdB := server.NewId()
		userIdB := server.NewId()
		networkNameB := "bcdefg"

		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		model.Testing_CreateNetwork(ctx, networkIdB, networkNameB, userIdB)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// it should fetch the network_user associated with the session userId
		networkUserResult, err := GetNetworkUser(userSession)
		connect.AssertEqual(t, err, nil)
		networkUser := networkUserResult.NetworkUser
		connect.AssertEqual(t, networkUser.UserId, userId)
		connect.AssertEqual(t, networkUser.UserAuth, fmt.Sprintf("%s@bringyour.com", networkId))
		connect.AssertEqual(t, networkUser.Verified, true)
		connect.AssertEqual(t, networkUser.AuthType, model.AuthTypePassword)
	})
}
