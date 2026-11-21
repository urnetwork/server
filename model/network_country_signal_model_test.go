package model

// GetNetworkCountrySignals against the database: a network's sign-up,
// billing and store countries, and the countries its newest active devices
// last connected from, each once, in one read. Needs the test database
// (server.DefaultTestEnv).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestGetNetworkCountrySignals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "countrysignals", server.NewId())
		otherNetworkId := server.NewId()
		exec := func(sql string, args ...any) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, sql, args...))
			})
		}

		connect.AssertEqual(t, GetNetworkCountrySignals(ctx, networkId, 32), &NetworkCountrySignals{
			Stores:      []string{},
			Connections: []string{},
		})

		connect.AssertEqual(t, CreateNetworkOnboarding(ctx, &NetworkOnboarding{
			NetworkId: networkId,
			CreatedAt: server.NowUtc(),
			Country:   "IN",
		}), true)
		connect.AssertEqual(t, CreateStripeCustomerForNetwork(ctx, networkId, "cus_countrysignals"), nil)
		connect.AssertEqual(t, SetStripeCustomerBillingCountry(ctx, "cus_countrysignals", "br"), true)

		// the latest store row of each store that carries a country
		start := server.NowUtc().Add(-90 * 24 * time.Hour).Truncate(time.Second)
		storeRow := func(market SubscriptionMarket, transactionId string, startTime time.Time, country string) {
			connect.AssertEqual(t, AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				StartTime:          startTime,
				EndTime:            startTime.Add(30 * 24 * time.Hour),
				SubscriptionMarket: market,
				TransactionId:      transactionId,
			}), nil)
			server.Tx(ctx, func(tx server.PgTx) {
				SetSubscriptionRenewalCountryByTransactionInTx(tx, ctx, networkId, market, transactionId, country)
			})
		}
		storeRow(SubscriptionMarketApple, "apple-1", start, "DEU")
		storeRow(SubscriptionMarketGoogle, "play-1", start, "MX")
		storeRow(SubscriptionMarketGoogle, "play-2", start.Add(30*24*time.Hour), "PH")

		// one location row per country: the location's full name is unique.
		// Ids are minted in order, so the last device made is the newest
		countryIds := map[string]server.Id{}
		device := func(networkId server.Id, countryCode string) server.Id {
			countryId, ok := countryIds[countryCode]
			if !ok {
				countryId = server.NewId()
				countryIds[countryCode] = countryId
			}
			clientId := server.NewId()
			Testing_CreateProviderAtLocation(ctx, networkId, clientId, countryId, countryCode)
			return clientId
		}
		device(networkId, "US")
		device(networkId, "IN")
		device(networkId, "IN")
		device(otherNetworkId, "GB")
		// a client a device provisions connects from our servers, and an
		// inactive client is no device of the network any more
		provisioned := device(networkId, "DE")
		parent := device(networkId, "BR")
		exec(`UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`, provisioned, parent)
		inactive := device(networkId, "JP")
		exec(`UPDATE network_client SET active = false WHERE client_id = $1`, inactive)

		connect.AssertEqual(t, GetNetworkCountrySignals(ctx, networkId, 32), &NetworkCountrySignals{
			Signup:      "IN",
			Billing:     "BR",
			Stores:      []string{"DE", "PH"},
			Connections: []string{"BR", "IN", "US"},
		})
		// the newest devices only
		connect.AssertEqual(t, GetNetworkCountrySignals(ctx, networkId, 2).Connections, []string{"BR", "IN"})
		connect.AssertEqual(t, GetNetworkCountrySignals(ctx, networkId, 0).Connections, []string{})
		connect.AssertEqual(t, GetNetworkCountrySignals(ctx, networkId, -1).Connections, []string{})
		connect.AssertEqual(t, GetNetworkCountrySignals(ctx, otherNetworkId, 32), &NetworkCountrySignals{
			Stores:      []string{},
			Connections: []string{"GB"},
		})

		// a placement whose country row is gone names no country
		exec(`DELETE FROM location WHERE location_id = $1`, countryIds["BR"])
		connect.AssertEqual(t, GetNetworkCountrySignals(ctx, networkId, 32).Connections, []string{"IN", "US"})
	})
}
