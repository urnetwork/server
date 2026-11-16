package model

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The revenue tests' fixed inputs: pro.yml's two tiers and the fees the credit
// paths take, so the expected dollars below do not move with the config repo.
func subscriptionMetricsRevenueTestSettings() *SubscriptionMetricsSettings {
	settings := DefaultSubscriptionMetricsSettings()
	settings.PriceTiers = []SubscriptionMetricsPriceTier{
		{Name: PriceTierStandard, YearlyUsd: 40, MonthlyUsd: 5},
		{Name: "regional", YearlyUsd: 4, MonthlyUsd: 0.5},
	}
	settings.StoreFeeFractions = map[string]float64{
		SubscriptionMarketStripe: 0.3,
		SubscriptionMarketApple:  0.2,
		SubscriptionMarketGoogle: 0.3,
	}
	// two ended trials are enough for a store's own rate here
	settings.TrialConversionMinTrials = 2
	return settings
}

type subscriptionMetricsRevenueFixture struct {
	t   testing.TB
	ctx context.Context
	now time.Time
}

func (self *subscriptionMetricsRevenueFixture) network() server.Id {
	networkId := server.NewId()
	testingCreatePaymentNetworkRow(self.ctx, networkId)
	return networkId
}

// a renewal row as a credit writes it; paidEnd nil is a row written before
// paid_end_time
func (self *subscriptionMetricsRevenueFixture) renewal(
	networkId server.Id,
	market string,
	transactionId string,
	purchaseToken string,
	start time.Time,
	end time.Time,
	netRevenue NanoCents,
	paidEnd *time.Time,
) {
	self.t.Helper()
	if err := AddSubscriptionRenewal(self.ctx, &SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   SubscriptionTypeSupporter,
		SubscriptionMarket: market,
		StartTime:          start,
		EndTime:            end,
		NetRevenue:         netRevenue,
		PurchaseToken:      purchaseToken,
		TransactionId:      transactionId,
		PaidEndTime:        paidEnd,
	}); err != nil {
		self.t.Fatalf("add renewal: %v", err)
	}
}

// a Stripe invoice's window: its period, and the grace its payment runs past
func (self *subscriptionMetricsRevenueFixture) stripe(networkId server.Id, invoiceId string, start time.Time, period time.Duration, netRevenue NanoCents) {
	self.t.Helper()
	paidEnd := start.Add(period)
	grace := 30 * 24 * time.Hour
	if netRevenue == 0 {
		grace = 24 * time.Hour
	}
	self.renewal(networkId, SubscriptionMarketStripe, invoiceId, "", start, paidEnd.Add(grace), netRevenue, &paidEnd)
}

// a Play credit of the purchase: from its start to the store's expiry, as
// every Play credit records it ($5 less 30%)
func (self *subscriptionMetricsRevenueFixture) play(networkId server.Id, purchaseToken string, start time.Time, expiry time.Time) {
	self.t.Helper()
	self.renewal(networkId, SubscriptionMarketGoogle, "", purchaseToken, start, expiry.Add(24*time.Hour), UsdToNanoCents(3.5), &expiry)
}

