package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
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

func MarkPaymentIntentCompletedInTx(
	tx server.PgTx,
	reference string,
	signature string,
	session *session.ClientSession,
) error {
	_, err := tx.Exec(
		session.Ctx,
		`
		UPDATE solana_payment_intent
		SET tx_signature = $1
		WHERE payment_reference = $2
		`,
		signature,
		reference,
	)
	return err
}

func MarkPaymentIntentCompleted(
	reference string,
	signature string,
	session *session.ClientSession,
) (err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {
		err = MarkPaymentIntentCompletedInTx(tx, reference, signature, session)
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
