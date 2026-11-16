package controller

// The Shopify order payments table on the test database
// (model/solana_shopify_order_model.go): idempotent on the signature, one
// payment per order, and on the record for the receiver leg.
//
// Needs the test database (server.DefaultTestEnv) and pro.yml.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A recognized payment is kept once: recording it again is the same answer,
// a second signature for the same order is refused, and the receiver leg's
// on-record check names it, so it is never read again.
func TestSolanaShopifyOrderPaymentsAreKeptOncePerOrder(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blockTime := time.Date(2026, 11, 13, 10, 15, 2, 0, time.UTC)
		payment := &model.SolanaShopifyOrderPayment{
			TxSignature:     "synthetic-shopify-paid",
			OrderId:         "gid://shopify/Order/synthetic-1",
			OrderName:       "#1",
			PaymentId:       "synthetic-payment-1",
			ReceivedUsd:     142.5303,
			OrderTotalUsd:   143.97,
			TransactionTime: &blockTime,
			Leg:             model.SolanaShopifyOrderLegReceiver,
		}
		connect.AssertEqual(t, model.IsSolanaShopifyOrderPaymentRecordedInConn(nil, ctx, payment.TxSignature), false)
		connect.AssertEqual(t, model.RecordSolanaShopifyOrderPaymentInConn(nil, ctx, payment), true)
		connect.AssertEqual(t, model.RecordSolanaShopifyOrderPaymentInConn(nil, ctx, payment), true)
		connect.AssertEqual(t, model.IsSolanaShopifyOrderPaymentRecordedInConn(nil, ctx, payment.TxSignature), true)

		stored := model.GetSolanaShopifyOrderPaymentInConn(nil, ctx, payment.TxSignature)
		connect.AssertEqual(t, stored.OrderId, payment.OrderId)
		connect.AssertEqual(t, stored.OrderName, "#1")
		connect.AssertEqual(t, stored.PaymentId, "synthetic-payment-1")
		connect.AssertEqual(t, stored.ReceivedUsd, 142.5303)
		connect.AssertEqual(t, stored.OrderTotalUsd, 143.97)
		connect.AssertEqual(t, stored.TransactionTime.Equal(blockTime), true)
		connect.AssertEqual(t, stored.Leg, model.SolanaShopifyOrderLegReceiver)

		// a second payment of the same order is not the order's
		second := *payment
		second.TxSignature = "synthetic-shopify-paid-again"
		connect.AssertEqual(t, model.RecordSolanaShopifyOrderPaymentInConn(nil, ctx, &second), false)
		connect.AssertEqual(t, model.IsSolanaShopifyOrderPaymentRecordedInConn(nil, ctx, second.TxSignature), false)
		// and the first signature never holds another order
		other := *payment
		other.OrderId = "gid://shopify/Order/synthetic-2"
		connect.AssertEqual(t, model.RecordSolanaShopifyOrderPaymentInConn(nil, ctx, &other), false)

		onRecord := model.ListSolanaSignaturesOnRecordInConn(nil, ctx, []string{payment.TxSignature, second.TxSignature})
		connect.AssertEqual(t, onRecord, map[string]bool{payment.TxSignature: true})
	})
}
