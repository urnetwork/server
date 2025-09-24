package controller

import (
	"context"
	"testing"
	"time"

	// "golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
)

func TestNetworkCreate(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, nil)

		referralNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, referralNetworkId, "referralNetwork", server.NewId())
		referralCode := model.CreateNetworkReferralCode(ctx, referralNetworkId)
		assert.NotEqual(t, referralCode, nil)

		userAuth := "foo@ur.io"
		password := "bar123456789Foo!"

		// check referral network has no points
		networkPoints := model.FetchAccountPoints(ctx, referralNetworkId)
		assert.Equal(t, len(networkPoints), 0)

		networkCreate := model.NetworkCreateArgs{
			UserName:     "",
			UserAuth:     &userAuth,
			Password:     &password,
			NetworkName:  "foobar",
			Terms:        true,
			GuestMode:    false,
			ReferralCode: &referralCode.ReferralCode,
		}
		result, err := NetworkCreate(networkCreate, session)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.Network, nil)

		// session.ByJwt.NetworkId = result.Network.NetworkId
		session.ByJwt = &jwt.ByJwt{
			NetworkId: result.Network.NetworkId,
		}

		// check referral network has points applied
		// networkPoints = model.FetchNetworkPoints(ctx, referralNetworkId)
		// assert.Equal(t, len(networkPoints), 1)
		// assert.Equal(t, networkPoints[0].NetworkId, referralNetworkId)
		// assert.Equal(t, networkPoints[0].Event, "referral")
		// assert.NotEqual(t, networkPoints[0].PointValue, 0)
		//
		// network name should not contain profanity
		network := model.GetNetwork(session)
		assert.NotEqual(t, network, nil)
		assert.Equal(t, network.ContainsProfanity, false)

		// check network referral
		networkReferral := model.GetReferralNetworkByChildNetworkId(ctx, result.Network.NetworkId)
		assert.Equal(t, networkReferral.Id, referralNetworkId)

		transferBalances := model.GetActiveTransferBalances(ctx, result.Network.NetworkId)
		assert.Equal(t, 1, len(transferBalances))
		transferBalance := transferBalances[0]
		assert.Equal(t, transferBalance.BalanceByteCount, RefreshFreeTransferBalance)
		assert.Equal(t, !transferBalance.StartTime.After(time.Now()), true)
		assert.Equal(t, time.Now().Before(transferBalance.EndTime), true)
	})
}

func TestNetworkCreateWithProfanity(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, nil)

		referralNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, referralNetworkId, "referralNetwork", server.NewId())

		userAuth := "foo@ur.io"
		password := "bar123456789Foo!"
		referralCode := ""

		networkCreate := model.NetworkCreateArgs{
			UserName:     "",
			UserAuth:     &userAuth,
			Password:     &password,
			NetworkName:  "shitty", // must be at least 6 characters
			Terms:        true,
			GuestMode:    false,
			ReferralCode: &referralCode,
		}
		result, err := NetworkCreate(networkCreate, session)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.Network, nil)

		session.ByJwt = &jwt.ByJwt{
			NetworkId: result.Network.NetworkId,
		}

		// check network contains profanity
		network := model.GetNetwork(session)
		assert.NotEqual(t, network, nil)
		assert.Equal(t, network.ContainsProfanity, true)
	})
}

func TestNetworkNameUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"

		networkIdB := server.NewId()
		userIdB := server.NewId()
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
