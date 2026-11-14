package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The Play sales notice reads a purchase's earlier credits, oldest first,
// from its renewal rows: retention deletes a balance 7 days after it ends,
// and nothing prunes a renewal row. A grace period's hold (no revenue), every
// other purchase and every other network's rows are left out.
func TestGetPlayPurchaseCreditEndTimesInTx(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, networkId)
		otherNetworkId := server.NewId()
		testingCreatePaymentNetworkRow(ctx, otherNetworkId)
		day := 24 * time.Hour
		// the trial began 40 days ago: its balances are long deleted
		start := server.NowUtc().Truncate(time.Second).Add(-40 * day)
		token := "token-" + server.NewId().String()
		add := func(networkId server.Id, market string, purchaseToken string, end time.Time, netRevenue NanoCents) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: market,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         netRevenue,
				PurchaseToken:      purchaseToken,
			}); err != nil {
				t.Fatalf("add renewal: %v", err)
			}
		}
		// the trial, Play's extra day, and the grace period's hold
		add(networkId, SubscriptionMarketGoogle, token, start.Add(15*day), UsdToNanoCents(3.5))
		add(networkId, SubscriptionMarketGoogle, token, start.Add(16*day+time.Second), UsdToNanoCents(3.5))
		add(networkId, SubscriptionMarketGoogle, token, start.Add(22*day), 0)
		// another purchase, the same token on another network, another market
		add(networkId, SubscriptionMarketGoogle, "token-other-"+server.NewId().String(), start.Add(30*day), UsdToNanoCents(3.5))
		add(otherNetworkId, SubscriptionMarketGoogle, token, start.Add(31*day), UsdToNanoCents(3.5))
		add(networkId, SubscriptionMarketStripe, token, start.Add(32*day), UsdToNanoCents(3.5))

		var endTimes []time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			endTimes = GetPlayPurchaseCreditEndTimesInTx(tx, ctx, networkId, token)
		})
		connect.AssertEqual(t, len(endTimes), 2)
		connect.AssertEqual(t, endTimes[0].Equal(start.Add(15*day)), true)
		connect.AssertEqual(t, endTimes[1].Equal(start.Add(16*day+time.Second)), true)

		server.Tx(ctx, func(tx server.PgTx) {
			connect.AssertEqual(t, len(GetPlayPurchaseCreditEndTimesInTx(tx, ctx, networkId, "")), 0)
		})
	})
}
