package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func CreateSolanaPaymentIntent(
	reference string,
	session *session.ClientSession,
) (err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {

		tag, execErr := tx.Exec(
			session.Ctx,
			`
				INSERT INTO solana_payment_intent
				(payment_reference, network_id, expires_at)
				VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING
			`,
			reference,
			session.ByJwt.NetworkId,
			server.NowUtc().Add(1*time.Hour),
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
			SELECT payment_reference, network_id
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
				))
			}
		})
	})

	return paymentIntent, nil

}

func MarkPaymentIntentCompleted(
	reference string,
	signature string,
	session *session.ClientSession,
) (err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {

		_, err = tx.Exec(
			session.Ctx,
			`
			UPDATE solana_payment_intent
			SET tx_signature = $1
			WHERE payment_reference = $2
			`,
			signature,
			reference,
		)
		server.Raise(err)

	})

	return

}

// todo - create a task to cleanup expired intents without a tx_signature
func CleanupExpiredPaymentIntents(
	ctx context.Context,
) (err error) {

	server.Tx(ctx, func(tx server.PgTx) {

		_, err = tx.Exec(
			ctx,
			`
			DELETE FROM solana_payment_intent
			WHERE expires_at < $1
			  AND tx_signature IS NULL
			`,
			server.NowUtc(),
		)
		server.Raise(err)

	})

	return

}
