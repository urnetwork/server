package model

// The reads behind the Solana reconcile's chain leg
// (controller/crypto_pay_solana_native.go, reconcileSolanaChainReferences):
// the USDC Solana Pay intents whose payment may have arrived without its
// Helius delivery.

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// ListOpenSolanaPaymentReferencesInConn lists the references of the USDC
// Solana Pay intents not yet paid (no tx_signature), made before
// createdBefore (a payment in flight is left to its webhook) and expiring
// after expiresAfter (a late payment is still credited: the webhook's search
// ignores the expiry), newest first, at most limit. Both the old endpoints'
// intents and the ones linked from crypto invoices are listed. Reuse the
// caller's PostgreSQL session; nil selects the outer acquisition boundary.
func ListOpenSolanaPaymentReferencesInConn(
	connOwner server.PgConn,
	ctx context.Context,
	createdBefore time.Time,
	expiresAfter time.Time,
	limit int,
) (references []string) {
	references = []string{}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT payment_reference
				FROM solana_payment_intent
				WHERE tx_signature IS NULL
				  AND $2 < expires_at
				  AND created_at < $1
				ORDER BY created_at DESC
				LIMIT $3
			`,
			createdBefore,
			expiresAfter,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var reference string
				server.Raise(result.Scan(&reference))
				references = append(references, reference)
			}
		})
	})
	return
}

// IsUnfulfilledSolanaPaymentRecordedInConn reports whether a transaction is
// already recorded as an unfulfilled payment (the reconcile's first leg
// sweeps those). Reuse the caller's PostgreSQL session; nil selects the outer
// acquisition boundary.
func IsUnfulfilledSolanaPaymentRecordedInConn(connOwner server.PgConn, ctx context.Context, signature string) (recorded bool) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT 1 FROM solana_unfulfilled_payment WHERE tx_signature = $1`,
			signature,
		)
		server.WithPgResult(result, err, func() {
			recorded = result.Next()
		})
	})
	return
}
