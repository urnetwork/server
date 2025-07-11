package model

import (
	"context"
	"fmt"
	"testing"

	// "time"

	"github.com/go-playground/assert/v2"

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

		testingUserAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

		byJwt := jwt.NewByJwt(networkId, userId, networkName, guestMode)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)

		passwordResetCreateCodeResult, err := AuthPasswordResetCreateCode(
			AuthPasswordResetCreateCodeArgs{
				UserAuth: testingUserAuth,
			},
			clientSession,
		)
		assert.Equal(t, err, nil)

		_, err = AuthPasswordSet(
			AuthPasswordSetArgs{
				ResetCode: *passwordResetCreateCodeResult.ResetCode,
				Password:  "testagain",
			},
			clientSession,
		)
		assert.Equal(t, err, nil)

	})
}

func TestAuthCode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		byJwt := jwt.NewByJwt(networkId, userId, networkName, guestMode)
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
		assert.Equal(t, len(*result.AuthAllowed), 1)
		authAllowed := (*result.AuthAllowed)[0]
		assert.Equal(t, UserAuthType(authAllowed), UserAuthTypeEmail)

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
				AuthJwt: parsedAuthJwt,
				// AuthJwtType:       SsoAuthTypeGoogle,
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
				AuthJwt: parsedAuthJwt,
				// AuthJwtType:       SsoAuthTypeApple,
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

// FIXME test concurrent redeem
// FIXME test expire all auth
