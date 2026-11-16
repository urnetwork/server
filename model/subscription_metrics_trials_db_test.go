package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The active trials gauge (owner request 2026-11-08): the networks inside a
// free trial now, by store and plan. A trial is a subscription and counts as
// a current subscriber too. A trial that converted, ended, or ran into a
// grace period after its first charge failed is no longer a trial. The
// windows are shaped as each store's credit writes them.
func TestLoadSubscriptionMetricsSnapshotCountsActiveTrials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().UTC().Truncate(time.Second)
		day := 24 * time.Hour
		// Apple and Play windows end a day past the store's expiry; Stripe's
		// run the 30-day manual-payment grace past the period
		storeGrace := day
		stripeGrace := 30 * day
		playRevenue := UsdToNanoCents(3.5)
		appleTrialMarker := UsdToNanoCents(0.01)

		newNetwork := func() server.Id {
			networkId := server.NewId()
			testingCreatePaymentNetworkRow(ctx, networkId)
			return networkId
		}
		addRenewal := func(networkId server.Id, market string, transactionId string, start time.Time, end time.Time, netRevenue NanoCents) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: market,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         netRevenue,
				TransactionId:      transactionId,
			}); err != nil {
				t.Fatalf("add renewal: %v", err)
			}
		}
		// a Play window of the network's purchase, from its start to the
		// store's expiry, as the credit writes it
		addPlayRenewal := func(networkId server.Id, start time.Time, expiry time.Time, netRevenue NanoCents) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: SubscriptionMarketGoogle,
				StartTime:          start,
				EndTime:            expiry.Add(storeGrace),
				NetRevenue:         netRevenue,
				PurchaseToken:      "play-token-" + networkId.String(),
			}); err != nil {
				t.Fatalf("add play renewal: %v", err)
			}
		}
		addAppleLedger := func(networkId server.Id, transactionId string, productId string, priceMilliunits int64) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO apple_subscription_transaction
					 (transaction_id, notification_uuid, network_id, product_id, price_milliunits, currency)
					 VALUES ($1, $2, $3, $4, $5, 'USD')`,
					transactionId, server.NewId(), networkId, productId, priceMilliunits,
				))
			})
		}

		// Google Play: every window starts at the purchase's start
		playTrial := newNetwork()
		playStart := now.Add(-3 * day)
		addPlayRenewal(playTrial, playStart, playStart.Add(14*day), playRevenue)

		// the day Play adds while the trial's first charge is pending
		playPending := newNetwork()
		pendingStart := now.Add(-14*day - time.Hour)
		addPlayRenewal(playPending, pendingStart, pendingStart.Add(14*day), playRevenue)
		addPlayRenewal(playPending, pendingStart, pendingStart.Add(15*day+time.Second), playRevenue)

		// converted as conversions land on Main (14, 15, 379 days): the trial
		// ended 6 hours ago, Play added its pending day, and the charge went
		// through inside it, so the pending day's window is still current
		playConverted := newNetwork()
		convertedStart := now.Add(-14*day - 6*time.Hour)
		addPlayRenewal(playConverted, convertedStart, convertedStart.Add(14*day), playRevenue)
		addPlayRenewal(playConverted, convertedStart, convertedStart.Add(15*day+time.Second), playRevenue)
		addPlayRenewal(playConverted, convertedStart, convertedStart.Add(379*day), playRevenue)

		// cancelled: the trial ran out
		playCancelled := newNetwork()
		cancelledStart := now.Add(-16 * day)
		addPlayRenewal(playCancelled, cancelledStart, cancelledStart.Add(14*day), playRevenue)

		// the first charge failed: Play's grace period holds Pro, with no
		// revenue
		playGrace := newNetwork()
		graceStart := now.Add(-17 * day)
		addPlayRenewal(playGrace, graceStart, graceStart.Add(14*day), playRevenue)
		addPlayRenewal(playGrace, graceStart, graceStart.Add(21*day), 0)

		// Stripe: the $0 trial invoice, inside its period
		stripeTrial := newNetwork()
		stripeStart := now.Add(-5 * day)
		addRenewal(stripeTrial, SubscriptionMarketStripe, "in_"+server.NewId().String(), stripeStart, stripeStart.Add(14*day+stripeGrace), 0)

		// past the trial with no charge yet: the manual-payment grace
		stripeUnpaid := newNetwork()
		unpaidStart := now.Add(-20 * day)
		addRenewal(stripeUnpaid, SubscriptionMarketStripe, "in_"+server.NewId().String(), unpaidStart, unpaidStart.Add(14*day+stripeGrace), 0)

		// converted: the paid year's invoice
		stripeConverted := newNetwork()
		stripeConvertedStart := now.Add(-16 * day)
		addRenewal(stripeConverted, SubscriptionMarketStripe, "in_"+server.NewId().String(), stripeConvertedStart, stripeConvertedStart.Add(14*day+stripeGrace), 0)
		addRenewal(stripeConverted, SubscriptionMarketStripe, "in_"+server.NewId().String(), stripeConvertedStart.Add(14*day), stripeConvertedStart.Add(379*day+stripeGrace), UsdToNanoCents(27.99))

		// cancelled during the trial: the reconcile ended its window
		stripeCancelled := newNetwork()
		stripeCancelledStart := now.Add(-6 * day)
		addRenewal(stripeCancelled, SubscriptionMarketStripe, "in_"+server.NewId().String(), stripeCancelledStart, now.Add(-time.Hour), 0)

		// App Store: the yearly product's free trial
		appleTrial := newNetwork()
		appleStart := now.Add(-2 * day)
		appleTrialTransaction := "apple-" + server.NewId().String()
		addRenewal(appleTrial, SubscriptionMarketApple, appleTrialTransaction, appleStart, appleStart.Add(14*day+storeGrace), appleTrialMarker)
		addAppleLedger(appleTrial, appleTrialTransaction, "supporter_yearly_26", 0)

		// a TestFlight tester's free sandbox purchase is no trial
		appleSandbox := newNetwork()
		sandboxTransaction := "apple-" + server.NewId().String()
		addRenewal(appleSandbox, SubscriptionMarketApple, sandboxTransaction, now.Add(-time.Hour), now.Add(-time.Hour+day+storeGrace), 0)
		addAppleLedger(appleSandbox, sandboxTransaction, "supporter_yearly_26", 0)

		// a paid month is no trial
		applePaid := newNetwork()
		paidTransaction := "apple-" + server.NewId().String()
		addRenewal(applePaid, SubscriptionMarketApple, paidTransaction, now.Add(-day), now.Add(30*day+storeGrace), UsdToNanoCents(3.99))
		addAppleLedger(applePaid, paidTransaction, "supporter_monthly_26", 4990)

		snapshot := LoadSubscriptionMetricsSnapshot(ctx, now)
		trials := func(store string, plan string) int64 {
			return snapshot.TrialsActive[SubscriptionMetricsStorePlan{Store: store, Plan: plan}]
		}
		want := func(got int64, want int64, name string) {
			t.Helper()
			if got != want {
				t.Errorf("%s = %d, want %d", name, got, want)
			}
		}
		want(trials(SubscriptionMarketGoogle, PlanYearly), 2, "google yearly trials (in trial, pending its first charge)")
		want(trials(SubscriptionMarketStripe, PlanYearly), 1, "stripe yearly trials")
		want(trials(SubscriptionMarketApple, PlanYearly), 1, "apple yearly trials")
		want(trials(SubscriptionMarketApple, PlanMonthly), 0, "apple monthly trials")
		want(trials(SubscriptionMetricsStoreDeduplicated, PlanYearly), 4, "deduplicated yearly trials")

		// trials are subscriptions: every trial counts as current
		want(snapshot.ActiveAccounts[SubscriptionMarketGoogle], 4, "active google (trial, pending, converted, grace)")
		want(snapshot.ActiveAccounts[SubscriptionMarketStripe], 3, "active stripe (trial, unpaid grace, converted)")
		want(snapshot.ActiveAccounts[SubscriptionMarketApple], 2, "active apple (trial, paid; not the tester)")
		week := func(store string) int64 {
			return snapshot.NewPaidAccounts[SubscriptionMetricsStoreWindow{Store: store, Window: SubscriptionMetricsWindow7Days}]
		}
		// a trial's start is a new subscription
		want(week(SubscriptionMarketGoogle), 1, "new google 7d (the trial)")
		want(week(SubscriptionMarketStripe), 2, "new stripe 7d (the trial and the trial cancelled during it)")
	})
}
