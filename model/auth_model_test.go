package model

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestGetUserAuth(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		testingUserAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

		userAuth, err := GetUserAuth(ctx, networkId)
		assert.Equal(t, err, nil)
		assert.Equal(t, userAuth, testingUserAuth)
	})
}

func TestResetPassword(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		isPro := false

		testingUserAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			networkName,
			guestMode,
			isPro,
		)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)

		// add a phone user auth to the network user
		phone := "16097370000"
		password := "password"
		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

		addUserAuth(
			&AddUserAuthArgs{
				UserId:       userId,
				UserAuth:     &phone,
				PasswordHash: passwordHash,
				PasswordSalt: passwordSalt,
				Verified:     true,
			},
			ctx,
		)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 2)

		passwordResetCreateCodeResult, err := AuthPasswordResetCreateCode(
			AuthPasswordResetCreateCodeArgs{
				UserAuth: testingUserAuth,
			},
			clientSession,
		)
		assert.Equal(t, err, nil)

		newPassword := "testagain"

		result, err := AuthPasswordSet(
			AuthPasswordSetArgs{
				ResetCode: *passwordResetCreateCodeResult.ResetCode,
				Password:  newPassword,
			},
			clientSession,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.NetworkId, nil)

		userAuths, err := getUserAuths(userId, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(userAuths), 2)

		for _, userAuth := range userAuths {
			loginPasswordHash := computePasswordHashV1([]byte(newPassword), userAuth.PasswordSalt)
			assert.Equal(t, bytes.Equal(userAuth.PasswordHash, loginPasswordHash), true)
		}

	})
}

func TestAuthCode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		isPro := false

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			networkName,
			guestMode,
			isPro,
		)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)

		authCodeCreate := &AuthCodeCreateArgs{}

		authCodeCreateResult, err := AuthCodeCreate(authCodeCreate, clientSession)
		assert.Equal(t, err, nil)

		assert.NotEqual(t, authCodeCreateResult.AuthCode, "")

		// now try to redeem the code

		authCodeLogin := &AuthCodeLoginArgs{
			AuthCode: authCodeCreateResult.AuthCode,
		}

		authCodeLoginResult, err := AuthCodeLogin(authCodeLogin, clientSession)
		assert.Equal(t, err, nil)

		assert.NotEqual(t, authCodeLoginResult.ByJwt, "")

		// the second redeem should fail

		authCodeLogin2 := &AuthCodeLoginArgs{
			AuthCode: authCodeCreateResult.AuthCode,
		}

		authCodeLoginResult2, err := AuthCodeLogin(authCodeLogin2, clientSession)
		assert.Equal(t, err, nil)

		assert.Equal(t, authCodeLoginResult2.ByJwt, "")
		assert.NotEqual(t, authCodeLoginResult2.Error, nil)

		RemoveExpiredAuthCodes(ctx, server.NowUtc())
	})
}

func TestVerifySolanaSignature(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="
		message := "Welcome to URnetwork"

		isValid, err := VerifySolanaSignature(pk, message, signature)
		assert.Equal(t, err, nil)
		assert.Equal(t, isValid, true)

		// now test with an invalid signature
		invalidSignature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw"

		isValid, err = VerifySolanaSignature(pk, message, invalidSignature)
		assert.NotEqual(t, err, nil)
		assert.Equal(t, isValid, false)

	})
}

func TestUserAuthLogin(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		userAuth := fmt.Sprintf("%s@bringyour.com", networkId)

		result, err := loginUserAuth(&userAuth, ctx)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)
		assert.Equal(t, result.UserAuth, userAuth)
		assert.NotEqual(t, result.AuthAllowed, nil)

		for _, authAllowed := range *result.AuthAllowed {
			assert.NotEqual(t, authAllowed, "")
			glog.Infof("Auth allowed: %s", authAllowed)
		}

		assert.Equal(t, len(*result.AuthAllowed), 2)
		authAllowed := (*result.AuthAllowed)[0]
		assert.Equal(t, UserAuthType(authAllowed), UserAuthTypeEmail)
		authAllowed = (*result.AuthAllowed)[1]
		assert.Equal(t, authAllowed, "password")

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 0)

		/**
		 * Login with SSO with same userAuth should work
		 */
		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeGoogle,
			UserAuth: userAuth,
			UserName: "",
		}
		useAuthAttemptId := server.NewId()

		result, err = handleLoginParsedAuthJwt(
			&HandleLoginParsedAuthJwtArgs{
				AuthJwt: parsedAuthJwt,
				// AuthJwtType:       SsoAuthTypeGoogle,
				AuthJwtStr:        "",
				UserAuthAttemptId: useAuthAttemptId,
			},
			ctx,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)
		assert.NotEqual(t, result.Network.ByJwt, nil)

		// the login should have created a SSO auth
		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 1)

	})
}

func TestLoginWithWallet(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="
		message := "Welcome to URnetwork"

		Testing_CreateNetworkByWallet(
			ctx,
			networkId,
			networkName,
			userId,
			pk,
			signature,
			message,
		)

		result, err := handleLoginWallet(&WalletAuthArgs{
			PublicKey:  pk,
			Signature:  signature,
			Message:    message,
			Blockchain: AuthTypeSolana,
		}, ctx)

		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)
		assert.NotEqual(t, result.Network.ByJwt, nil)

	})
}

