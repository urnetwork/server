package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// An App Store sandbox purchase (TestFlight, App Review) keeps the tester's
// Pro with a renewal that records no revenue. It is no subscription: on Main
// it made 3 of the 72 App Store networks counted as current paid accounts on
// 2026-11-08. An App Store grace period's hold records no revenue either, and
// stays: it holds a paid transaction.
func TestLoadSubscriptionMetricsSnapshotLeavesOutAppStoreSandbox(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().UTC().Truncate(time.Second)
		day := 24 * time.Hour
		addRenewal := func(networkId server.Id, transactionId string, start time.Time, end time.Time, netRevenue NanoCents) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: SubscriptionMarketApple,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         netRevenue,
				TransactionId:      transactionId,
			}); err != nil {
				t.Fatalf("add renewal: %v", err)
			}
		}

		// a TestFlight tester: daily sandbox renewals, no revenue
		tester := server.NewId()
		testingCreatePaymentNetworkRow(ctx, tester)
		addRenewal(tester, "sandbox-"+server.NewId().String(), now.Add(-2*day), now.Add(-day), 0)
		addRenewal(tester, "sandbox-"+server.NewId().String(), now.Add(-day), now.Add(day), 0)

		// a subscriber in the billing grace period: the paid month, then the
		// hold of the same transaction
		graced := server.NewId()
		testingCreatePaymentNetworkRow(ctx, graced)
		graceTransaction := "paid-" + server.NewId().String()
		addRenewal(graced, graceTransaction, now.Add(-32*day), now.Add(-day), UsdToNanoCents(3.99))
		addRenewal(graced, graceTransaction, now.Add(-32*day), now.Add(5*day), 0)

		// a paying subscriber
		paying := server.NewId()
		testingCreatePaymentNetworkRow(ctx, paying)
		addRenewal(paying, "paid-"+server.NewId().String(), now.Add(-3*day), now.Add(28*day), UsdToNanoCents(3.99))

		snapshot := LoadSubscriptionMetricsSnapshot(ctx, now)
		if got := snapshot.ActiveAccounts[SubscriptionMarketApple]; got != 2 {
			t.Errorf("active apple = %d, want 2: the graced and paying subscribers, not the tester", got)
		}
		if got := snapshot.ActiveAccounts[SubscriptionMetricsStoreDeduplicated]; got != 2 {
			t.Errorf("active deduplicated = %d, want 2", got)
		}
		week := SubscriptionMetricsStoreWindow{Store: SubscriptionMarketApple, Window: SubscriptionMetricsWindow7Days}
		if got := snapshot.NewPaidAccounts[week]; got != 1 {
			t.Errorf("new apple 7d = %d, want 1: the paying subscriber, not the tester", got)
		}
	})
}
