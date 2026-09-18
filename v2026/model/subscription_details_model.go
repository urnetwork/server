package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// ActiveSubscriptionRenewal is one subscription_renewal row that is billing the
// network right now (start_time <= now < end_time), with the store handles a
// caller needs to ask that store about it: the Play purchase token, the Stripe
// invoice id / App Store original transaction id / Solana payment reference in
// transaction_id.
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
