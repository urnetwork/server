package model

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestNetworkCreateGuestMode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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

		assert.Equal(t, regex.MatchString(result.Network.NetworkName), true)

	})
}

func TestNetworkUpgradeGuestMode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"

		privateKey, err := solana.NewRandomPrivateKey()
		assert.Equal(t, err, nil)
		publicKey := privateKey.PublicKey().String()

		challenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, challenge.Error, nil)
		signature, err := privateKey.Sign([]byte(challenge.MessageTemplate))
		assert.Equal(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		Testing_CreateNetworkByWallet(
			ctx,
			networkId,
			networkName,
			userId,
			publicKey,
			signatureB64,
			challenge.MessageTemplate,
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

		// login with a fresh challenge
		challenge = CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, challenge.Error, nil)
		signature, err = privateKey.Sign([]byte(challenge.MessageTemplate))
		assert.Equal(t, err, nil)
		signatureB64 = base64.StdEncoding.EncodeToString(signature[:])

		args := UpgradeGuestExistingArgs{
			WalletAuth: &WalletAuthArgs{
				PublicKey:  publicKey,
				Signature:  signatureB64,
				Message:    challenge.MessageTemplate,
				Blockchain: AuthTypeSolana,
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
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

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
			fmt.Sprintf("guest-%s", networkId.String()),
			userId,
		)

		privateKey, err := solana.NewRandomPrivateKey()
		assert.Equal(t, err, nil)
		publicKey := privateKey.PublicKey().String()
		networkName := "abcdef"

		challenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, challenge.Error, nil)
		signature, err := privateKey.Sign([]byte(challenge.MessageTemplate))
		assert.Equal(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		args := UpgradeGuestArgs{
			NetworkName: networkName,
			WalletAuth: &WalletAuthArgs{
				PublicKey:  publicKey,
				Signature:  signatureB64,
				Message:    challenge.MessageTemplate,
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
		assert.Equal(t, user.WalletAddress, publicKey)

	})
}

func TestNetworkCreateTermsFail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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

func TestNetworkNameValidation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		// too short
		networkName := ""
		_, err := validateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		// too long
		networkName = "a123456789012345678901234567890123456789012345678901"
		_, err = validateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		/**
		 * testing special characters
		 */
		networkName = "abcde$"
		_, err = validateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		networkName = "abcdeé"
		_, err = validateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		networkName = "東京タワー"
		_, err = validateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		// test spaces
		networkName = "abc def"
		expected := "abc-def"
		validated, err := validateNetworkName(networkName)
		assert.Equal(t, validated, expected)

		// valid name should pass
		networkName = "abcdef"
		_, err = validateNetworkName(networkName)
		assert.Equal(t, err, nil)

	})
}
