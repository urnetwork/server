package model

import (
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

/**
 * We use this when creating Stripe payment intents
 */
func CreateStripeCustomer(
	stripeCustomerId string,
	session *session.ClientSession,
) (err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {

		// a failed insert raises, which ends the transaction at once. It used
		// to set the error result while the transaction went on to a commit
		// that server.Tx retried for a minute.
		tag := server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				INSERT INTO stripe_customer
				(network_id, stripe_customer_id)
				VALUES ($1, $2)
				ON CONFLICT DO NOTHING
			`,
			session.ByJwt.NetworkId,
			stripeCustomerId,
		))
		if tag.RowsAffected() == 0 {
			err = errors.New("stripe_customer already exists")
			return
		}

	})

	return

}

func GetStripeCustomer(
	session *session.ClientSession,
) (stripeCustomerId *string, err error) {

	server.Tx(session.Ctx, func(tx server.PgTx) {

		result, queryErr := tx.Query(
			session.Ctx,
			`
			SELECT stripe_customer_id
		    FROM stripe_customer
		    WHERE network_id = $1
			`,
			session.ByJwt.NetworkId,
		)

		// a failed query raises, which ends the transaction at once. It used to
		// set the error result while the transaction went on to a commit that
		// server.Tx retried for a minute.
		server.WithPgResult(result, queryErr, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&stripeCustomerId,
				))
			}
		})
	})

	return

}
