package model

// The reads and writes behind the Solana reconcile's receiver leg
// (controller/solana_receiver_sweep.go): the cursor of each USDC token account
// of our Solana Pay receivers, and which transaction signatures a record
// already names.

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// The newest transaction of one receiver token account that the receiver leg
// has examined. Every older transaction of the account was examined before
// it.
type SolanaReceiverSweepCursor struct {
	TokenAccount string
	// the receiver (the token account's owner) the account was listed under
	Receiver  string
	Signature string
	Slot      uint64
	// the transaction's block time, when the chain gave one
	BlockTime *time.Time
}

// One token account's cursor; nil when the account was never swept. Reuse the
// caller's PostgreSQL session; nil selects the outer acquisition boundary.
func GetSolanaReceiverSweepCursorInConn(
	connOwner server.PgConn,
	ctx context.Context,
	tokenAccount string,
) (cursor *SolanaReceiverSweepCursor) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT receiver, signature, slot, block_time
				FROM solana_receiver_sweep_cursor
				WHERE token_account = $1
			`,
			tokenAccount,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				cursor = &SolanaReceiverSweepCursor{TokenAccount: tokenAccount}
				var slot int64
				server.Raise(result.Scan(&cursor.Receiver, &cursor.Signature, &slot, &cursor.BlockTime))
				cursor.Slot = uint64(max(0, slot))
			}
		})
	})
	return
}

// Moves one token account's cursor to the newest transaction examined. Reuse
// the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func SetSolanaReceiverSweepCursorInConn(
	connOwner server.PgConn,
	ctx context.Context,
	cursor *SolanaReceiverSweepCursor,
) {
	// a failed statement raises, which ends the transaction at once
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO solana_receiver_sweep_cursor
				(token_account, receiver, signature, slot, block_time, update_time)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (token_account) DO UPDATE
				SET receiver = $2, signature = $3, slot = $4, block_time = $5, update_time = $6
			`,
			cursor.TokenAccount,
			cursor.Receiver,
			cursor.Signature,
			int64(cursor.Slot),
			cursor.BlockTime,
			server.NowUtc(),
		))
	})
}

// Which of the given transaction signatures a record already names: a Solana
// Pay intent it paid (the webhook's, the reconcile's and a USDC invoice's
// credit all set it, and the retention unlink keeps it), a Solana or x402
// renewal keyed by it, a balance bought with it (a data pack, an x402 data
// grant), a crypto rail receipt of it, the unfulfilled record of a payment
// that bought nothing, or a Shopify order it paid. One read for the batch.
// Reuse the caller's PostgreSQL session; nil selects the outer acquisition
// boundary.
func ListSolanaSignaturesOnRecordInConn(
	connOwner server.PgConn,
	ctx context.Context,
	signatures []string,
) (onRecordSignatures map[string]bool) {
	onRecordSignatures = map[string]bool{}
	if len(signatures) == 0 {
		return
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT candidate.signature
				FROM unnest($1::text[]) AS candidate(signature)
				WHERE EXISTS (
					SELECT 1 FROM solana_payment_intent intent
					WHERE intent.tx_signature = candidate.signature
				) OR EXISTS (
					SELECT 1 FROM solana_unfulfilled_payment unfulfilled
					WHERE unfulfilled.tx_signature = candidate.signature
				) OR EXISTS (
					SELECT 1 FROM subscription_renewal renewal
					WHERE renewal.market = ANY($2) AND renewal.transaction_id = candidate.signature
				) OR EXISTS (
					SELECT 1 FROM transfer_balance balance
					WHERE balance.purchase_token = candidate.signature
				) OR EXISTS (
					SELECT 1 FROM crypto_payment_receipt receipt
					WHERE receipt.asset = ANY($3) AND receipt.tx_id = candidate.signature
				) OR EXISTS (
					SELECT 1 FROM solana_shopify_order_payment shopify
					WHERE shopify.tx_signature = candidate.signature
				)
			`,
			signatures,
			[]string{SubscriptionMarketSolana, SubscriptionMarketX402},
			CryptoAssets,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var signature string
				server.Raise(result.Scan(&signature))
				onRecordSignatures[signature] = true
			}
		})
	})
	return
}
