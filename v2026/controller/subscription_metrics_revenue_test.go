package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// The revenue gauges publish every tracked store with a known zero where
// nothing was counted: the forecast and the expected trial revenue with the
// deduplicated total, recurring revenue and the conversion rate with the all
// total, in the same snapshot generation as the subscriber counts.
func TestSubscriptionMetricsCollectorPublishesRevenue(t *testing.T) {
	refreshedAt := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	snapshot := &model.SubscriptionMetricsSnapshot{
		NewForecastRevenueUsd: map[model.SubscriptionMetricsStoreWindow]float64{
			{Store: model.SubscriptionMarketStripe, Window: model.SubscriptionMetricsWindow30Days}:             119,
			{Store: model.SubscriptionMetricsStoreDeduplicated, Window: model.SubscriptionMetricsWindow30Days}: 234.356,
		},
		RecurringRevenueUsd: map[model.SubscriptionMetricsStoreWindow]float64{
			{Store: model.SubscriptionMarketSolana, Window: model.SubscriptionMetricsWindow30Days}:    40,
			{Store: model.SubscriptionMetricsStoreAll, Window: model.SubscriptionMetricsWindow30Days}: 84.25,
		},
		TrialExpectedRevenueUsd: map[string]float64{
			model.SubscriptionMarketStripe:             14,
			model.SubscriptionMetricsStoreDeduplicated: 15.26,
		},
		TrialsConvertible: map[string]int64{
			model.SubscriptionMarketStripe:             263,
			model.SubscriptionMetricsStoreDeduplicated: 290,
		},
		TrialConversionRates: map[string]float64{
			model.SubscriptionMarketStripe:    0.5,
			model.SubscriptionMetricsStoreAll: 0.6,
		},
		TrialConversionTrials: map[model.SubscriptionMetricsStoreOutcome]int64{
			{Store: model.SubscriptionMarketStripe, Outcome: model.SubscriptionMetricsTrialOutcomeConverted}: 2,
			{Store: model.SubscriptionMarketStripe, Outcome: model.SubscriptionMetricsTrialOutcomeLapsed}:    2,
		},
		RevenueLoaded: true,
	}
	collector := newSubscriptionMetricsCollector()
	collector.set(snapshot, refreshedAt)
	metricFamilies := gatherSubscriptionMetricFamilies(t, collector)

	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_revenue_refresh_failures", nil); got != 0 {
		t.Errorf("revenue refresh failures = %v, want 0", got)
	}
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"urnetwork_subscription_new_forecast_revenue_usd", map[string]string{"store": model.SubscriptionMarketStripe, "window": model.SubscriptionMetricsWindow30Days}, 119},
		{"urnetwork_subscription_new_forecast_revenue_usd", map[string]string{"store": model.SubscriptionMetricsStoreDeduplicated, "window": model.SubscriptionMetricsWindow30Days}, 234.356},
		{"urnetwork_subscription_new_forecast_revenue_usd", map[string]string{"store": model.SubscriptionMarketGamma, "window": model.SubscriptionMetricsWindow7Days}, 0},
		{"urnetwork_subscription_recurring_revenue_usd", map[string]string{"store": model.SubscriptionMarketSolana, "window": model.SubscriptionMetricsWindow30Days}, 40},
		{"urnetwork_subscription_recurring_revenue_usd", map[string]string{"store": model.SubscriptionMetricsStoreAll, "window": model.SubscriptionMetricsWindow30Days}, 84.25},
		{"urnetwork_subscription_recurring_revenue_usd", map[string]string{"store": model.SubscriptionMarketApple, "window": model.SubscriptionMetricsWindow24Hours}, 0},
		{"urnetwork_subscription_trial_expected_revenue_usd", map[string]string{"store": model.SubscriptionMarketStripe}, 14},
		{"urnetwork_subscription_trial_expected_revenue_usd", map[string]string{"store": model.SubscriptionMetricsStoreDeduplicated}, 15.26},
		{"urnetwork_subscription_trial_expected_revenue_usd", map[string]string{"store": model.SubscriptionMarketBitcoin}, 0},
		{"urnetwork_subscription_trials_convertible", map[string]string{"store": model.SubscriptionMarketStripe}, 263},
		{"urnetwork_subscription_trials_convertible", map[string]string{"store": model.SubscriptionMetricsStoreDeduplicated}, 290},
		{"urnetwork_subscription_trials_convertible", map[string]string{"store": model.SubscriptionMarketApple}, 0},
		{"urnetwork_subscription_trial_conversion_rate", map[string]string{"store": model.SubscriptionMarketStripe}, 0.5},
		{"urnetwork_subscription_trial_conversion_rate", map[string]string{"store": model.SubscriptionMetricsStoreAll}, 0.6},
		{"urnetwork_subscription_trial_conversion_rate", map[string]string{"store": model.SubscriptionMarketGoogle}, 0},
		{"urnetwork_subscription_trial_conversion_trials", map[string]string{"store": model.SubscriptionMarketStripe, "outcome": model.SubscriptionMetricsTrialOutcomeConverted}, 2},
		{"urnetwork_subscription_trial_conversion_trials", map[string]string{"store": model.SubscriptionMarketStripe, "outcome": model.SubscriptionMetricsTrialOutcomeLapsed}, 2},
		{"urnetwork_subscription_trial_conversion_trials", map[string]string{"store": model.SubscriptionMetricsStoreAll, "outcome": model.SubscriptionMetricsTrialOutcomeLapsed}, 0},
	} {
		if got := subscriptionMetricValue(t, metricFamilies, c.name, c.labels); got != c.want {
			t.Errorf("%s %v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}

	// ten tracked stores plus one total, per window or outcome where labeled
	for name, want := range map[string]int{
		"urnetwork_subscription_new_forecast_revenue_usd":   33,
		"urnetwork_subscription_recurring_revenue_usd":      33,
		"urnetwork_subscription_trial_expected_revenue_usd": 11,
		"urnetwork_subscription_trials_convertible":         11,
		"urnetwork_subscription_trial_conversion_rate":      11,
		"urnetwork_subscription_trial_conversion_trials":    22,
	} {
		if got := len(metricFamilies[name].Metric); got != want {
			t.Errorf("%s series = %d, want %d", name, got, want)
		}
	}
	// the totals are the label each gauge documents, never the other one
	for name, total := range map[string]string{
		"urnetwork_subscription_new_forecast_revenue_usd":   model.SubscriptionMetricsStoreAll,
		"urnetwork_subscription_recurring_revenue_usd":      model.SubscriptionMetricsStoreDeduplicated,
		"urnetwork_subscription_trial_expected_revenue_usd": model.SubscriptionMetricsStoreAll,
		"urnetwork_subscription_trial_conversion_rate":      model.SubscriptionMetricsStoreDeduplicated,
	} {
		for _, metric := range metricFamilies[name].Metric {
			for _, pair := range metric.Label {
				if pair.GetName() == "store" && pair.GetValue() == total {
					t.Errorf("%s publishes store=%s", name, total)
				}
			}
		}
	}
}

// The revenue values a price at the fee its credit path takes off: Stripe's
// invoice credit, the App Store's validation, and play.yml's supporter SKU,
// or the model's default when play.yml is absent or names none.
func TestSubscriptionMetricsStoreFeeFractionsAreTheCreditPaths(t *testing.T) {
	previous := playSkusFunc
	defer func() { playSkusFunc = previous }()

	playSkusFunc = func() map[string]*Sku {
		return map[string]*Sku{
			"monthly_transfer_1tib": {FeeFraction: 0.5},
			"supporter":             {FeeFraction: 0.15, Supporter: true},
		}
	}
	feeFractions := subscriptionMetricsStoreFeeFractions()
	for store, want := range map[string]float64{
		model.SubscriptionMarketStripe: stripeInvoiceFeeFraction,
		model.SubscriptionMarketApple:  appleTransactionFeeFraction,
		model.SubscriptionMarketGoogle: 0.15,
	} {
		if got := feeFractions[store]; got != want {
			t.Errorf("%s fee = %v, want %v", store, got, want)
		}
	}
	if stripeInvoiceFeeFraction != 0.3 || appleTransactionFeeFraction != 0.2 {
		t.Errorf("credit path fees = stripe %v, apple %v, want 0.3 and 0.2", stripeInvoiceFeeFraction, appleTransactionFeeFraction)
	}
	// the model's defaults are the same fees, for callers without a controller
	for store, fee := range model.DefaultSubscriptionMetricsStoreFeeFractions() {
		if store != model.SubscriptionMarketGoogle && feeFractions[store] != fee {
			t.Errorf("model default %s fee = %v, want the credit path's %v", store, fee, feeFractions[store])
		}
	}

	playSkusFunc = func() map[string]*Sku {
		panic("play.yml is not present")
	}
	if got, want := subscriptionMetricsStoreFeeFractions()[model.SubscriptionMarketGoogle], model.DefaultSubscriptionMetricsStoreFeeFractions()[model.SubscriptionMarketGoogle]; got != want {
		t.Errorf("google fee without play.yml = %v, want the default %v", got, want)
	}
	if got := subscriptionMetricsSettings().StoreFeeFractions[model.SubscriptionMarketStripe]; got != stripeInvoiceFeeFraction {
		t.Errorf("settings stripe fee = %v, want %v", got, stripeInvoiceFeeFraction)
	}
}

// The revenue is its own step of the snapshot. A snapshot whose revenue step
// failed publishes every other series as usual and no revenue series at all
// (no data, never a false zero, whatever the revenue fields hold), and the
// failures in a row are counted until a refresh succeeds.
func TestSubscriptionMetricsCollectorLeavesOutRevenueWhenItFailed(t *testing.T) {
	refreshedAt := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	revenueFamilies := []string{
		"urnetwork_subscription_new_forecast_revenue_usd",
		"urnetwork_subscription_recurring_revenue_usd",
		"urnetwork_subscription_trial_expected_revenue_usd",
		"urnetwork_subscription_trials_convertible",
		"urnetwork_subscription_trial_conversion_rate",
		"urnetwork_subscription_trial_conversion_trials",
	}
	failed := func() *model.SubscriptionMetricsSnapshot {
		return &model.SubscriptionMetricsSnapshot{
			ActiveAccounts: map[string]int64{
				model.SubscriptionMarketApple:              4,
				model.SubscriptionMetricsStoreDeduplicated: 9,
			},
			// a half-read revenue step: never published
			NewForecastRevenueUsd: map[model.SubscriptionMetricsStoreWindow]float64{
				{Store: model.SubscriptionMetricsStoreDeduplicated, Window: model.SubscriptionMetricsWindow30Days}: 99,
			},
			RevenueLoaded: false,
		}
	}
	collector := newSubscriptionMetricsCollector()
	for i, want := range []float64{1, 2} {
		collector.set(failed(), refreshedAt.Add(time.Duration(i)*15*time.Minute))
		metricFamilies := gatherSubscriptionMetricFamilies(t, collector)
		if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_active_accounts", map[string]string{"store": model.SubscriptionMarketApple}); got != 4 {
			t.Errorf("active apple = %v, want 4: the rest of the snapshot is published", got)
		}
		if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_snapshot_timestamp_seconds", nil); got != float64(refreshedAt.Add(time.Duration(i)*15*time.Minute).Unix()) {
			t.Errorf("snapshot timestamp = %v, want this refresh's", got)
		}
		if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_revenue_refresh_failures", nil); got != want {
			t.Errorf("revenue refresh failures = %v, want %v", got, want)
		}
		for _, name := range revenueFamilies {
			if family := metricFamilies[name]; family != nil && 0 < len(family.Metric) {
				t.Errorf("%s published %d series from a failed revenue step", name, len(family.Metric))
			}
		}
	}

	loaded := failed()
	loaded.RevenueLoaded = true
	collector.set(loaded, refreshedAt.Add(time.Hour))
	metricFamilies := gatherSubscriptionMetricFamilies(t, collector)
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_revenue_refresh_failures", nil); got != 0 {
		t.Errorf("revenue refresh failures after a success = %v, want 0", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_new_forecast_revenue_usd", map[string]string{"store": model.SubscriptionMetricsStoreDeduplicated, "window": model.SubscriptionMetricsWindow30Days}); got != 99 {
		t.Errorf("forecast after a success = %v, want 99", got)
	}
}
