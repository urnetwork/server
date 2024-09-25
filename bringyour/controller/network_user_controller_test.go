package controller

import (
	"context"
	"fmt"
	"testing"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"github.com/go-playground/assert/v2"
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
		assert.Equal(t, networkUser.UserName, "test")
		assert.Equal(t, networkUser.UserAuth, fmt.Sprintf("%s@bringyour.com", networkId))
		assert.Equal(t, networkUser.Verified, true)
		assert.Equal(t, networkUser.AuthType, model.AuthTypePassword)

		// should fail because network not greater than 5 characters
		updatedName := "usernameB"
		updateArgs := &NetworkUserUpdateArgs{
			NetworkName: "",
			UserName:    updatedName,
		}
		updateNetworkUserResult, err := UpdateNetworkUser(updateArgs, userSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, updateNetworkUserResult.Error, nil)

		networkUserResult, err = GetNetworkUser(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, networkUserResult.NetworkUser.UserName, "test")

		// should fail because network name unavailable
		updateArgs = &NetworkUserUpdateArgs{
			NetworkName: networkNameB,
			UserName:    updatedName,
		}
		updateNetworkUserResult, err = UpdateNetworkUser(updateArgs, userSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, updateNetworkUserResult.Error, nil)

		networkUserResult, err = GetNetworkUser(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, networkUserResult.NetworkUser.UserName, "test")

		// should update only the username since network name is unchanged
		updateArgs = &NetworkUserUpdateArgs{
			NetworkName: networkName,
			UserName:    updatedName,
		}
		updateNetworkUserResult, err = UpdateNetworkUser(updateArgs, userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, updateNetworkUserResult.Error, nil)

		networkUserResult, err = GetNetworkUser(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, networkUserResult.NetworkUser.UserName, updatedName)
		// should be the original network name
		assert.Equal(t, networkUserResult.NetworkUser.NetworkName, networkName)

		// should update both network name && username
		updatedNetworkName := "uvwxyz"
		updatedName = "hello world"
		updateArgs = &NetworkUserUpdateArgs{
			NetworkName: updatedNetworkName,
			UserName:    updatedName,
		}
		updateNetworkUserResult, err = UpdateNetworkUser(updateArgs, userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, updateNetworkUserResult.Error, nil)

		networkUserResult, err = GetNetworkUser(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, networkUserResult.NetworkUser.UserName, updatedName)
		assert.Equal(t, networkUserResult.NetworkUser.NetworkName, updatedNetworkName)

		network := model.GetNetwork(userSession)
		assert.Equal(t, network.NetworkId, userSession.ByJwt.NetworkId)
		assert.Equal(t, network.NetworkName, updatedNetworkName)

	})
}
