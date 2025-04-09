package model_test

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
)

func TestNetworkCreateGuestMode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkCreate := model.NetworkCreateArgs{
			Terms:     true,
			GuestMode: true,
		}

		byJwt := jwt.ByJwt{}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		pattern := `^g[0-9a-f]+$`

		// Compile the regex
		regex, err := regexp.Compile(pattern)
		assert.Equal(t, err, nil)

		result, err := model.NetworkCreate(networkCreate, clientSession)
		assert.Equal(t, err, nil)

		assert.MatchRegex(t, result.Network.NetworkName, regex)

	})
}

func TestNetworkUpgradeGuestMode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()

		byJwt := jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		model.Testing_CreateGuestNetwork(
			ctx,
			networkId,
			"abcdef",
			userId,
		)

		// fetch the network user and make sure it's a guest

		network := model.GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)

		networkUser := model.GetNetworkUser(ctx, *network.AdminUserId)

		assert.Equal(t, networkUser.AuthType, model.AuthTypeGuest)

		// upgrade to non-guest

		userAuth := "test@ur.io"
		password := "abcdefg1234567"
		upgradedNetworkName := "abcdef1234"

		upgradeGuestArgs := model.UpgradeGuestArgs{
			NetworkName: upgradedNetworkName,
			UserAuth:    &userAuth,
			Password:    &password,
		}

		upgradeGuestResult, err := model.UpgradeGuest(
			upgradeGuestArgs,
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, upgradeGuestResult.Error, nil)
		assert.Equal(t, upgradeGuestResult.VerificationRequired.UserAuth, userAuth)

		// fetch the network user and make sure it's no longer a guest
		networkUser = model.GetNetworkUser(ctx, userId)
		assert.Equal(t, networkUser.AuthType, model.AuthTypePassword)

		// ensure network name has been updated
		network = model.GetNetwork(clientSession)
		assert.Equal(t, network.NetworkName, upgradedNetworkName)

	})
}

func TestUpgradeGuestExistingUser(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		// clientId := server.NewId()
		networkName := "abcdef"

		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		guestNetworkId := server.NewId()
		guestUserId := server.NewId()

		byJwt := jwt.ByJwt{
			NetworkId: guestNetworkId,
			UserId:    guestUserId,
		}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		model.Testing_CreateGuestNetwork(
			ctx,
			guestNetworkId,
			fmt.Sprintf("guest-%s", networkId.String()),
			guestUserId,
		)

		// fetch the network
		// it should have no guest upgrade network id
		network := model.GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)
		assert.Equal(t, network.GuestUpgradeNetworkId, nil)

		userAuth := fmt.Sprintf("%s@bringyour.com", networkId) // pulled from Testing_CreateNetwork
		password := "password"                                 // pulled from Testing_CreateNetwork
		args := model.UpgradeGuestExistingArgs{
			UserAuth: &userAuth,
			Password: &password,
		}

		result, err := model.UpgradeFromGuestExisting(args, clientSession)
		assert.Equal(t, err, nil)
		// fmt.Println("error UpgradeFromGuestExisting: ", result.Error.Message)
		assert.Equal(t, result.Error, nil)

		// fetch the network
		// it should have the guest upgrade network id
		network = model.GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)

		assert.Equal(t, network.GuestUpgradeNetworkId, networkId)

	})
}

func TestNetworkCreateTermsFail(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkCreate := model.NetworkCreateArgs{
			GuestMode: true,
		}

		byJwt := jwt.ByJwt{}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		result, err := model.NetworkCreate(networkCreate, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error.Message, model.AgreeToTerms)
	})
}

func TestNetworkUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		networkName := "abcdef"

		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// fail
		// network name unavailable
		networkUpdateArgs := model.NetworkUpdateArgs{
			NetworkName: networkName,
		}
		result, err := model.NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.Error, nil)

		// fail
		// network name should be at least 6 characters
		networkUpdateArgs = model.NetworkUpdateArgs{
			NetworkName: "a",
		}
		result, err = model.NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.Error, nil)

		// success
		newName := "uvwxyz"
		networkUpdateArgs = model.NetworkUpdateArgs{
			NetworkName: newName,
		}
		result, err = model.NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)

		network := model.GetNetwork(sourceSession)
		assert.Equal(t, network.NetworkName, newName)

	})
}
