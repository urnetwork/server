package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/session"
)

func TestSolanaPaymentIntents(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		reference := "test-reference-1"

		err := CreateSolanaPaymentIntent(reference, userSession)
		assert.Equal(t, err, nil)

		// adding the same reference twice should fail
		err = CreateSolanaPaymentIntent(reference, userSession)
		assert.NotEqual(t, err, nil)

		references := []string{"AAA", "BBB", "CCC", "DDD"}

		// test not found
		paymentSearchResult, err := SearchPaymentIntents(references, userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, paymentSearchResult, nil)

		// test found
		references = append(references, reference)
		paymentSearchResult, err = SearchPaymentIntents(references, userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, *paymentSearchResult.NetworkId, networkId)
		assert.Equal(t, *&paymentSearchResult.PaymentReference, reference)

		// mark completed
		err = MarkPaymentIntentCompleted(reference, "tx-signature-1", userSession)
		assert.Equal(t, err, nil)

	})
}
