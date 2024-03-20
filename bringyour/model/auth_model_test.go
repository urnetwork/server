package model


import (
	"context"
    "testing"
    // "time"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/session"
    "bringyour.com/bringyour"
)


func TestGetUserAuth(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	networkId := bringyour.NewId()
	userId := bringyour.NewId()
	networkName := "test"

	testingUserAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

	userAuth, err := GetUserAuth(ctx, networkId)
	assert.Equal(t, err, nil)
	assert.Equal(t, userAuth, testingUserAuth)
})}


func TestResetPassword(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	networkId := bringyour.NewId()
	userId := bringyour.NewId()
	networkName := "test"

	testingUserAuth := Testing_CreateNetwork(ctx, networkId, networkName, userId)

	byJwt := jwt.NewByJwt(networkId, userId, networkName)
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
			Password: "testagain",
		},
		clientSession,
	)
	assert.Equal(t, err, nil)
	
})}


func TestAuthCode(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	networkId := bringyour.NewId()
	userId := bringyour.NewId()
	networkName := "test"

	Testing_CreateNetwork(ctx, networkId, networkName, userId)

	byJwt := jwt.NewByJwt(networkId, userId, networkName)
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

	RemoveExpiredAuthCodes(ctx, bringyour.NowUtc())
})}


// FIXME test concurrent redeem
// FIXME test expire all auth