// test social logins
func TestSocialLogin(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		useAuthAttemptId := server.NewId()

		email := "hello@bringyour.com"

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

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 0)
		assert.Equal(t, len(networkUser.SsoAuths), 1)

		// login
		result, err := handleLoginParsedAuthJwt(
			&HandleLoginParsedAuthJwtArgs{
				AuthJwt:           parsedAuthJwt,
				AuthJwtStr:        "",
				UserAuthAttemptId: useAuthAttemptId,
			},
			ctx,
		)

		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.Network.ByJwt, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 0)
		assert.Equal(t, len(networkUser.SsoAuths), 1)

		// logging in with an Apple SSO auth should work too
		parsedAuthJwt = AuthJwt{
			AuthType: SsoAuthTypeApple,
			UserAuth: email,
			UserName: "",
		}
		result, err = handleLoginParsedAuthJwt(
			&HandleLoginParsedAuthJwtArgs{
				AuthJwt:           parsedAuthJwt,
				AuthJwtStr:        "",
				UserAuthAttemptId: useAuthAttemptId,
			},
			ctx,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)

		/**
		 * Should now have 2 SSO auths
		 */
		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 0)
		assert.Equal(t, len(networkUser.SsoAuths), 2)

	})
}

func TestAddingSsoToDifferentNetworksShouldFail(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		walletNetworkId := server.NewId()
		walletNetworkUserId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "network_a", userId)

		email := fmt.Sprintf("%s@bringyour.com", networkId)

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="
		message := "Welcome to URnetwork"

		Testing_CreateNetworkByWallet(ctx, walletNetworkId, "wallet_network", walletNetworkUserId, pk, signature, message)

		/**
		 * adding SSO to wallet_network with email associated with network_a should fail
		 */
		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeApple,
			UserAuth: email,
			UserName: "",
		}

		err := addSsoAuth(&AddSsoAuthArgs{
			ParsedAuthJwt: parsedAuthJwt,
			AuthJwt:       "",
			AuthJwtType:   SsoAuthTypeGoogle,
			UserId:        walletNetworkUserId,
		}, ctx)
		assert.NotEqual(t, err, nil)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 0)
		assert.Equal(t, len(networkUser.WalletAuths), 0)

		walletNetworkUser := GetNetworkUser(ctx, walletNetworkUserId)
		assert.NotEqual(t, walletNetworkUser, nil)
		assert.Equal(t, len(walletNetworkUser.UserAuths), 0)
		assert.Equal(t, len(walletNetworkUser.SsoAuths), 0)
		assert.Equal(t, len(walletNetworkUser.WalletAuths), 1)

		/**
		 * add a SSO to the email network should work
		 */
		err = addSsoAuth(&AddSsoAuthArgs{
			ParsedAuthJwt: parsedAuthJwt,
			AuthJwt:       "",
			AuthJwtType:   SsoAuthTypeGoogle,
			UserId:        userId,
		}, ctx)
		assert.Equal(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 1)
		assert.Equal(t, len(networkUser.WalletAuths), 0)

	})
}

func TestAddingSameSsoToNetworkShouldFail(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "network_a", userId)

		email := fmt.Sprintf("%s@bringyour.com", networkId)

		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeApple,
			UserAuth: email,
			UserName: "",
		}

		addSsoAuthArgs := &AddSsoAuthArgs{
			ParsedAuthJwt: parsedAuthJwt,
			AuthJwt:       "",
			AuthJwtType:   SsoAuthTypeGoogle,
			UserId:        userId,
		}

		err := addSsoAuth(addSsoAuthArgs, ctx)
		assert.Equal(t, err, nil)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 1)
		assert.Equal(t, len(networkUser.WalletAuths), 0)

		/**
		 * Trying to add the same SSO auth again should fail
		 */
		err = addSsoAuth(addSsoAuthArgs, ctx)
		assert.NotEqual(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.Equal(t, len(networkUser.SsoAuths), 1)

	})
}

func TestAddingSameUserAuthToNetworkShouldFail(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "network_a", userId)

		email := fmt.Sprintf("%s@bringyour.com", networkId)
		password := "password123"
		passwordSalt := createPasswordSalt()
		passwordHash := computePasswordHashV1([]byte(password), passwordSalt)

		networkUser := GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)

		/**
		 * Trying to add the same user auth again should fail
		 */
		args := &AddUserAuthArgs{
			UserId:       userId,
			UserAuth:     &email,
			PasswordHash: passwordHash,
			PasswordSalt: passwordSalt,
			Verified:     true,
		}

		err := addUserAuth(args, ctx)
		assert.NotEqual(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)

		/**
		 * But adding a phone user auth should work
		 */
		phoneNumber := "16097370000"
		args = &AddUserAuthArgs{
			UserId:       userId,
			UserAuth:     &phoneNumber,
			PasswordHash: passwordHash,
			PasswordSalt: passwordSalt,
			Verified:     true,
		}

		err = addUserAuth(args, ctx)
		assert.Equal(t, err, nil)

		networkUser = GetNetworkUser(ctx, userId)
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 2)

		/**
		 * Adding an existing user auth to a different network should fail
		 */
		userId2 := server.NewId()
		args = &AddUserAuthArgs{
			UserId:       userId2,
			UserAuth:     &phoneNumber,
			PasswordHash: passwordHash,
			PasswordSalt: passwordSalt,
			Verified:     true,
		}

		err = addUserAuth(args, ctx)
		assert.NotEqual(t, err, nil)

	})
}

// FIXME test concurrent redeem
// FIXME test expire all auth
