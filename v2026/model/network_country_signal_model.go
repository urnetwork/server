package model

// The countries the server holds about a network, which a purchase's price
// tier is decided from with the request's own address
// (controller/price_tier_signals.go). The plan response every app polls reads
// them too, so they are one statement.

import (
	"context"
	"slices"

	"github.com/urnetwork/server/v2026"
)

// NetworkCountrySignals are a network's countries by where the server got
// them, each upper-case alpha-2 as stored; "" or empty for none.
type NetworkCountrySignals struct {
	// the country of the address the account signed up from
	// (network_onboarding.country)
	Signup string
	// the Stripe customer's billing country (stripe_customer.billing_country,
	// as GetStripeCustomerBillingCountry)
	Billing string
	// the countries of the network's latest App Store row and latest Google
	// Play row that carry one (as NetworkStoreCountries)
	Stores []string
	// the countries the network's newest active devices last connected from,
	// distinct and sorted. A device's last placement stays in
	// network_client_location_reliability after it disconnects. A device is
	// a top-level client: a client a device provisions (source_client_id,
	// such as a hosted proxy) connects from our servers rather than from the
	// device, and is not read
	Connections []string
}

// GetNetworkCountrySignals reads the network's countries in one statement, of
// indexed reads: at most maxDeviceCount devices are read, newest first;
// non-positive reads none.
func GetNetworkCountrySignals(ctx context.Context, networkId server.Id, maxDeviceCount int) *NetworkCountrySignals {
	signals := &NetworkCountrySignals{
		Stores:      []string{},
		Connections: []string{},
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					(SELECT country FROM network_onboarding WHERE network_id = $1),
					(SELECT billing_country FROM stripe_customer WHERE network_id = $1),
					ARRAY(
						SELECT DISTINCT ON (market) country::text
						FROM subscription_renewal
						WHERE network_id = $1 AND market = ANY($2::text[]) AND country IS NOT NULL
						ORDER BY market, start_time DESC, end_time DESC
					),
					ARRAY(
						SELECT DISTINCT location.country_code::text
						FROM (
							SELECT client_id
							FROM network_client
							WHERE
								network_id = $1 AND
								active = true AND
								source_client_id IS NULL
							ORDER BY client_id DESC
							LIMIT $3
						) device
						INNER JOIN network_client_location_reliability ON
							network_client_location_reliability.client_id = device.client_id
						INNER JOIN location ON
							location.location_id = network_client_location_reliability.country_location_id
					)
			`,
			networkId,
			[]string{SubscriptionMarketApple, SubscriptionMarketGoogle},
			max(0, maxDeviceCount),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var signup *string
				var billing *string
				var stores []string
				var connections []string
				server.Raise(result.Scan(&signup, &billing, &stores, &connections))
				if signup != nil {
					signals.Signup = NormalizeCountryCode(*signup)
				}
				if billing != nil {
					signals.Billing = NormalizeCountryCode(*billing)
				}
				for _, country := range stores {
					if code := CountryCodeFromStorefront(country); code != "" {
						signals.Stores = append(signals.Stores, code)
					}
				}
				for _, country := range connections {
					if code := NormalizeCountryCode(country); code != "" && !slices.Contains(signals.Connections, code) {
						signals.Connections = append(signals.Connections, code)
					}
				}
			}
		})
	})
	slices.Sort(signals.Connections)
	return signals
}
