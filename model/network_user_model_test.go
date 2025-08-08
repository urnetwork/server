package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestNetworkUser(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()

		networkName := "hello_world"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		networkUser := GetNetworkUser(ctx, userId)

		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, networkUser.UserId, userId)
		assert.Equal(t, networkUser.UserAuth, fmt.Sprintf("%s@bringyour.com", networkId))
		assert.Equal(t, networkUser.Verified, true)
		assert.Equal(t, networkUser.AuthType, AuthTypePassword)
		assert.Equal(t, networkUser.NetworkName, networkName)

		// test for invalid user id
		userId = server.NewId()
		networkUser = GetNetworkUser(ctx, userId)
		assert.Equal(t, networkUser, nil)

		// create guest network
		guestNetworkId := server.NewId()
		guestUserId := server.NewId()
		guestNetworkName := "guest_hello_world"

		Testing_CreateGuestNetwork(ctx, guestNetworkId, guestNetworkName, guestUserId)

		networkUser = GetNetworkUser(ctx, guestUserId)

		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, networkUser.UserId, guestUserId)
		assert.Equal(t, networkUser.UserAuth, nil)
		assert.Equal(t, networkUser.Verified, false)
		assert.Equal(t, networkUser.AuthType, AuthTypeGuest)
		assert.Equal(t, networkUser.NetworkName, guestNetworkName)

	})
}
