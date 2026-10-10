package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// ActiveSubscriptionRenewal is one subscription_renewal row, with the store
// handles a caller needs to ask that store about it: the Play purchase token,
// the Stripe invoice id / App Store original transaction id / Solana payment
// reference in transaction_id. GetActiveSubscriptionRenewals returns the rows
// billing the network right now (start_time <= now < end_time);
// GetLastSubscriptionRenewal returns a store's last row whatever its window.
type ActiveSubscriptionRenewal struct {
	Market        SubscriptionMarket
	StartTime     time.Time
	EndTime       time.Time
	PurchaseToken string
	TransactionId string
}

// GetActiveSubscriptionRenewals returns every renewal row currently billing the
// network for subscriptionType, newest window first within each market.
//
// GetActiveSubscriptionRenewalMarkets collapses the same rows to the set of
// markets; this keeps the rows, so a caller can show WHEN each store's window
// ends and name the store object to look up or cancel. Market is nullable (it
// predates the column) and older rows also wrote the empty string, so both are
// normalized to "".
func GetActiveSubscriptionRenewals(
	ctx context.Context,
	networkId server.Id,
	subscriptionType SubscriptionType,
) []*ActiveSubscriptionRenewal {
	renewals := []*ActiveSubscriptionRenewal{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				COALESCE(market, '') AS market,
				start_time,
				end_time,
				COALESCE(purchase_token, ''),
				COALESCE(transaction_id, '')
			FROM subscription_renewal
			WHERE
				network_id = $1
				AND subscription_type = $2
				AND start_time <= $3
				AND $3 < end_time
			ORDER BY market, end_time DESC, start_time DESC
			`,
			networkId,
			subscriptionType,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				renewal := &ActiveSubscriptionRenewal{}
				server.Raise(result.Scan(
					&renewal.Market,
					&renewal.StartTime,
					&renewal.EndTime,
					&renewal.PurchaseToken,
					&renewal.TransactionId,
				))
				renewals = append(renewals, renewal)
			}
		})
	})
	return renewals
}

// The network's renewal row for subscriptionType in market that ends last,
// whether or not its window still covers now, or nil when the market never
// billed the network.
//
// A store can go on charging after the last window it was paid for: a Stripe
// renewal whose payment fails stays past_due and is retried for days or weeks
// after that window and its grace have ended. No row covers now then, so
// GetActiveSubscriptionRenewals has nothing for that store, and this row's
// handles (the Stripe invoice id in transaction_id) are what is left to ask
// the store about the subscription. The row is a handle, not evidence: only
// the store's answer says the subscription still bills.
func GetLastSubscriptionRenewal(
	ctx context.Context,
	networkId server.Id,
	subscriptionType SubscriptionType,
	market SubscriptionMarket,
) *ActiveSubscriptionRenewal {
	var renewal *ActiveSubscriptionRenewal
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				market,
				start_time,
				end_time,
				COALESCE(purchase_token, ''),
				COALESCE(transaction_id, '')
			FROM subscription_renewal
			WHERE
				network_id = $1
				AND subscription_type = $2
				AND market = $3
			ORDER BY end_time DESC, start_time DESC
			LIMIT 1
			`,
			networkId,
			subscriptionType,
			market,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				renewal = &ActiveSubscriptionRenewal{}
				server.Raise(result.Scan(
					&renewal.Market,
					&renewal.StartTime,
					&renewal.EndTime,
					&renewal.PurchaseToken,
					&renewal.TransactionId,
				))
			}
		})
	})
	return renewal
}
