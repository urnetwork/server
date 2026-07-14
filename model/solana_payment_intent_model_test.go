package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestSolanaPaymentIntents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		reference := "test-reference-1"

		err := CreateSolanaPaymentIntent(reference, 10.00, "supporter", userSession)
		connect.AssertEqual(t, err, nil)

		// adding the same reference twice should fail
		err = CreateSolanaPaymentIntent(reference, 10.00, "supporter", userSession)
		connect.AssertNotEqual(t, err, nil)

		references := []string{"AAA", "BBB", "CCC", "DDD"}

		// test not found
		paymentSearchResult, err := SearchPaymentIntents(references, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, paymentSearchResult, nil)

		// test found
		references = append(references, reference)
		paymentSearchResult, err = SearchPaymentIntents(references, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *paymentSearchResult.NetworkId, networkId)
		connect.AssertEqual(t, *&paymentSearchResult.PaymentReference, reference)

		// mark completed
		err = MarkPaymentIntentCompleted(reference, "tx-signature-1", userSession)
		connect.AssertEqual(t, err, nil)

	})
}
