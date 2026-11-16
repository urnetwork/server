package model

// The Shopify orders paid with Solana Pay into our USDC receivers
// (controller/solana_shopify_order.go). Our Shopify store's URnode orders are
// paid through Shopify's Solana Pay app into the same receiver the apps'
// purchases pay. A payment recognized as one by its Shopify order record is
// kept here, instead of as an unfulfilled payment: it paid for a Shopify
// order, not an app purchase, so it is never credited and never pages.

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The legs that recognize a Shopify order's payment.
const (
	SolanaShopifyOrderLegWebhook  = "webhook"
	SolanaShopifyOrderLegReceiver = "receiver"
)

// One Shopify order's Solana Pay payment.
type SolanaShopifyOrderPayment struct {
	TxSignature string
	// the Shopify order's id (gid://shopify/Order/...) and name ("#1109")
	OrderId   string
	OrderName string
	// the order's Solana Pay sale's payment id
	PaymentId string
	// what reached our receiver, and the order's total: the whole of what
	// the payment's source paid out (our share and the Solana Pay app's fee)
	ReceivedUsd   float64
	OrderTotalUsd float64
	// the on-chain time, when the chain gave one
	TransactionTime *time.Time
	// the leg that recognized it (SolanaShopifyOrderLegWebhook, ...Receiver)
	Leg string
}

// Keeps a recognized payment, once: idempotent on the signature, and one
// payment per order. recorded = false when another signature already holds the
// order (a second payment of one order is no order payment of its own: the
// caller keeps it unfulfilled). Reuse the caller's PostgreSQL session; nil
// selects the outer acquisition boundary.
func RecordSolanaShopifyOrderPaymentInConn(
	connOwner server.PgConn,
	ctx context.Context,
	payment *SolanaShopifyOrderPayment,
) (recorded bool) {
	// a failed statement raises, which ends the transaction at once
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO solana_shopify_order_payment
				(tx_signature, order_id, order_name, payment_id, received_usd, order_total_usd, transaction_time, leg, record_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT DO NOTHING
			`,
			payment.TxSignature,
			payment.OrderId,
			payment.OrderName,
			payment.PaymentId,
			payment.ReceivedUsd,
			payment.OrderTotalUsd,
			payment.TransactionTime,
			payment.Leg,
			server.NowUtc(),
		))
		result, err := tx.Query(
			ctx,
			`SELECT order_id FROM solana_shopify_order_payment WHERE tx_signature = $1`,
			payment.TxSignature,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var orderId string
				server.Raise(result.Scan(&orderId))
				recorded = orderId == payment.OrderId
			}
		})
	})
	return
}

// Whether a signature was recognized as a Shopify order's payment. Reuse the
// caller's PostgreSQL session; nil selects the outer acquisition boundary.
func IsSolanaShopifyOrderPaymentRecordedInConn(
	connOwner server.PgConn,
	ctx context.Context,
	signature string,
) (recorded bool) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT 1 FROM solana_shopify_order_payment WHERE tx_signature = $1`,
			signature,
		)
		server.WithPgResult(result, err, func() {
			recorded = result.Next()
		})
	})
	return
}

// One recognized payment by its signature; nil when there is none. Reuse the
// caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetSolanaShopifyOrderPaymentInConn(
	connOwner server.PgConn,
	ctx context.Context,
	signature string,
) (payment *SolanaShopifyOrderPayment) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT order_id, order_name, payment_id, received_usd, order_total_usd, transaction_time, leg
				FROM solana_shopify_order_payment
				WHERE tx_signature = $1
			`,
			signature,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				payment = &SolanaShopifyOrderPayment{TxSignature: signature}
				server.Raise(result.Scan(
					&payment.OrderId,
					&payment.OrderName,
					&payment.PaymentId,
					&payment.ReceivedUsd,
					&payment.OrderTotalUsd,
					&payment.TransactionTime,
					&payment.Leg,
				))
			}
		})
	})
	return
}
