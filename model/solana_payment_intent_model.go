package model

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The Solana subscription plans. These are recorded on the intent so the webhook can
// grant what was actually bought, instead of assuming.
const (
	SolanaPlanMonthly = "monthly"
	SolanaPlanYearly  = "yearly"
)

// CreateSolanaPaymentIntent records what the customer was QUOTED: the price shown to
// them, and the plan they picked.
//
// Without this the webhook had nothing to check an arriving payment against, so it
// hardcoded `>= 40 USDC` and always granted a year. The $5 monthly option on the site
// therefore took the money and delivered nothing (5 < 40, ignored as "no matching USDC
// payment"), while any payment of 40+ bought a year regardless of size.
func CreateSolanaPaymentIntent(
	reference string,
	expectedAmountUsd float64,
	subscriptionPlan string,
	session *session.ClientSession,
) (err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {

		tag, execErr := tx.Exec(
			session.Ctx,
			`
				INSERT INTO solana_payment_intent
				(payment_reference, network_id, expires_at, expected_amount_usd, subscription_plan)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT DO NOTHING
			`,
			reference,
			session.ByJwt.NetworkId,
			server.NowUtc().Add(1*time.Hour),
			expectedAmountUsd,
			subscriptionPlan,
		)
		if execErr != nil {
			err = execErr
			return
		}
		if tag.RowsAffected() == 0 {
			err = errors.New("payment_reference already exists")
			return
		}

	})

	return

}

/**
 * The Helius webhook returns an array of accounts
 * There is no indication which is the reference id, so we have to search them all
 */

type PaymentIntentSearchResult struct {
	NetworkId        *server.Id `json:"network_id"`
	PaymentReference string     `json:"payment_reference"`
	// what the customer was quoted, and what they picked. 0 / "" for intents created
	// before these were recorded: the webhook then falls back to the old behavior.
	ExpectedAmountUsd float64 `json:"expected_amount_usd"`
	SubscriptionPlan  string  `json:"subscription_plan"`
}

func SearchPaymentIntents(
	references []string,
	session *session.ClientSession,
) (*PaymentIntentSearchResult, error) {

	var paymentIntent *PaymentIntentSearchResult

	server.Tx(session.Ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			session.Ctx,
			`
			SELECT payment_reference, network_id, expected_amount_usd, subscription_plan
		    FROM solana_payment_intent
		    WHERE tx_signature IS NULL
		      AND payment_reference = ANY($1)
		    LIMIT 1
			`,
			references,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				paymentIntent = &PaymentIntentSearchResult{}
				server.Raise(result.Scan(
					&paymentIntent.PaymentReference,
					&paymentIntent.NetworkId,
					&paymentIntent.ExpectedAmountUsd,
					&paymentIntent.SubscriptionPlan,
				))
			}
		})
	})

	return paymentIntent, nil

}

// MarkPaymentIntentCompletedInTx consumes the intent. completed = false means the
// intent was already consumed (or never existed) -- the caller must NOT credit.
//
// The `tx_signature IS NULL` predicate is the concurrency gate: two deliveries of
// the same transaction both set the SAME signature, so the unique index on
// tx_signature never fires. Under READ COMMITTED the second UPDATE blocks on the
// row lock, re-evaluates the predicate after the first commits, and affects zero
// rows -- exactly one delivery wins the credit.
func MarkPaymentIntentCompletedInTx(
	tx server.PgTx,
	reference string,
	signature string,
	session *session.ClientSession,
) (completed bool, err error) {
	tag, err := tx.Exec(
		session.Ctx,
		`
		UPDATE solana_payment_intent
		SET tx_signature = $1
		WHERE payment_reference = $2
		  AND tx_signature IS NULL
		`,
		signature,
		reference,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() != 0, nil
}

func MarkPaymentIntentCompleted(
	reference string,
	signature string,
	session *session.ClientSession,
) (completed bool, err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {
		completed, err = MarkPaymentIntentCompletedInTx(tx, reference, signature, session)
	})

	return

}

