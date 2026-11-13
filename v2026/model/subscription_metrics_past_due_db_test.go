package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The past-due gauge and the failed-renewal counts of the subscriptions
// dashboard (owner, 2026-11-13). A network is past due while it holds a
// current window but none that covers now with the period it paid for: a
// grace after a failed renewal, a store's billing grace hold, the day after a
// free trial. The windows are shaped as each credit writes them, those written
// before paid_end_time included. A Stripe trial window credited now runs a day
// past its period, and is still a trial inside it; the trial query read every
// Stripe window as its end less 30 days, so it missed these.
func TestLoadSubscriptionMetricsSnapshotCountsPastDueAndPaymentFailures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().UTC().Truncate(time.Second)
		day := 24 * time.Hour
		manualGrace := 30 * day
		storeGrace := day

		newNetwork := func() server.Id {
			networkId := server.NewId()
			testingCreatePaymentNetworkRow(ctx, networkId)
			return networkId
		}
		// paidEnd nil writes the row as a credit before paid_end_time did
		addRenewal := func(networkId server.Id, market string, transactionId string, start time.Time, paidEnd *time.Time, end time.Time, netRevenue NanoCents) {
			t.Helper()
			if err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				SubscriptionMarket: market,
				StartTime:          start,
				EndTime:            end,
				NetRevenue:         netRevenue,
				TransactionId:      transactionId,
				PurchaseToken:      "token-" + transactionId,
				PaidEndTime:        paidEnd,
			}); err != nil {
				t.Fatalf("add renewal: %v", err)
			}
		}
		at := func(t time.Time) *time.Time {
			return &t
		}
		paid := UsdToNanoCents(3.5)

		// Stripe, paid, inside its period
		stripeActive := newNetwork()
		addRenewal(stripeActive, SubscriptionMarketStripe, "in_"+server.NewId().String(), now.Add(-day), at(now.Add(29*day)), now.Add(29*day+manualGrace), paid)

		// Stripe, paid, its renewal failing: the manual-payment grace
		stripeGrace := newNetwork()
		addRenewal(stripeGrace, SubscriptionMarketStripe, "in_"+server.NewId().String(), now.Add(-35*day), at(now.Add(-5*day)), now.Add(25*day), paid)

		// Stripe, a trial credited now whose first charge failed: its day
		stripeTrialDay := newNetwork()
		trialDayEnd := now.Add(-2 * time.Hour)
		addRenewal(stripeTrialDay, SubscriptionMarketStripe, "in_"+server.NewId().String(), trialDayEnd.Add(-14*day), at(trialDayEnd), trialDayEnd.Add(storeGrace), 0)

		// Stripe, a trial credited now, inside the trial
		stripeTrial := newNetwork()
		trialEnd := now.Add(3 * day)
		addRenewal(stripeTrial, SubscriptionMarketStripe, "in_"+server.NewId().String(), trialEnd.Add(-14*day), at(trialEnd), trialEnd.Add(storeGrace), 0)

		// Stripe, written before paid_end_time: a paid month in its grace
		stripeLegacyGrace := newNetwork()
		legacyStart := now.Add(-40 * day)
		addRenewal(stripeLegacyGrace, SubscriptionMarketStripe, "in_"+server.NewId().String(), legacyStart, nil, legacyStart.Add(31*day+manualGrace), paid)

		// Stripe, written before paid_end_time: a trial past its period, in
		// the 30-day grace that credit wrote
		stripeLegacyTrial := newNetwork()
		legacyTrialStart := now.Add(-20 * day)
		addRenewal(stripeLegacyTrial, SubscriptionMarketStripe, "in_"+server.NewId().String(), legacyTrialStart, nil, legacyTrialStart.Add(14*day+manualGrace), 0)

		// App Store, written before paid_end_time: a paid month
		applePaid := newNetwork()
		addRenewal(applePaid, SubscriptionMarketApple, "apple-"+server.NewId().String(), now.Add(-day), nil, now.Add(30*day+storeGrace), paid)

		// App Store billing grace: the hold beside the paid transaction it holds
		appleHold := newNetwork()
		holdTransaction := "apple-" + server.NewId().String()
		addRenewal(appleHold, SubscriptionMarketApple, holdTransaction, now.Add(-31*day), nil, now.Add(-day), paid)
		addRenewal(appleHold, SubscriptionMarketApple, holdTransaction, now.Add(-day), nil, now.Add(5*day), 0)

		// a Stripe grace beside a paid Play period: past due in Stripe only
		stripeGracePlayPaid := newNetwork()
		addRenewal(stripeGracePlayPaid, SubscriptionMarketStripe, "in_"+server.NewId().String(), now.Add(-35*day), at(now.Add(-5*day)), now.Add(25*day), paid)
		addRenewal(stripeGracePlayPaid, SubscriptionMarketGoogle, "play-"+server.NewId().String(), now.Add(-10*day), at(now.Add(20*day)), now.Add(20*day+storeGrace), paid)

		// failed renewals: still failing, and resolved at each age
		addFailure := func(resolveReason string, resolveAge time.Duration) {
			server.Tx(ctx, func(tx server.PgTx) {
				invoiceId := "in_" + server.NewId().String()
				RecordStripeInvoicePaymentFailureInTx(ctx, tx, &StripeInvoicePaymentFailure{
					InvoiceId:      invoiceId,
					NetworkId:      stripeGrace,
					CustomerId:     "cus_synthetic_past_due",
					SubscriptionId: "sub_" + invoiceId,
					AttemptCount:   1,
				}, now.Add(-45*day))
				if resolveReason != "" {
					ResolveStripeInvoicePaymentFailureInTx(ctx, tx, invoiceId, resolveReason, now.Add(-resolveAge))
				}
			})
		}
		addFailure("", 0)
		addFailure("", 0)
		addFailure(StripeInvoicePaymentFailureResolvePaid, 2*time.Hour)
		addFailure(StripeInvoicePaymentFailureResolveSubscriptionEnded, 3*day)
		addFailure(StripeInvoicePaymentFailureResolveUncollectible, 10*day)
		addFailure(StripeInvoicePaymentFailureResolveVoided, 40*day)

		snapshot := LoadSubscriptionMetricsSnapshot(ctx, now)
		want := func(got int64, want int64, name string) {
			t.Helper()
			if got != want {
				t.Errorf("%s = %d, want %d", name, got, want)
			}
		}
		want(snapshot.InGraceAccounts[SubscriptionMarketStripe], 5, "past due stripe (grace, trial day, legacy grace, legacy trial, beside play)")
		want(snapshot.InGraceAccounts[SubscriptionMarketApple], 1, "past due apple (the hold)")
		want(snapshot.InGraceAccounts[SubscriptionMarketGoogle], 0, "past due google")
		want(snapshot.InGraceAccounts[SubscriptionMetricsStoreDeduplicated], 5, "past due deduplicated (not the network paid through play)")
		want(snapshot.ActiveAccounts[SubscriptionMarketStripe], 7, "active stripe")

		want(snapshot.TrialsActive[SubscriptionMetricsStorePlan{Store: SubscriptionMarketStripe, Plan: PlanYearly}], 1, "stripe trials (inside the trial only)")

		want(snapshot.PaymentFailuresOpen[SubscriptionMarketStripe], 2, "open failures")
		resolved := func(reason string, window string) int64 {
			return snapshot.PaymentFailuresResolved[SubscriptionMetricsStoreReasonWindow{Store: SubscriptionMarketStripe, Reason: reason, Window: window}]
		}
		want(resolved(StripeInvoicePaymentFailureResolvePaid, SubscriptionMetricsWindow24Hours), 1, "paid 24h")
		want(resolved(StripeInvoicePaymentFailureResolvePaid, SubscriptionMetricsWindow30Days), 1, "paid 30d")
		want(resolved(StripeInvoicePaymentFailureResolveSubscriptionEnded, SubscriptionMetricsWindow24Hours), 0, "subscription ended 24h")
		want(resolved(StripeInvoicePaymentFailureResolveSubscriptionEnded, SubscriptionMetricsWindow7Days), 1, "subscription ended 7d")
		want(resolved(StripeInvoicePaymentFailureResolveUncollectible, SubscriptionMetricsWindow7Days), 0, "uncollectible 7d")
		want(resolved(StripeInvoicePaymentFailureResolveUncollectible, SubscriptionMetricsWindow30Days), 1, "uncollectible 30d")
		want(resolved(StripeInvoicePaymentFailureResolveVoided, SubscriptionMetricsWindow30Days), 0, "voided 30d (resolved 40 days ago)")
	})
}
