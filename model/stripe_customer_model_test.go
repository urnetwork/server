package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestStripeCustomer(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		stripeCustomerId := "cus_abc123xyz"

		err := CreateStripeCustomer(stripeCustomerId, userSession)
		assert.Equal(t, nil, err)

		cust, err := GetStripeCustomer(userSession)
		assert.Equal(t, nil, err)
		assert.Equal(t, stripeCustomerId, cust)

		/**
		 * Test not found
		 */
		networkId = server.NewId()
		clientId = server.NewId()

		userSession = session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		cust, err = GetStripeCustomer(userSession)
		assert.Equal(t, cust, nil)
	})
}
