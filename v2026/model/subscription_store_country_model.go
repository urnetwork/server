package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// subscription_renewal.country is the country a store sold a subscription row
// in: the App Store storefront (ISO alpha-3 in the signed transaction) or the
// Google Play region code, kept as upper-case ISO 3166-1 alpha-2. The store
// purchase paths record it beside the row's price tier, from the same value
// (controller/onboarding_store_controller.go). Rows credited before the column
// existed are NULL: the stores' signed payloads were not kept, so only their
// APIs know those countries. The re-engagement program reads it as a network's
// country when the network has no onboarding row (mmm/reengagement/PLAN.md
// "Server" 10).

// SetSubscriptionRenewalCountryByTransactionInTx records the store country on
// the renewal row of a store transaction. Returns the rows stamped.
func SetSubscriptionRenewalCountryByTransactionInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	market SubscriptionMarket,
	transactionId string,
	country string,
) int64 {
	country = CountryCodeFromStorefront(country)
	if transactionId == "" || country == "" {
		return 0
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE subscription_renewal
			SET country = $4
			WHERE network_id = $1 AND market = $2 AND transaction_id = $3
		`,
		networkId,
		market,
		transactionId,
		country,
	))
	return tag.RowsAffected()
}

// SetSubscriptionRenewalCountryByPurchaseTokenInTx records the store country on
// the renewal row of a Play purchase token ending at endTime.
func SetSubscriptionRenewalCountryByPurchaseTokenInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	market SubscriptionMarket,
	purchaseToken string,
	endTime time.Time,
	country string,
) int64 {
	country = CountryCodeFromStorefront(country)
	if purchaseToken == "" || country == "" {
		return 0
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE subscription_renewal
			SET country = $5
			WHERE network_id = $1 AND market = $2 AND purchase_token = $3 AND end_time = $4
		`,
		networkId,
		market,
		purchaseToken,
		endTime,
		country,
	))
	return tag.RowsAffected()
}

// NetworkStoreCountries are the store countries of the network's latest App
// Store row and latest Google Play row that carry one (upper-case alpha-2), ""
// for a store without one.
func NetworkStoreCountries(ctx context.Context, networkId server.Id) (appStore string, play string) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT ON (market) market, country
				FROM subscription_renewal
				WHERE network_id = $1 AND market = ANY($2::text[]) AND country IS NOT NULL
				ORDER BY market, start_time DESC, end_time DESC
			`,
			networkId,
			[]string{SubscriptionMarketApple, SubscriptionMarketGoogle},
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var market string
				var country string
				server.Raise(result.Scan(&market, &country))
				switch market {
				case SubscriptionMarketApple:
					appStore = CountryCodeFromStorefront(country)
				case SubscriptionMarketGoogle:
					play = CountryCodeFromStorefront(country)
				}
			}
		})
	})
	return
}
