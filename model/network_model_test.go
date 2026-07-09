package model

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"testing"

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
		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		message := "Welcome to URnetwork"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="

		Testing_CreateNetworkByWallet(
			ctx,
			networkId,
			networkName,
			userId,
			pk,
			signature,
			message,
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
				Signature:  signature,
				Message:    message,
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

// TestUpgradeGuestByWalletInvalidSignatureRejected reproduces a Class-A auth bug
// adjacent to the adopt_secret / confirm-share fixes: UpgradeGuest's wallet branch
// (network_model.go ~1261) binds WalletAuth.PublicKey to the caller's own account
// WITHOUT ever calling VerifySignature — unlike NetworkCreate's wallet branch
// (network_model.go:365) and handleLoginWallet (auth_model.go:457), which both verify.
// So a guest can claim any wallet address (e.g. a victim's) with a bogus signature,
// squatting the wallet identity and blocking the real owner from ever registering it
// (networkCreateWalletAuth gates on network_user.wallet_address).
//
// This asserts the secure expectation (invalid signature => wallet NOT bound), so it
// FAILS on the current code and will pass once UpgradeGuest verifies the signature.
func TestUpgradeGuestByWalletInvalidSignatureRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})
		Testing_CreateGuestNetwork(
			ctx,
			networkId,
			fmt.Sprintf("guest-%s", networkId.String()),
			userId,
		)

		// A real-format Solana address the caller does NOT control, paired with a
		// well-formed but cryptographically invalid signature (64 zero bytes).
		victimWallet := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		invalidSignature := base64.StdEncoding.EncodeToString(make([]byte, 64))

		before := GetNetworkUser(ctx, userId)
		assert.Equal(t, before.AuthType, AuthTypeGuest)
		assert.Equal(t, before.WalletAddress, nil)

		args := UpgradeGuestArgs{
			NetworkName: "abcdef",
			WalletAuth: &WalletAuthArgs{
				PublicKey:  victimWallet,
				Signature:  invalidSignature,
				Message:    "Welcome to URnetwork",
				Blockchain: "solana",
			},
		}
		result, err := UpgradeGuest(args, clientSession)

		// Security invariant: without a valid signature proving control of the key,
		// the guest account must NOT be bound to that wallet address.
		user := GetNetworkUser(ctx, userId)
		if user.WalletAddress != nil {
			t.Fatalf("SECURITY: UpgradeGuest bound wallet_address=%q to the guest account with an "+
				"INVALID signature — no proof of key control. The wallet branch never calls "+
				"VerifySignature (NetworkCreate and handleLoginWallet do). auth_type=%q err=%v result=%+v",
				*user.WalletAddress, user.AuthType, err, result)
		}
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
