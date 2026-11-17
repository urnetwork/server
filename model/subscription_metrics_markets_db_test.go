package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A subscription paid with TAO, Bitcoin, ETH (the crypto rail) or gamma (Pay
// with Gamma) is a subscription like any other: current, new, engaged and
// churned per store and in the deduplicated totals. The account query once
// named only six markets, so these four counted as zero and were missing from
// the totals. Manual grants and x402 stay out, as before.
func TestLoadSubscriptionMetricsSnapshotCountsEveryPaymentMarket(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().UTC().Truncate(time.Second)
		day := 24 * time.Hour

		newNetwork := func() server.Id {
			networkId := server.NewId()
			testingCreatePaymentNetworkRow(ctx, networkId)
			return networkId
		}
		addRenewal := func(networkId server.Id, market string, start time.Time, end time.Time) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: market,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         UsdToNanoCents(40),
				TransactionId:      server.NewId().String(),
			}); err != nil {
				t.Fatalf("add %s renewal: %v", market, err)
			}
		}
		addTopLevelAuth := func(networkId server.Id, authTime time.Time) {
			t.Helper()
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic-market-device", "synthetic")
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_client SET auth_time = $2 WHERE client_id = $1`,
					clientId,
					authTime,
				))
			})
		}

		// per market: one current subscriber, three days in and seen two
		// hours ago, and one whose month ended two days ago
		for _, market := range subscriptionMetricsNewMarkets {
			current := newNetwork()
			// a one-time year runs the 30-day manual-payment grace past it
			addRenewal(current, market, now.Add(-3*day), now.Add((365+30-3)*day))
			addTopLevelAuth(current, now.Add(-2*time.Hour))

			churned := newNetwork()
			addRenewal(churned, market, now.Add(-40*day), now.Add(-2*day))
		}
		// a network that also pays in the App Store is one subscriber
		appleAndGamma := newNetwork()
		addRenewal(appleAndGamma, SubscriptionMarketApple, now.Add(-60*day), now.Add(5*day))
		addRenewal(appleAndGamma, SubscriptionMarketGamma, now.Add(-day), now.Add(29*day))
		// not subscriptions here
		addRenewal(newNetwork(), SubscriptionMarketManual, now.Add(-day), now.Add(29*day))
		addRenewal(newNetwork(), SubscriptionMarketX402, now.Add(-day), now.Add(29*day))

		// the coins' reconciliation legs audit and advance a watermark like
		// every other store's; gamma has no leg
		heartbeatTime := now.Add(-30 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, market := range []string{SubscriptionMarketBittensor, SubscriptionMarketBitcoin, SubscriptionMarketEthereum} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO payment_reconciliation_watermark (store, watermark_time, update_time) VALUES ($1, $2, $2)`,
					market,
					heartbeatTime,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO payment_reconciliation_event
					 (event_id, run_id, store, action, dry_run, event_time)
					 VALUES ($1, $2, $3, $4, false, $5)`,
					server.NewId(), server.NewId(), market, PaymentReconcileActionCredited, now.Add(-time.Hour),
				))
			}
		})

		snapshot := LoadSubscriptionMetricsSnapshot(ctx, now)
		want := func(got int64, want int64, name string) {
			t.Helper()
			if got != want {
				t.Errorf("%s = %d, want %d", name, got, want)
			}
		}
		storeWindow := func(store string, window string) SubscriptionMetricsStoreWindow {
			return SubscriptionMetricsStoreWindow{Store: store, Window: window}
		}
		for _, market := range subscriptionMetricsNewMarkets {
			currentCount := int64(1)
			newCount := int64(1)
			if market == SubscriptionMarketGamma {
				// appleAndGamma's gamma month began yesterday
				currentCount = 2
				newCount = 2
			}
			want(snapshot.ActiveAccounts[market], currentCount, "active "+market)
			want(snapshot.NewPaidAccounts[storeWindow(market, SubscriptionMetricsWindow7Days)], newCount, "new "+market+" 7d")
			want(snapshot.NewPaidAccounts[storeWindow(market, SubscriptionMetricsWindow30Days)], newCount, "new "+market+" 30d")
			want(snapshot.EngagedAccounts[storeWindow(market, SubscriptionMetricsWindow24Hours)], 1, "engaged "+market+" 24h")
			want(snapshot.ChurnedAccounts[storeWindow(market, SubscriptionMetricsWindow7Days)], 1, "churned "+market+" 7d")
		}
		want(snapshot.ActiveAccounts[SubscriptionMarketApple], 1, "active apple")
		// four current subscribers, one per new market, and appleAndGamma once
		want(snapshot.ActiveAccounts[SubscriptionMetricsStoreDeduplicated], 5, "active deduplicated")
		// appleAndGamma's first window was the App Store's, 60 days ago
		want(snapshot.NewPaidAccounts[storeWindow(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow7Days)], 4, "new deduplicated 7d")
		want(snapshot.EngagedAccounts[storeWindow(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow24Hours)], 4, "engaged deduplicated 24h")
		want(snapshot.ChurnedAccounts[storeWindow(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow7Days)], 4, "churned deduplicated 7d")
		for _, market := range []string{SubscriptionMarketManual, SubscriptionMarketX402} {
			if _, ok := snapshot.ActiveAccounts[market]; ok {
				t.Errorf("%s renewals were counted as subscriptions", market)
			}
		}

		for _, market := range []string{SubscriptionMarketBittensor, SubscriptionMarketBitcoin, SubscriptionMarketEthereum} {
			want(snapshot.ReconciliationEvents[SubscriptionMetricsStoreActionWindow{
				Store: market, Action: PaymentReconcileActionCredited, Window: SubscriptionMetricsWindow24Hours,
			}], 1, market+" credited 24h")
			if got := snapshot.ReconciliationWatermarks[market]; !got.Equal(heartbeatTime) {
				t.Errorf("%s watermark = %v, want %v", market, got, heartbeatTime)
			}
		}
		if _, ok := snapshot.ReconciliationWatermarks[SubscriptionMarketGamma]; ok {
			t.Errorf("gamma has a reconciliation watermark, but no reconciliation leg writes one")
		}
	})
}
