package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/session"
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

func TestAddUserAuthPassword(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
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
		assert.NotEqual(t, err, nil)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, networkUser.UserAuths[0].AuthType, UserAuthTypeEmail)

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
		assert.Equal(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 2)
		assert.Equal(t, strings.Contains(networkUser.UserAuths[1].UserAuth, userAuth), true) // adds "+1" to phone number, so doing a string check
		assert.Equal(t, networkUser.UserAuths[1].AuthType, UserAuthTypePhone)

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

		err := addWalletAuth(
			&AddWalletAuthArgs{
				WalletAuth: &args,
				UserId:     userId,
			},
			session.Ctx,
		)
		assert.Equal(t, err, nil)

		/**
		 * Make sure it's being populated whe we fetch the network user
		 */
		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.WalletAuths), 1)
		assert.Equal(t, networkUser.WalletAuths[0].WalletAddress, pk)

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
		assert.Equal(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.WalletAuths), 1)
		assert.Equal(t, networkUser.WalletAuths[0].WalletAddress, pk)

	})
}

func TestFindNetworkIdByEmail(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"

		userAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

		retrievedNetworkId, err := FindNetworkIdByEmail(ctx, userAuth)
		assert.Equal(t, err, nil)
		assert.Equal(t, *retrievedNetworkId, networkId)

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
		assert.Equal(t, err, nil)
		assert.Equal(t, *retrievedNetworkId, networkId)

		/**
		 * Test not found
		 */

		retrievedNetworkId, err = FindNetworkIdByEmail(ctx, "unknown@email.com")
		assert.Equal(t, err, nil)
		assert.Equal(t, retrievedNetworkId, nil)

	})
}