// an App Store transaction and its ledger row
func (self *subscriptionMetricsRevenueFixture) apple(
	networkId server.Id,
	start time.Time,
	period time.Duration,
	productId string,
	priceMilliunits int64,
	currency string,
	netRevenue NanoCents,
) {
	self.t.Helper()
	transactionId := "apple-" + server.NewId().String()
	expiry := start.Add(period)
	self.renewal(networkId, SubscriptionMarketApple, transactionId, "", start, expiry.Add(24*time.Hour), netRevenue, &expiry)
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`INSERT INTO apple_subscription_transaction
			 (transaction_id, notification_uuid, network_id, product_id, price_milliunits, currency)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			transactionId, server.NewId(), networkId, productId, priceMilliunits, currency,
		))
	})
}

// a one-time crypto or gamma term paid at start
func (self *subscriptionMetricsRevenueFixture) term(networkId server.Id, market string, transactionId string, start time.Time, period time.Duration, netRevenue NanoCents) {
	self.t.Helper()
	paidEnd := start.Add(period)
	self.renewal(networkId, market, transactionId, "", start, paidEnd.Add(30*24*time.Hour), netRevenue, &paidEnd)
}

func (self *subscriptionMetricsRevenueFixture) tier(networkId server.Id, market string, start time.Time, tier string) {
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`UPDATE subscription_renewal SET price_tier = $4
			 WHERE network_id = $1 AND market = $2 AND start_time = $3`,
			networkId, market, start, tier,
		))
	})
}

// the welcome offer, redeemed in the store
func (self *subscriptionMetricsRevenueFixture) offer(networkId server.Id, store string, percentOff int) {
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`INSERT INTO network_onboarding_offer
			 (network_id, issued_at, expires_at, issued_by, percent_off, months_free, redeemed_at, store)
			 VALUES ($1, $2, $3, 'in_app', $4, 3, $2, $5)`,
			networkId, self.now.Add(-5*24*time.Hour), self.now.Add(24*time.Hour), percentOff, store,
		))
	})
}

// the reconcile's store-side leg credited the invoice
func (self *subscriptionMetricsRevenueFixture) reconcileCredited(networkId server.Id, invoiceId string) {
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`INSERT INTO payment_reconciliation_event
			 (event_id, run_id, store, network_id, action, evidence, dry_run, event_time)
			 VALUES ($1, $2, 'stripe', $3, 'credited', $4, false, $5)`,
			server.NewId(), server.NewId(), networkId, invoiceId, self.now.Add(-time.Hour),
		))
	})
}

func subscriptionMetricsWantUsd(t testing.TB, got float64, want float64, name string) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %.6f, want %.6f", name, got, want)
	}
}

// Net new forecasted revenue (owner request 2026-11-16), and the expected
// trial conversion revenue it holds: each subscription that started in the
// window counts once, at its first paid window's yearly net, or, while its
// trial runs, at the trial's expected value. The conversion rate comes from
// the trials that ended 90 to 7 days ago.
func TestLoadSubscriptionMetricsSnapshotForecastsNewRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().UTC().Truncate(time.Second)
		day := 24 * time.Hour
		year := 365 * day
		month := 30 * day
		trial := 14 * day
		f := &subscriptionMetricsRevenueFixture{t: t, ctx: ctx, now: now}

		// ----- the conversion history: trials that ended 90 to 7 days ago -----

		// Stripe: two of four converted, at the start of the period after the
		// trial; a store with two ended trials has its own rate (0.5)
		for _, history := range []struct {
			start     time.Duration
			converted bool
		}{
			{40 * day, true},
			{50 * day, false},
			{60 * day, false},
			{31 * day, true},
		} {
			networkId := f.network()
			trialStart := now.Add(-history.start)
			f.stripe(networkId, "in_history_trial_"+server.NewId().String(), trialStart, trial, 0)
			if history.converted {
				f.stripe(networkId, "in_history_paid_"+server.NewId().String(), trialStart.Add(trial), year, UsdToNanoCents(21))
			}
		}
		// a payment sheet abandoned before its card: credited only by the
		// reconcile, never a trial in the rate (with it Stripe's rate would be
		// 2 of 5)
		abandonedHistory := f.network()
		abandonedHistoryInvoice := "in_abandoned_" + server.NewId().String()
		f.stripe(abandonedHistory, abandonedHistoryInvoice, now.Add(-45*day), trial, 0)
		f.reconcileCredited(abandonedHistory, abandonedHistoryInvoice)
		// ended 3 days ago: its first charge may still land
		f.stripe(f.network(), "in_recent_"+server.NewId().String(), now.Add(-17*day), trial, 0)
		// ended 100 days ago: outside the period
		f.stripe(f.network(), "in_old_"+server.NewId().String(), now.Add(-114*day), trial, 0)

		// Google Play: one trial, converted by the purchase's charged year;
		// one ended trial is too few for Play's own rate, so Play uses the
		// pooled rate: 3 converted of 5 = 0.6
		playHistory := f.network()
		playHistoryStart := now.Add(-40 * day)
		f.play(playHistory, "play-history", playHistoryStart, playHistoryStart.Add(trial))
		f.play(playHistory, "play-history", playHistoryStart, playHistoryStart.Add(trial+year))

		// ----- subscriptions that started in the last 30 days -----

		// a Stripe month, paid: $3.50 net x 12
		stripeMonthly := f.network()
		f.stripe(stripeMonthly, "in_monthly_"+server.NewId().String(), now.Add(-5*day), month, UsdToNanoCents(3.5))

		// a Solana year, paid: $40 x 1
		solanaYearly := f.network()
		f.term(solanaYearly, SubscriptionMarketSolana, "solana-"+server.NewId().String(), now.Add(-10*day), year, UsdToNanoCents(40))

		// a Stripe trial running now (standard tier): $40 less 30%, at
		// Stripe's rate 0.5
		stripeTrial := f.network()
		stripeTrialStart := now.Add(-3 * day)
		f.stripe(stripeTrial, "in_trial_"+server.NewId().String(), stripeTrialStart, trial, 0)
		f.tier(stripeTrial, SubscriptionMarketStripe, stripeTrialStart, PriceTierStandard)

		// a Play trial running now (regional tier, the welcome offer redeemed
		// on Play): $4 less 30%, less 25%, at the pooled rate 0.6
		playTrial := f.network()
		playTrialStart := now.Add(-2 * day)
		f.play(playTrial, "play-trial", playTrialStart, playTrialStart.Add(trial))
		f.tier(playTrial, SubscriptionMarketGoogle, playTrialStart, "regional")
		f.offer(playTrial, OnboardingStorePlay, 25)

		// an App Store sandbox purchase is no subscription
		sandbox := f.network()
		sandboxTransaction := "sandbox-" + server.NewId().String()
		f.renewal(sandbox, SubscriptionMarketApple, sandboxTransaction, "", now.Add(-day), now.Add(day), 0, nil)

		// a gift redemption is no sale, and its market is not tracked
		gifted := f.network()
		f.term(gifted, SubscriptionMarketGift, "gift-"+server.NewId().String(), now.Add(-4*day), year, UsdToNanoCents(28))

		// a payment sheet abandoned before its card, running now: no trial to
		// value, though the trials gauge still counts it
		abandoned := f.network()
		abandonedInvoice := "in_abandoned_" + server.NewId().String()
		abandonedStart := now.Add(-2 * day)
		abandonedPaidEnd := abandonedStart.Add(trial)
		f.renewal(abandoned, SubscriptionMarketStripe, abandonedInvoice, "", abandonedStart, abandonedPaidEnd.Add(30*day), 0, &abandonedPaidEnd)
		f.reconcileCredited(abandoned, abandonedInvoice)

		// a Play month charged at once (regional tier): the credit records
		// $3.50, a placeholder, so the month is $0.50 less 30%, x 12
		playMonthly := f.network()
		playMonthlyStart := now.Add(-6 * day)
		f.play(playMonthly, "play-monthly", playMonthlyStart, playMonthlyStart.Add(month))
		f.tier(playMonthly, SubscriptionMarketGoogle, playMonthlyStart, "regional")

		// an App Store year sold in euros (standard tier): the credit records
		// the $0.01 marker, so the year is $40 less 20%
		appleEuro := f.network()
		appleEuroStart := now.Add(-8 * day)
		f.apple(appleEuro, appleEuroStart, year, "supporter_yearly_26", 39990, "EUR", UsdToNanoCents(0.01))
		f.tier(appleEuro, SubscriptionMarketApple, appleEuroStart, PriceTierStandard)

		// an App Store month sold in dollars: its net, $4.99 less 20%, x 12
		appleMonthly := f.network()
		f.apple(appleMonthly, now.Add(-9*day), month, "supporter_monthly_26", 4990, "USD", UsdToNanoCents(3.992))

		// a Stripe trial that converted in the window: its paid year ($30
		// with the welcome offer, less 30%) x 1. Its trial ended 5 days ago,
		// inside the settle, so it is not in the conversion rate
		stripeConverted := f.network()
		convertedStart := now.Add(-19 * day)
		f.stripe(stripeConverted, "in_converted_trial_"+server.NewId().String(), convertedStart, trial, 0)
		f.stripe(stripeConverted, "in_converted_paid_"+server.NewId().String(), convertedStart.Add(trial), year, UsdToNanoCents(21))

		// a Stripe trial that ended without a charge: nothing
		f.stripe(f.network(), "in_lapsed_"+server.NewId().String(), now.Add(-20*day), trial, 0)

		// a first payment refunded inside its period: nothing
		refunded := f.network()
		refundedStart := now.Add(-7*day + time.Hour)
		refundedPaidEnd := refundedStart.Add(month)
		f.renewal(refunded, SubscriptionMarketStripe, "in_refunded_"+server.NewId().String(), "", refundedStart, now.Add(-6*day), UsdToNanoCents(3.5), &refundedPaidEnd)

		// a network that paid an App Store year ($39.99 less 20%), then a
		// Stripe month: each store counts its own first paid window, and the
		// deduplicated total only the first ever
		twoStores := f.network()
		f.apple(twoStores, now.Add(-12*day), year, "supporter_yearly_26", 39990, "USD", UsdToNanoCents(31.992))
		f.stripe(twoStores, "in_two_stores_"+server.NewId().String(), now.Add(-4*day), month, UsdToNanoCents(3.5))

		snapshot := LoadSubscriptionMetricsSnapshotWithSettings(ctx, now, subscriptionMetricsRevenueTestSettings())

		// the conversion rates
		sample := func(store string, outcome string) int64 {
			return snapshot.TrialConversionTrials[SubscriptionMetricsStoreOutcome{Store: store, Outcome: outcome}]
		}
		wantCount := func(got int64, want int64, name string) {
			t.Helper()
			if got != want {
				t.Errorf("%s = %d, want %d", name, got, want)
			}
		}
		wantCount(sample(SubscriptionMarketStripe, SubscriptionMetricsTrialOutcomeConverted), 2, "stripe trials converted")
		wantCount(sample(SubscriptionMarketStripe, SubscriptionMetricsTrialOutcomeLapsed), 2, "stripe trials lapsed (not the abandoned sheet, the recent or the old trial)")
		wantCount(sample(SubscriptionMarketGoogle, SubscriptionMetricsTrialOutcomeConverted), 1, "google trials converted")
		wantCount(sample(SubscriptionMarketGoogle, SubscriptionMetricsTrialOutcomeLapsed), 0, "google trials lapsed")
		wantCount(sample(SubscriptionMetricsStoreAll, SubscriptionMetricsTrialOutcomeConverted), 3, "all trials converted")
		wantCount(sample(SubscriptionMetricsStoreAll, SubscriptionMetricsTrialOutcomeLapsed), 2, "all trials lapsed")
		subscriptionMetricsWantUsd(t, snapshot.TrialConversionRates[SubscriptionMarketStripe], 0.5, "stripe conversion rate (its own)")
		subscriptionMetricsWantUsd(t, snapshot.TrialConversionRates[SubscriptionMarketGoogle], 0.6, "google conversion rate (pooled)")
		subscriptionMetricsWantUsd(t, snapshot.TrialConversionRates[SubscriptionMarketApple], 0.6, "apple conversion rate (pooled)")
		subscriptionMetricsWantUsd(t, snapshot.TrialConversionRates[SubscriptionMetricsStoreAll], 0.6, "pooled conversion rate")

		// the trials running now and their expected revenue
		stripeExpected := 40 * 0.7 * 0.5
		playExpected := 4 * 0.7 * 0.75 * 0.6
		subscriptionMetricsWantUsd(t, snapshot.TrialExpectedRevenueUsd[SubscriptionMarketStripe], stripeExpected, "stripe expected trial revenue")
		subscriptionMetricsWantUsd(t, snapshot.TrialExpectedRevenueUsd[SubscriptionMarketGoogle], playExpected, "google expected trial revenue")
		subscriptionMetricsWantUsd(t, snapshot.TrialExpectedRevenueUsd[SubscriptionMetricsStoreDeduplicated], stripeExpected+playExpected, "deduplicated expected trial revenue")
		wantCount(snapshot.TrialsConvertible[SubscriptionMarketStripe], 1, "convertible stripe trials (not the abandoned sheet)")
		wantCount(snapshot.TrialsConvertible[SubscriptionMarketGoogle], 1, "convertible google trials")
		wantCount(snapshot.TrialsConvertible[SubscriptionMetricsStoreDeduplicated], 2, "convertible trials")
		// the trials gauge keeps its meaning: the abandoned sheet counts there
		wantCount(snapshot.TrialsActive[SubscriptionMetricsStorePlan{Store: SubscriptionMarketStripe, Plan: PlanYearly}], 2, "active stripe trials")

		// the forecast
		forecast := func(store string, window string) float64 {
			return snapshot.NewForecastRevenueUsd[SubscriptionMetricsStoreWindow{Store: store, Window: window}]
		}
		stripeMonthlyYear := 3.5 * 12
		solanaYear := 40.0
		playMonthlyYear := 0.5 * 0.7 * 12
		appleEuroYear := 40 * 0.8
		appleMonthlyYear := 3.992 * 12
		stripeConvertedYear := 21.0
		twoStoresAppleYear := 31.992
		twoStoresStripeYear := 3.5 * 12
		subscriptionMetricsWantUsd(t, forecast(SubscriptionMarketStripe, SubscriptionMetricsWindow30Days),
			stripeMonthlyYear+stripeExpected+stripeConvertedYear+twoStoresStripeYear, "stripe 30d forecast")
		subscriptionMetricsWantUsd(t, forecast(SubscriptionMarketGoogle, SubscriptionMetricsWindow30Days),
			playExpected+playMonthlyYear, "google 30d forecast")
		subscriptionMetricsWantUsd(t, forecast(SubscriptionMarketApple, SubscriptionMetricsWindow30Days),
			appleEuroYear+appleMonthlyYear+twoStoresAppleYear, "apple 30d forecast (not the sandbox purchase)")
		subscriptionMetricsWantUsd(t, forecast(SubscriptionMarketSolana, SubscriptionMetricsWindow30Days),
			solanaYear, "solana 30d forecast")
		subscriptionMetricsWantUsd(t, forecast(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow30Days),
			stripeMonthlyYear+solanaYear+stripeExpected+playExpected+playMonthlyYear+appleEuroYear+appleMonthlyYear+stripeConvertedYear+twoStoresAppleYear,
			"deduplicated 30d forecast")
		// the 30-day forecast holds the expected trial revenue whole
		if got := forecast(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow30Days); got < snapshot.TrialExpectedRevenueUsd[SubscriptionMetricsStoreDeduplicated] {
			t.Errorf("deduplicated 30d forecast %.2f is less than its expected trial revenue %.2f", got, snapshot.TrialExpectedRevenueUsd[SubscriptionMetricsStoreDeduplicated])
		}
		// 7 days: the subscriptions and trials that started since
		subscriptionMetricsWantUsd(t, forecast(SubscriptionMarketStripe, SubscriptionMetricsWindow7Days),
			stripeMonthlyYear+stripeExpected+twoStoresStripeYear, "stripe 7d forecast")
		subscriptionMetricsWantUsd(t, forecast(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow7Days),
			stripeMonthlyYear+stripeExpected+playExpected+playMonthlyYear, "deduplicated 7d forecast (the two-store network began 12 days ago)")
		subscriptionMetricsWantUsd(t, forecast(SubscriptionMetricsStoreDeduplicated, SubscriptionMetricsWindow24Hours), 0, "deduplicated 24h forecast")
		// the gift market publishes nothing
		if _, ok := snapshot.NewForecastRevenueUsd[SubscriptionMetricsStoreWindow{Store: SubscriptionMarketGift, Window: SubscriptionMetricsWindow30Days}]; ok {
			t.Error("the gift market has a forecast")
		}
	})
}

// Net recurring revenue (owner request 2026-11-16): the net collected from
// renewals in the window, by when each payment was taken. A store's first paid
// window and trials are not renewals; a crypto year paid again is.
func TestLoadSubscriptionMetricsSnapshotCountsRecurringRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().UTC().Truncate(time.Second)
		day := 24 * time.Hour
		year := 365 * day
		month := 30 * day
		trial := 14 * day
		f := &subscriptionMetricsRevenueFixture{t: t, ctx: ctx, now: now}

		// Stripe: the first paid month 70 days ago is no renewal, the renewal
		// 40 days ago is outside the window, the one 10 days ago counts (its
		// payment dated by the invoice's credit)
		stripeRenewing := f.network()
		f.stripe(stripeRenewing, "in_first_"+server.NewId().String(), now.Add(-70*day), month, UsdToNanoCents(3.5))
		f.stripe(stripeRenewing, "in_old_renewal_"+server.NewId().String(), now.Add(-40*day), month, UsdToNanoCents(3.5))
		renewalInvoice := "in_renewal_" + server.NewId().String()
		f.stripe(stripeRenewing, renewalInvoice, now.Add(-10*day), month, UsdToNanoCents(3.5))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO stripe_invoice (invoice_id, network_id, process_time) VALUES ($1, $2, $3)`,
				renewalInvoice, stripeRenewing, now.Add(-10*day+time.Hour),
			))
		})
		// a renewal refunded inside its period: nothing
		refundedStart := now.Add(-8 * day)
		refundedPaidEnd := refundedStart.Add(month)
		f.renewal(stripeRenewing, SubscriptionMarketStripe, "in_refunded_"+server.NewId().String(), "", refundedStart, now.Add(-7*day), UsdToNanoCents(3.5), &refundedPaidEnd)

		// a first paid window in the window: no renewal
		f.stripe(f.network(), "in_new_"+server.NewId().String(), now.Add(-5*day), month, UsdToNanoCents(3.5))

		// trials: a Stripe trial, and a Play trial whose credit records $3.50,
		// then its first charge: none is a renewal
		f.stripe(f.network(), "in_trial_"+server.NewId().String(), now.Add(-3*day), trial, 0)
		playConverted := f.network()
		playConvertedStart := now.Add(-20 * day)
		f.play(playConverted, "play-converted", playConvertedStart, playConvertedStart.Add(trial))
		f.play(playConverted, "play-converted", playConvertedStart, playConvertedStart.Add(trial+year))

		// Play: a regional month renewed 10 days ago, valued at the tier's
		// $0.50 less 30%, not the $3.50 the credit records
		playRenewing := f.network()
		playStart := now.Add(-40 * day)
		f.play(playRenewing, "play-renewing", playStart, playStart.Add(month))
		f.play(playRenewing, "play-renewing", playStart, playStart.Add(2*month))
		f.tier(playRenewing, SubscriptionMarketGoogle, playStart, "regional")

		// a Solana year paid again 15 days ago is a renewal
		solanaRenewing := f.network()
		f.term(solanaRenewing, SubscriptionMarketSolana, "solana-"+server.NewId().String(), now.Add(-400*day), year, UsdToNanoCents(40))
		f.term(solanaRenewing, SubscriptionMarketSolana, "solana-"+server.NewId().String(), now.Add(-15*day), year, UsdToNanoCents(40))

		// a gamma renewal charged 2 days ago continues the paid term, which
		// starts in 5 days: dated by the intent's credit
		gammaRenewing := f.network()
		f.term(gammaRenewing, SubscriptionMarketGamma, server.NewId().String(), now.Add(-360*day), year, UsdToNanoCents(40))
		gammaIntent := server.NewId()
		f.term(gammaRenewing, SubscriptionMarketGamma, gammaIntent.String(), now.Add(5*day), year, UsdToNanoCents(40))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO gamma_payment_intent (
					intent_id, status_token_hash, network_id, hotkey, purpose, reference,
					delivery, item, price_usd_nano_cents, regular_price_usd_nano_cents,
					create_time, expires_at, credited_at
				) VALUES ($1, '\x00', $2, 'synthetic-hotkey', 'plan', $3, 'network', 'yearly', $4, $4, $5, $6, $7)`,
				gammaIntent, gammaRenewing, []byte(gammaIntent.String()), UsdToNanoCents(40),
				now.Add(-3*day), now.Add(day), now.Add(-2*day),
			))
		})

		// an App Store month renewed in euros 6 days ago (regional tier):
		// valued at the tier's $0.50 less 20%, not the $0.01 marker
		appleRenewing := f.network()
		f.apple(appleRenewing, now.Add(-100*day), month, "supporter_monthly_26", 4990, "USD", UsdToNanoCents(3.992))
		appleRenewalStart := now.Add(-6 * day)
		f.apple(appleRenewing, appleRenewalStart, month, "supporter_monthly_26", 549, "EUR", UsdToNanoCents(0.01))
		f.tier(appleRenewing, SubscriptionMarketApple, appleRenewalStart, "regional")

		// a gift redemption is no sale
		gifted := f.network()
		f.term(gifted, SubscriptionMarketGift, "gift-"+server.NewId().String(), now.Add(-400*day), year, UsdToNanoCents(28))
		f.term(gifted, SubscriptionMarketGift, "gift-"+server.NewId().String(), now.Add(-3*day), year, UsdToNanoCents(28))

		snapshot := LoadSubscriptionMetricsSnapshotWithSettings(ctx, now, subscriptionMetricsRevenueTestSettings())
		recurring := func(store string, window string) float64 {
			return snapshot.RecurringRevenueUsd[SubscriptionMetricsStoreWindow{Store: store, Window: window}]
		}
		subscriptionMetricsWantUsd(t, recurring(SubscriptionMarketStripe, SubscriptionMetricsWindow30Days), 3.5, "stripe 30d recurring (the renewal 10 days ago)")
		subscriptionMetricsWantUsd(t, recurring(SubscriptionMarketGoogle, SubscriptionMetricsWindow30Days), 0.5*0.7, "google 30d recurring (not the trial or its first charge)")
		subscriptionMetricsWantUsd(t, recurring(SubscriptionMarketSolana, SubscriptionMetricsWindow30Days), 40, "solana 30d recurring")
		subscriptionMetricsWantUsd(t, recurring(SubscriptionMarketGamma, SubscriptionMetricsWindow30Days), 40, "gamma 30d recurring")
		subscriptionMetricsWantUsd(t, recurring(SubscriptionMarketApple, SubscriptionMetricsWindow30Days), 0.5*0.8, "apple 30d recurring")
		subscriptionMetricsWantUsd(t, recurring(SubscriptionMetricsStoreAll, SubscriptionMetricsWindow30Days), 3.5+0.5*0.7+40+40+0.5*0.8, "all 30d recurring")
		subscriptionMetricsWantUsd(t, recurring(SubscriptionMetricsStoreAll, SubscriptionMetricsWindow7Days), 40+0.5*0.8, "all 7d recurring (gamma, the App Store)")
		subscriptionMetricsWantUsd(t, recurring(SubscriptionMetricsStoreAll, SubscriptionMetricsWindow24Hours), 0, "all 24h recurring")
		if _, ok := snapshot.RecurringRevenueUsd[SubscriptionMetricsStoreWindow{Store: SubscriptionMarketGift, Window: SubscriptionMetricsWindow30Days}]; ok {
			t.Error("the gift market has recurring revenue")
		}
	})
}
