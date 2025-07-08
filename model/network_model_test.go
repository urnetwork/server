package model

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestNetworkCreateGuestMode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkCreate := NetworkCreateArgs{
			Terms:     true,
			GuestMode: true,
		}

		byJwt := jwt.ByJwt{}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		pattern := `^g[0-9a-f]+$`

		// Compile the regex
		regex, err := regexp.Compile(pattern)
		assert.Equal(t, err, nil)

		result, err := NetworkCreate(networkCreate, clientSession)
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

		Testing_CreateGuestNetwork(
			ctx,
			networkId,
			"abcdef",
			userId,
		)

		// fetch the network user and make sure it's a guest

		network := GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)

		networkUser := GetNetworkUser(ctx, *network.AdminUserId)

		assert.Equal(t, networkUser.AuthType, AuthTypeGuest)

		// upgrade to non-guest

		userAuth := "test@ur.io"
		password := "abcdefg1234567"
		upgradedNetworkName := "abcdef1234"

		upgradeGuestArgs := UpgradeGuestArgs{
			NetworkName: upgradedNetworkName,
			UserAuth:    &userAuth,
			Password:    &password,
		}

		upgradeGuestResult, err := UpgradeGuest(
			upgradeGuestArgs,
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, upgradeGuestResult.Error, nil)
		assert.Equal(t, upgradeGuestResult.VerificationRequired.UserAuth, userAuth)

		// fetch the network user and make sure it's no longer a guest
		networkUser = GetNetworkUser(ctx, userId)
		assert.Equal(t, networkUser.AuthType, AuthTypePassword)

		// ensure network name has been updated
		network = GetNetwork(clientSession)
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

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		guestNetworkId := server.NewId()
		guestUserId := server.NewId()

		byJwt := jwt.ByJwt{
			NetworkId: guestNetworkId,
			UserId:    guestUserId,
		}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		Testing_CreateGuestNetwork(
			ctx,
			guestNetworkId,
			fmt.Sprintf("guest-%s", networkId.String()),
			guestUserId,
		)

		// fetch the network
		// it should have no guest upgrade network id
		network := GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)
		assert.Equal(t, network.GuestUpgradeNetworkId, nil)

		userAuth := fmt.Sprintf("%s@bringyour.com", networkId) // pulled from Testing_CreateNetwork
		password := "password"                                 // pulled from Testing_CreateNetwork
		args := UpgradeGuestExistingArgs{
			UserAuth: &userAuth,
			Password: &password,
		}

		result, err := UpgradeFromGuestExisting(args, clientSession)
		assert.Equal(t, err, nil)
		// fmt.Println("error UpgradeFromGuestExisting: ", result.Error.Message)
		assert.Equal(t, result.Error, nil)

		// fetch the network
		// it should have the guest upgrade network id
		network = GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)

		assert.Equal(t, network.GuestUpgradeNetworkId, networkId)

	})
}

func TestUpgradeGuestExistingWalletUser(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"
		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"

		Testing_CreateNetworkByWallet(
			ctx,
			networkId,
			networkName,
			userId,
			pk,
		)

		guestNetworkId := server.NewId()
		guestUserId := server.NewId()

		byJwt := jwt.ByJwt{
			NetworkId: guestNetworkId,
			UserId:    guestUserId,
		}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		Testing_CreateGuestNetwork(
			ctx,
			guestNetworkId,
			fmt.Sprintf("guest-%s", networkId.String()),
			guestUserId,
		)

		// fetch the network
		// it should have no guest upgrade network id
		network := GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)
		assert.Equal(t, network.GuestUpgradeNetworkId, nil)

		args := UpgradeGuestExistingArgs{
			WalletAuth: &WalletAuthArgs{
				PublicKey:  pk,
				Signature:  "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw==",
				Message:    "Welcome to URnetwork",
				Blockchain: "solana",
			},
		}

		result, err := UpgradeFromGuestExisting(args, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)

		// fetch the network
		// it should have the guest upgrade network id
		network = GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)

		assert.Equal(t, network.GuestUpgradeNetworkId, networkId)

	})
}

func TestUpgradeGuestByWallet(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		// networkId := server.NewId()
		// userId := server.NewId()
		// clientId := server.NewId()
		// networkName := "abcdef"

		// model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		networkId := server.NewId()
		userId := server.NewId()

		byJwt := jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		Testing_CreateGuestNetwork(
			ctx,
			networkId,
			fmt.Sprintf("guest-%s", networkId.String()),
			userId,
		)

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		networkName := "abcdef"

		args := UpgradeGuestArgs{
			NetworkName: networkName,
			WalletAuth: &WalletAuthArgs{
				PublicKey:  pk,
				Signature:  "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw==",
				Message:    "Welcome to URnetwork",
				Blockchain: "solana",
			},
		}

		networkUser := GetNetworkUser(ctx, userId)
		assert.Equal(t, networkUser.AuthType, AuthTypeGuest)
		assert.Equal(t, networkUser.WalletAddress, nil)

		result, err := UpgradeGuest(args, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)

		network := GetNetwork(clientSession)
		assert.NotEqual(t, network, nil)
		assert.Equal(t, network.NetworkName, networkName)

		user := GetNetworkUser(ctx, *network.AdminUserId)
		assert.Equal(t, user.AuthType, "solana")
		assert.Equal(t, user.WalletAddress, pk)

	})
}

func TestNetworkCreateTermsFail(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkCreate := NetworkCreateArgs{
			GuestMode: true,
		}

		byJwt := jwt.ByJwt{}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		result, err := NetworkCreate(networkCreate, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error.Message, AgreeToTerms)
	})
}

func TestNetworkUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		networkName := "abcdef"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// fail
		// network name unavailable
		networkUpdateArgs := NetworkUpdateArgs{
			NetworkName: networkName,
		}
		result, err := NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.Error, nil)

		// fail
		// network name should be at least 6 characters
		networkUpdateArgs = NetworkUpdateArgs{
			NetworkName: "a",
		}
		result, err = NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.Error, nil)

		// success
		newName := "uvwxyz"
		networkUpdateArgs = NetworkUpdateArgs{
			NetworkName: newName,
		}
		result, err = NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)

		network := GetNetwork(sourceSession)
		assert.Equal(t, network.NetworkName, newName)

	})
}

func TestAddUserAuthPassword(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		networkName := "abcdef"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		userAuth := "hello@ur.io"
		password := "abcdefg1234567"

		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

		err := addUserAuth(
			&AddUserAuthArgs{
				UserId:       userId,
				UserAuth:     &userAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
			},
			session,
		)
		assert.Equal(t, err, nil)

		/**
		 * Trying to add another email to the same user should fail
		 */
		userAuth = "hello@ur.io"

		err = addUserAuth(
			&AddUserAuthArgs{
				UserId:       userId,
				UserAuth:     &userAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
			},
			session,
		)
		assert.NotEqual(t, err, nil)

		/**
		 * But adding a phone number should work
		 */
		userAuth = "1234567890"

		err = addUserAuth(
			&AddUserAuthArgs{
				UserId:       userId,
				UserAuth:     &userAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
			},
			session,
		)
		assert.Equal(t, err, nil)

		// todo - fetch user auths and make sure the user auth was added

	})
}

func TestAddUserAuthWallet(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		networkName := "abcdef"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"

		args := WalletAuthArgs{
			PublicKey:  pk,
			Signature:  "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw==",
			Message:    "Welcome to URnetwork",
			Blockchain: "solana",
		}

		err := AddWalletAuth(args, session)
		assert.Equal(t, err, nil)

		// todo - fetch user auths and make sure the wallet auth was added

	})
}
