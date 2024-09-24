package model

import (
	"context"
	"fmt"
	"testing"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
	"github.com/go-playground/assert/v2"
)

func TestNetworkUser(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := bringyour.NewId()
		userId := bringyour.NewId()
		clientId := bringyour.NewId()

		networkName := "hello_world"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		networkUser := GetNetworkUser(ctx, userId)

		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, networkUser.UserId, userId)
		assert.Equal(t, networkUser.UserName, "test")
		assert.Equal(t, networkUser.UserAuth, fmt.Sprintf("%s@bringyour.com", networkId))
		assert.Equal(t, networkUser.Verified, true)
		assert.Equal(t, networkUser.AuthType, AuthTypePassword)
		assert.Equal(t, networkUser.NetworkName, networkName)

		// update username
		updatedName := "Lorem Ipsum"
		NetworkUserUpdate(updatedName, sourceSession)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, networkUser.UserId, userId)
		assert.Equal(t, networkUser.UserName, updatedName)
		assert.Equal(t, networkUser.UserAuth, fmt.Sprintf("%s@bringyour.com", networkId))
		assert.Equal(t, networkUser.Verified, true)
		assert.Equal(t, networkUser.AuthType, AuthTypePassword)

		// test for invalid user id
		userId = bringyour.NewId()
		networkUser = GetNetworkUser(ctx, userId)
		assert.Equal(t, networkUser, nil)

	})
}