// IsSolanaPaymentCompleted reports whether a transaction signature has already
// consumed an intent -- i.e. this exact on-chain payment has already been credited.
// The webhook uses it to tell a REDELIVERY of a credited payment (nothing to do)
// apart from a payment that never matched an intent (record it for an operator).
func IsSolanaPaymentCompleted(
	ctx context.Context,
	signature string,
) (completed bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT 1 FROM solana_payment_intent
			WHERE tx_signature = $1
			`,
			signature,
		)
		server.WithPgResult(result, err, func() {
			completed = result.Next()
		})
	})
	return
}

// The reasons a received payment could not be fulfilled -- see
// RecordUnfulfilledSolanaPayment.
const (
	SolanaUnfulfilledReasonNoIntent  = "no_intent"
	SolanaUnfulfilledReasonUnderpaid = "underpaid"
)

// UnfulfilledSolanaPayment records a USDC payment that arrived at one of our
// receiving addresses but bought nothing: either no open intent matched any of
// the transaction's account keys (a late payment whose intent was already swept,
// or an unknown reference), or the amount was under the quote. The money moved
// on-chain and Helius is acked 200 either way (it never re-examines a delivered
// tx), so this row is the operator-visible record with enough detail to repair
// by hand.
type UnfulfilledSolanaPayment struct {
	TxSignature    string
	Reason         string
	TokenAmountUsd float64
	// what the payment was checked against, when an intent matched (underpaid)
	ExpectedAmountUsd *float64
	PaymentReference  *string
	NetworkId         *server.Id
	// the account keys the reference was searched among, when none matched
	ReferenceCandidates []string
	// the on-chain timestamp, when the webhook carried one
	TransactionTime *time.Time
}

// RecordUnfulfilledSolanaPayment writes the row. Idempotent on the tx signature
// (ON CONFLICT DO NOTHING), so a Helius redelivery does not duplicate it. Never
// raises: recording must not turn an acked payment into a retry loop.
func RecordUnfulfilledSolanaPayment(
	ctx context.Context,
	payment *UnfulfilledSolanaPayment,
) (err error) {

	var referenceCandidates *string
	if 0 < len(payment.ReferenceCandidates) {
		candidatesJson, jsonErr := json.Marshal(payment.ReferenceCandidates)
		if jsonErr == nil {
			candidatesJsonStr := string(candidatesJson)
			referenceCandidates = &candidatesJsonStr
		}
	}

	server.Tx(ctx, func(tx server.PgTx) {
		_, err = tx.Exec(
			ctx,
			`
			INSERT INTO solana_unfulfilled_payment
			(tx_signature, reason, token_amount_usd, expected_amount_usd,
			 payment_reference, network_id, reference_candidates, transaction_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT DO NOTHING
			`,
			payment.TxSignature,
			payment.Reason,
			payment.TokenAmountUsd,
			payment.ExpectedAmountUsd,
			payment.PaymentReference,
			payment.NetworkId,
			referenceCandidates,
			payment.TransactionTime,
		)
	})

	return
}

// ListUnfulfilledSolanaPayments returns recorded payments with the given
// reason, oldest first, with the reference candidates decoded -- the shape the
// reconciler sweeps: a no_intent payment whose reference NOW resolves to an
// open intent can be credited after all.
func ListUnfulfilledSolanaPayments(
	ctx context.Context,
	reason string,
	limit int,
) []*UnfulfilledSolanaPayment {
	payments := []*UnfulfilledSolanaPayment{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT tx_signature, token_amount_usd, expected_amount_usd,
			       payment_reference, network_id, reference_candidates
			FROM solana_unfulfilled_payment
			WHERE reason = $1
			ORDER BY record_time ASC
			LIMIT $2
			`,
			reason,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				payment := &UnfulfilledSolanaPayment{
					Reason: reason,
				}
				var referenceCandidates *string
				server.Raise(result.Scan(
					&payment.TxSignature,
					&payment.TokenAmountUsd,
					&payment.ExpectedAmountUsd,
					&payment.PaymentReference,
					&payment.NetworkId,
					&referenceCandidates,
				))
				if referenceCandidates != nil {
					json.Unmarshal([]byte(*referenceCandidates), &payment.ReferenceCandidates)
				}
				payments = append(payments, payment)
			}
		})
	})
	return payments
}

// RemoveUnfulfilledSolanaPayment clears a recorded payment once it is no
// longer unfulfilled (the reconciler credited it, or found it already
// credited by a redelivery).
func RemoveUnfulfilledSolanaPayment(
	ctx context.Context,
	txSignature string,
) (err error) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err = tx.Exec(
			ctx,
			`
			DELETE FROM solana_unfulfilled_payment
			WHERE tx_signature = $1
			`,
			txSignature,
		)
	})
	return
}

// GetSolanaPaymentIntentSignature returns the tx signature a completed intent
// was consumed by. ok = false means the intent does not exist or is still
// open. The reconciler uses this to re-verify credited payments on-chain.
func GetSolanaPaymentIntentSignature(
	ctx context.Context,
	reference string,
) (signature string, ok bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT tx_signature FROM solana_payment_intent
			WHERE payment_reference = $1
			  AND tx_signature IS NOT NULL
			`,
			reference,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&signature))
				ok = true
			}
		})
	})
	return
}

// GetUnfulfilledSolanaPayment reads one recorded payment back by signature.
func GetUnfulfilledSolanaPayment(
	ctx context.Context,
	txSignature string,
) (payment *UnfulfilledSolanaPayment, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT reason, token_amount_usd, expected_amount_usd,
			       payment_reference, network_id
			FROM solana_unfulfilled_payment
			WHERE tx_signature = $1
			`,
			txSignature,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				payment = &UnfulfilledSolanaPayment{
					TxSignature: txSignature,
				}
				server.Raise(result.Scan(
					&payment.Reason,
					&payment.TokenAmountUsd,
					&payment.ExpectedAmountUsd,
					&payment.PaymentReference,
					&payment.NetworkId,
				))
			} else {
				returnErr = errors.New("Unfulfilled payment not found.")
			}
		})
	})
	return
}

// todo - create a task to cleanup expired intents without a tx_signature
func CleanupExpiredPaymentIntents(
	ctx context.Context,
	minTime time.Time,
) (err error) {

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		_, err = tx.Exec(
			ctx,
			`
			DELETE FROM solana_payment_intent
			WHERE expires_at < $1
			  AND tx_signature IS NULL
			`,
			minTime,
		)
		server.Raise(err)

	})

	return

}
