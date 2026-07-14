package model

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/urnetwork/connect"
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
		connect.AssertEqual(t, err, nil)

		result, err := NetworkCreate(networkCreate, clientSession)
		connect.AssertEqual(t, err, nil)

		connect.AssertEqual(t, regex.MatchString(result.Network.NetworkName), true)

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
		connect.AssertNotEqual(t, network, nil)

		networkUser := GetNetworkUser(ctx, *network.AdminUserId)

		connect.AssertEqual(t, networkUser.AuthType, AuthTypeGuest)

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, upgradeGuestResult.Error, nil)
		connect.AssertEqual(t, upgradeGuestResult.VerificationRequired.UserAuth, userAuth)

		// fetch the network user and make sure it's no longer a guest
		networkUser = GetNetworkUser(ctx, userId)
		connect.AssertEqual(t, networkUser.AuthType, AuthTypePassword)

		// ensure network name has been updated
		network = GetNetwork(clientSession)
		connect.AssertEqual(t, network.NetworkName, upgradedNetworkName)

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
		connect.AssertNotEqual(t, network, nil)
		connect.AssertEqual(t, network.GuestUpgradeNetworkId, nil)

		userAuth := fmt.Sprintf("%s@bringyour.com", networkId) // pulled from Testing_CreateNetwork
		password := "password"                                 // pulled from Testing_CreateNetwork
		args := UpgradeGuestExistingArgs{
			UserAuth: &userAuth,
			Password: &password,
		}

		result, err := UpgradeFromGuestExisting(args, clientSession)
		connect.AssertEqual(t, err, nil)
		// fmt.Println("error UpgradeFromGuestExisting: ", result.Error.Message)
		connect.AssertEqual(t, result.Error, nil)

		// fetch the network
		// it should have the guest upgrade network id
		network = GetNetwork(clientSession)
		connect.AssertNotEqual(t, network, nil)

		connect.AssertEqual(t, network.GuestUpgradeNetworkId, networkId)

	})
}

func TestUpgradeGuestExistingWalletUser(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"

		privateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)
		publicKey := privateKey.PublicKey().String()

		challenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, challenge.Error, nil)
		signature, err := privateKey.Sign([]byte(challenge.MessageTemplate))
		connect.AssertEqual(t, err, nil)
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
		connect.AssertNotEqual(t, network, nil)
		connect.AssertEqual(t, network.GuestUpgradeNetworkId, nil)

		// login with a fresh challenge
		challenge = CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, challenge.Error, nil)
		signature, err = privateKey.Sign([]byte(challenge.MessageTemplate))
		connect.AssertEqual(t, err, nil)
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

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		// fetch the network
		// it should have the guest upgrade network id
		network = GetNetwork(clientSession)
		connect.AssertNotEqual(t, network, nil)

		connect.AssertEqual(t, network.GuestUpgradeNetworkId, networkId)

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
		connect.AssertEqual(t, err, nil)
		publicKey := privateKey.PublicKey().String()
		networkName := "abcdef"

		challenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, challenge.Error, nil)
		signature, err := privateKey.Sign([]byte(challenge.MessageTemplate))
		connect.AssertEqual(t, err, nil)
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
		connect.AssertEqual(t, networkUser.AuthType, AuthTypeGuest)
		connect.AssertEqual(t, networkUser.WalletAddress, nil)

		result, err := UpgradeGuest(args, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		network := GetNetwork(clientSession)
		connect.AssertNotEqual(t, network, nil)
		connect.AssertEqual(t, network.NetworkName, networkName)

		user := GetNetworkUser(ctx, *network.AdminUserId)
		connect.AssertEqual(t, user.AuthType, "solana")
		connect.AssertEqual(t, user.WalletAddress, publicKey)

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
		connect.AssertEqual(t, before.AuthType, AuthTypeGuest)
		connect.AssertEqual(t, before.WalletAddress, nil)

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Message, AgreeToTerms)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

		// fail
		// network name should be at least 6 characters
		networkUpdateArgs = NetworkUpdateArgs{
			NetworkName: "a",
		}
		result, err = NetworkUpdate(networkUpdateArgs, sourceSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

		// success
		newName := "uvwxyz"
		networkUpdateArgs = NetworkUpdateArgs{
			NetworkName: newName,
		}
		result, err = NetworkUpdate(networkUpdateArgs, sourceSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		network := GetNetwork(sourceSession)
		connect.AssertEqual(t, network.NetworkName, newName)

	})
}

func TestNetworkNameValidation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		// too short
		networkName := ""
		_, err := validateNetworkName(networkName)
		connect.AssertNotEqual(t, err, nil)

		// too long
		networkName = "a123456789012345678901234567890123456789012345678901"
		_, err = validateNetworkName(networkName)
		connect.AssertNotEqual(t, err, nil)

		/**
		 * testing special characters
		 */
		networkName = "abcde$"
		_, err = validateNetworkName(networkName)
		connect.AssertNotEqual(t, err, nil)

		networkName = "abcdeé"
		_, err = validateNetworkName(networkName)
		connect.AssertNotEqual(t, err, nil)

		networkName = "東京タワー"
		_, err = validateNetworkName(networkName)
		connect.AssertNotEqual(t, err, nil)

		// test spaces
		networkName = "abc def"
		expected := "abc-def"
		validated, err := validateNetworkName(networkName)
		connect.AssertEqual(t, validated, expected)

		// valid name should pass
		networkName = "abcdef"
		_, err = validateNetworkName(networkName)
		connect.AssertEqual(t, err, nil)

	})
}
