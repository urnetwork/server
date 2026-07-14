package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestNetworkUser(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()

		networkName := "hello_world"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		networkUser := GetNetworkUser(ctx, userId)

		connect.AssertNotEqual(t, networkUser, nil)
		connect.AssertEqual(t, networkUser.UserId, userId)
		connect.AssertEqual(t, networkUser.UserAuth, fmt.Sprintf("%s@bringyour.com", networkId))
		connect.AssertEqual(t, networkUser.Verified, true)
		connect.AssertEqual(t, networkUser.AuthType, AuthTypePassword)
		connect.AssertEqual(t, networkUser.NetworkName, networkName)

		// test for invalid user id
		userId = server.NewId()
		networkUser = GetNetworkUser(ctx, userId)
		connect.AssertEqual(t, networkUser, nil)

		// create guest network
		guestNetworkId := server.NewId()
		guestUserId := server.NewId()
		guestNetworkName := "guest_hello_world"

		Testing_CreateGuestNetwork(ctx, guestNetworkId, guestNetworkName, guestUserId)

		networkUser = GetNetworkUser(ctx, guestUserId)

		connect.AssertNotEqual(t, networkUser, nil)
		connect.AssertEqual(t, networkUser.UserId, guestUserId)
		connect.AssertEqual(t, networkUser.UserAuth, nil)
		connect.AssertEqual(t, networkUser.Verified, false)
		connect.AssertEqual(t, networkUser.AuthType, AuthTypeGuest)
		connect.AssertEqual(t, networkUser.NetworkName, guestNetworkName)

	})
}

func TestAddUserAuthPassword(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		// clientId := server.NewId()
		networkName := "abcdef"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		// userAuth := "hello@ur.io"

		password := "abcdefg1234567"
		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

		/**
		 * Testing_CreateNetwork creates a user auth
		 * Trying to add another email to the same user should fail
		 */
		userAuth := "hello@ur.io"

		err := addUserAuth(
			&AddUserAuthArgs{
				UserId:       userId,
				UserAuth:     &userAuth,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
			},
			ctx,
		)
		connect.AssertNotEqual(t, err, nil)

		networkUser := GetNetworkUser(ctx, userId)
		connect.AssertNotEqual(t, networkUser, nil)
		connect.AssertEqual(t, len(networkUser.UserAuths), 1)
		connect.AssertEqual(t, networkUser.UserAuths[0].AuthType, UserAuthTypeEmail)

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
			ctx,
		)
		connect.AssertEqual(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		connect.AssertNotEqual(t, networkUser, nil)
		connect.AssertEqual(t, len(networkUser.UserAuths), 2)
		connect.AssertEqual(t, strings.Contains(networkUser.UserAuths[1].UserAuth, userAuth), true) // adds "+1" to phone number, so doing a string check
		connect.AssertEqual(t, networkUser.UserAuths[1].AuthType, UserAuthTypePhone)

	})
}

func TestAddUserAuthWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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

		err := addWalletAuth(
			&AddWalletAuthArgs{
				WalletAuth: &args,
				UserId:     userId,
			},
			session.Ctx,
		)
		connect.AssertEqual(t, err, nil)

		/**
		 * Make sure it's being populated whe we fetch the network user
		 */
		networkUser := GetNetworkUser(ctx, userId)
		connect.AssertNotEqual(t, networkUser, nil)
		connect.AssertEqual(t, len(networkUser.WalletAuths), 1)
		connect.AssertEqual(t, networkUser.WalletAuths[0].WalletAddress, pk)

		/**
		 * Overwrite the wallet auth with a different public key
		 */
		pk = "26acoTqfWANX72SUvcTdPkDXdZXYCcHbRDLkFThViPLk"

		args = WalletAuthArgs{
			PublicKey:  pk,
			Signature:  "xQLllMyvXMb6zAp0DWOsViESau6WL/OPeT1nKmyD+OD8yjfxf5TY5AmfiO4edykSgbnFRhTKjjM9cLoPAFXMCw==",
			Message:    "Welcome to URnetwork",
			Blockchain: "solana",
		}

		err = addWalletAuth(
			&AddWalletAuthArgs{
				WalletAuth: &args,
				UserId:     userId,
			},
			ctx,
		)
		connect.AssertEqual(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		connect.AssertNotEqual(t, networkUser, nil)
		connect.AssertEqual(t, len(networkUser.WalletAuths), 1)
		connect.AssertEqual(t, networkUser.WalletAuths[0].WalletAddress, pk)

	})
}

func TestFindNetworkIdByEmail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"

		userAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

		retrievedNetworkId, err := FindNetworkIdByEmail(ctx, userAuth)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *retrievedNetworkId, networkId)

		/**
		 * Test SSO
		 */
		email := "hello@ur.io"
		networkId = server.NewId()
		userId = server.NewId()
		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeGoogle,
			UserAuth: email,
			UserName: "",
		}

		Testing_CreateNetworkSso(
			networkId,
			userId,
			parsedAuthJwt,
			ctx,
		)

		retrievedNetworkId, err = FindNetworkIdByEmail(ctx, email)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *retrievedNetworkId, networkId)

		/**
		 * Test not found
		 */

		retrievedNetworkId, err = FindNetworkIdByEmail(ctx, "unknown@email.com")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, retrievedNetworkId, nil)

	})
}

func TestFindNetworkIdByWalletAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "wallet_lookup"
		publicKey := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="
		message := "Welcome to URnetwork"

		Testing_CreateNetworkByWallet(
			ctx,
			networkId,
			networkName,
			userId,
			publicKey,
			signature,
			message,
		)

		retrievedNetworkId, err := FindNetworkIdByWalletAddress(ctx, publicKey)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *retrievedNetworkId, networkId)

		/**
		 * Test not found
		 */

		retrievedNetworkId, err = FindNetworkIdByWalletAddress(ctx, "unknown_wallet_address")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, retrievedNetworkId, nil)

	})
}
