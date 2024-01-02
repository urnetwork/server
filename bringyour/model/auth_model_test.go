package model


import (
	"context"
    "testing"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/session"
    "bringyour.com/bringyour"
)


func TestAuthCode(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	networkId := bringyour.NewId()
	userId := bringyour.NewId()
	networkName := "test"

	testCreateNetwork(ctx, networkId, networkName, userId)

	byJwt := jwt.NewByJwt(networkId, userId, networkName)
	clientSession := session.NewLocalClientSession(ctx, byJwt)

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
})}


// FIXME test concurrent redeem
// FIXME test expire all auth

