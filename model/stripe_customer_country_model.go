package model

import (
	"context"

	"github.com/urnetwork/server"
)

// stripe_customer.billing_country caches the Stripe customer's billing country
// (from the card's billing details or the customer address, upper-case ISO alpha-2)
// so the plan response can resolve the price tier with one cheap read instead of a
// Stripe API call per request. The Stripe webhooks keep it current.

// SetStripeCustomerBillingCountry records the billing country for a Stripe
// customer id known to the network table. Returns true when a row was updated.
func SetStripeCustomerBillingCountry(ctx context.Context, stripeCustomerId string, countryCode string) (updated bool) {
	code := NormalizeCountryCode(countryCode)
	if stripeCustomerId == "" || code == "" {
		return false
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE stripe_customer
				SET billing_country = $2
				WHERE stripe_customer_id = $1
			`,
			stripeCustomerId,
			code,
		))
		updated = tag.RowsAffected() == 1
	}, server.TxReadCommitted)
	return
}

// GetStripeCustomerBillingCountry is the cached billing country for the network's
// Stripe customer, "" when there is no customer or no country yet.
func GetStripeCustomerBillingCountry(ctx context.Context, networkId server.Id) (countryCode string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT billing_country
				FROM stripe_customer
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var value *string
				server.Raise(result.Scan(&value))
				if value != nil {
					countryCode = *value
				}
			}
		})
	})
	return
}

// GetStripeCustomerNetworkId maps a Stripe customer id back to its network (the
// webhooks arrive with only the customer id).
func GetStripeCustomerNetworkId(ctx context.Context, stripeCustomerId string) (networkId *server.Id) {
	if stripeCustomerId == "" {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_id
				FROM stripe_customer
				WHERE stripe_customer_id = $1
			`,
			stripeCustomerId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id))
				networkId = &id
			}
		})
	})
	return
}

// GetStripeCustomerIdForNetwork is GetStripeCustomer without a session.
func GetStripeCustomerIdForNetwork(ctx context.Context, networkId server.Id) (stripeCustomerId string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT stripe_customer_id
				FROM stripe_customer
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&stripeCustomerId))
			}
		})
	})
	return
}
