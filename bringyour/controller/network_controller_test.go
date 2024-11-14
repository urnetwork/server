package controller

import (
	"context"
	"testing"
	"time"

	// "golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/jwt"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
)

func TestNetworkCreate(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, nil)

		userAuth := "foo@ur.io"
		password := "bar123456789Foo!"
		networkCreate := model.NetworkCreateArgs{
			UserName:    "",
			UserAuth:    &userAuth,
			Password:    &password,
			NetworkName: "foobar",
			Terms:       true,
			GuestMode:   false,
		}
		result, err := NetworkCreate(networkCreate, session)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.Network, nil)

		transferBalances := model.GetActiveTransferBalances(ctx, result.Network.NetworkId)
		assert.Equal(t, 1, len(transferBalances))
		transferBalance := transferBalances[0]
		assert.Equal(t, transferBalance.BalanceByteCount, RefreshFreeTransferBalance)
		assert.Equal(t, !transferBalance.StartTime.After(time.Now()), true)
		assert.Equal(t, time.Now().Before(transferBalance.EndTime), true)
	})
}

func TestNetworkNameUpdate(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := bringyour.NewId()
		clientId := bringyour.NewId()
		userId := bringyour.NewId()
		networkName := "abcdef"

		networkIdB := bringyour.NewId()
		userIdB := bringyour.NewId()
		networkNameB := "bcdefg"

		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		model.Testing_CreateNetwork(ctx, networkIdB, networkNameB, userIdB)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// should fail because network not greater than 5 characters
		updateArgs := &UpdateNetworkNameArgs{
			NetworkName: "",
		}
		updateNetworkUserResult, err := UpdateNetworkName(updateArgs, userSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, updateNetworkUserResult.Error, nil)

		networkResult := model.GetNetwork(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, networkResult.NetworkName, networkName)

		// should fail because network name unavailable
		updateArgs = &UpdateNetworkNameArgs{
			NetworkName: networkNameB,
		}
		updateNetworkUserResult, err = UpdateNetworkName(updateArgs, userSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, updateNetworkUserResult.Error, nil)

		networkResult = model.GetNetwork(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, networkResult.NetworkName, networkName)

		// should update the network name
		updatedNetworkName := "uvwxyz"
		updateArgs = &UpdateNetworkNameArgs{
			NetworkName: updatedNetworkName,
		}
		updateNetworkUserResult, err = UpdateNetworkName(updateArgs, userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, updateNetworkUserResult.Error, nil)

		networkResult = model.GetNetwork(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, networkResult.NetworkName, updatedNetworkName)
	})
}
