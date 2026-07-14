package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestStripeCustomer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		stripeCustomerId := "cus_abc123xyz"

		err := CreateStripeCustomer(stripeCustomerId, userSession)
		connect.AssertEqual(t, nil, err)

		cust, err := GetStripeCustomer(userSession)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, stripeCustomerId, cust)

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
		connect.AssertEqual(t, cust, nil)
	})
}
