package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/jwt"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
)

func TestGetNetworkUser(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := bringyour.NewId()
		clientId := bringyour.NewId()
		userId := bringyour.NewId()
		networkName := "abcdef"

		networkIdB := bringyour.NewId()
		// clientIdB := bringyour.NewId()
		userIdB := bringyour.NewId()
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
		assert.Equal(t, err, nil)
		networkUser := networkUserResult.NetworkUser
		assert.Equal(t, networkUser.UserId, userId)
		assert.Equal(t, networkUser.UserAuth, fmt.Sprintf("%s@bringyour.com", networkId))
		assert.Equal(t, networkUser.Verified, true)
		assert.Equal(t, networkUser.AuthType, model.AuthTypePassword)
	})
}
