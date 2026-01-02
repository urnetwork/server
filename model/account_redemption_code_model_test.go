package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestAccountRedemptionCode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()

		sessionA := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		redemptionCode, err := CreateAccountRedemptionCode(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(redemptionCode), 8)

		claimed, err := ClaimAccountRedemptionCode(redemptionCode, sessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, claimed, true)

		// Try and claim again, should fail
		claimed, err = ClaimAccountRedemptionCode(redemptionCode, sessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, claimed, false)

		// different account trying to claim, should fail
		networkIdB := server.NewId()
		clientIdB := server.NewId()

		sessionB := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkIdB,
			ClientId:  &clientIdB,
		})

		claimed, err = ClaimAccountRedemptionCode(redemptionCode, sessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, claimed, false)

	})
}
