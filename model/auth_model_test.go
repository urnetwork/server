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

func TestAuthLogin(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		userAuth := fmt.Sprintf("%s@bringyour.com", networkId)
		// password := "password"

		result, err := loginUserAuth(&userAuth, ctx)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result, nil)
		assert.Equal(t, result.UserAuth, userAuth)
		assert.NotEqual(t, result.AuthAllowed, nil)
		assert.Equal(t, len(*result.AuthAllowed), 1)
		authAllowed := (*result.AuthAllowed)[0]
		assert.Equal(t, UserAuthType(authAllowed), UserAuthTypeEmail)

	})
}

// FIXME test concurrent redeem
// FIXME test expire all auth
