package model

import (
	"math"
	"strings"
	"testing"
)

// A store's trials are valued at its own conversion rate when enough of them
// ended in the period, else at the pooled rate of every store's trials.
func TestSubscriptionMetricsTrialConversionRates(t *testing.T) {
	outcome := func(store string, converted int64, lapsed int64) map[SubscriptionMetricsStoreOutcome]int64 {
		return map[SubscriptionMetricsStoreOutcome]int64{
			{Store: store, Outcome: SubscriptionMetricsTrialOutcomeConverted}: converted,
			{Store: store, Outcome: SubscriptionMetricsTrialOutcomeLapsed}:    lapsed,
		}
	}
	merge := func(samples ...map[SubscriptionMetricsStoreOutcome]int64) map[SubscriptionMetricsStoreOutcome]int64 {
		merged := map[SubscriptionMetricsStoreOutcome]int64{}
		for _, sample := range samples {
			for key, value := range sample {
				merged[key] = value
			}
		}
		return merged
	}
	near := func(got float64, want float64, name string) {
		t.Helper()
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}

	// Stripe has 100 ended trials, the App Store 2: the App Store, Play and
	// every store that sells no trial use the pooled rate
	sample := merge(
		outcome(SubscriptionMarketStripe, 30, 70),
		outcome(SubscriptionMarketApple, 1, 1),
		// a pooled count in the sample is not read as a store's
		outcome(SubscriptionMetricsStoreAll, 1000, 1000),
	)
	rates := subscriptionMetricsTrialConversionRates(sample, 30)
	pooled := 31.0 / 102.0
	near(rates[SubscriptionMarketStripe], 0.3, "stripe rate (its own)")
	near(rates[SubscriptionMarketApple], pooled, "apple rate (pooled)")
	near(rates[SubscriptionMarketGoogle], pooled, "google rate (pooled)")
	near(rates[SubscriptionMarketSolana], pooled, "solana rate (pooled)")
	near(rates[SubscriptionMetricsStoreAll], pooled, "pooled rate")
	for _, store := range SubscriptionMetricsStores() {
		if _, ok := rates[store]; !ok {
			t.Errorf("no rate for %s", store)
		}
	}

	// exactly the minimum is enough
	near(subscriptionMetricsTrialConversionRates(sample, 2)[SubscriptionMarketApple], 0.5, "apple rate at the minimum")

	// no ended trial: no evidence of a conversion
	for store, rate := range subscriptionMetricsTrialConversionRates(map[SubscriptionMetricsStoreOutcome]int64{}, 30) {
		near(rate, 0, store+" rate with no trials")
	}

	// a non-positive minimum still needs one ended trial
	lapsedOnly := subscriptionMetricsTrialConversionRates(outcome(SubscriptionMarketGoogle, 0, 3), 0)
	near(lapsedOnly[SubscriptionMarketGoogle], 0, "google rate, every trial lapsed")
	near(lapsedOnly[SubscriptionMarketStripe], 0, "stripe rate, no trials, pooled")
}

func TestSubscriptionMetricsRevenueQueriesKeepContracts(t *testing.T) {
	trackedMarkets := subscriptionMetricsSqlList(SubscriptionMarkets)
	rows := subscriptionMetricsRevenueRowsSql()
	for _, required := range []string{
		// the account query's tracked set, App Store sandbox purchases left out
		"renewal.market IN (" + trackedMarkets + ")",
		"renewal.market = 'apple'\n              AND renewal.net_revenue_nano_cents = 0\n              AND NOT EXISTS (",
		"AND paid.transaction_id = renewal.transaction_id",
		// a $0 Stripe invoice only the reconcile's store-side leg credited (a
		// payment sheet abandoned before its card) is left out
		"WHERE event.store = 'stripe'\n      AND event.action = 'credited'\n      AND NOT event.dry_run",
		"renewal.market = 'stripe'\n              AND renewal.net_revenue_nano_cents = 0\n              AND EXISTS (",
		"WHERE credited.invoice_id = renewal.transaction_id",
		// the trial query's trials
		"WHEN sw.net_revenue_nano_cents = 0\n                AND sw.paid_end_time < sw.start_time + interval '27 days' THEN 'trial'",
		"WHEN sw.price_milliunits = 0\n                AND 0 < sw.net_revenue_nano_cents\n                AND sw.end_time - interval '24 hours' < sw.start_time + interval '27 days' THEN 'trial'",
		"WHEN credit.start_time + interval '2 days' <= credit.expiry_time\n                AND credit.expiry_time < credit.start_time + interval '27 days' THEN 'trial'",
		"(credit.start_time <= $1 AND $1 < credit.expiry_time AND NOT credit.purchase_charged)",
		"(sw.start_time <= $1 AND $1 < sw.end_time AND $1 < sw.paid_end_time)",
		// a paid window cut before its paid period counts nothing
		"WHEN sw.end_time < sw.billed_end_time THEN 'ended_early'",
		// placeholders: every Play credit, an App Store sale in another currency
		"COALESCE(0 < sw.price_milliunits AND sw.currency IS DISTINCT FROM 'USD', false)",
		// payment times
		"COALESCE(sw.invoice_process_time, sw.start_time)",
		"COALESCE(credit.previous_expiry_time, credit.start_time)",
		"COALESCE(sw.intent_credited_at, sw.start_time)",
	} {
		if !strings.Contains(rows, required) {
			t.Errorf("revenue rows are missing %q", required)
		}
	}
	for _, required := range []string{
		"WHERE $2 <= trial.trial_end_time\n      AND trial.trial_end_time < $3",
		"AND conversion.kind = 'paid'",
		"AND conversion.start_time <= ended.trial_end_time + $4::bigint * interval '1 second'",
		"GROUP BY r.network_id, r.market, r.purchase_token",
	} {
		if !strings.Contains(subscriptionMetricsTrialConversionQuery, required) {
			t.Errorf("trial conversion query is missing %q", required)
		}
	}
	for _, required := range []string{
		// each pair, and each network, valued once: a running trial that
		// started in the window, else the first paid window of a new one
		"ON metric_window.since_time <= running_trial.trial_start_time",
		"AND paid_window.store_paid_number = 1",
		"AND paid_window.network_paid_number = 1",
		"WHERE NOT EXISTS (",
		// renewals: every paid window after a store's first
		"WHERE 1 < paid_window.store_paid_number",
		"ON metric_window.since_time <= paid_window.payment_time",
		// the welcome offer of the store it was redeemed in
		"AND offer.store = CASE r.market WHEN 'google' THEN 'play' ELSE r.market END",
		"LEFT JOIN pro_tier default_tier\n      ON default_tier.tier_order = 1",
	} {
		if !strings.Contains(subscriptionMetricsRevenueQuery, required) {
			t.Errorf("revenue query is missing %q", required)
		}
	}
}
