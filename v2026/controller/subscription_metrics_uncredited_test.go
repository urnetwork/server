package controller

// The received-but-uncredited payments of the subscriptions snapshot, as the
// collector publishes them.

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// The snapshot publishes the received-but-uncredited payments of every rail,
// a known zero included, and the oldest's age only for a rail with one: a
// USDC payment no record credited shows on the dashboard and in the
// monitor's view as a count and an age, never as no data.
func TestSubscriptionMetricsCollectorPublishesUncreditedPayments(t *testing.T) {
	refreshedAt := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	snapshot := &model.SubscriptionMetricsSnapshot{
		UncreditedPayments: map[string]int64{
			model.CryptoAssetUsdcSolana: 1,
			model.CryptoAssetXmr:        2,
		},
		UncreditedOldestAgeSeconds: map[string]int64{
			model.CryptoAssetUsdcSolana: 7200,
			model.CryptoAssetXmr:        3600,
		},
	}
	collector := newSubscriptionMetricsCollector()
	collector.set(snapshot, refreshedAt)
	metricFamilies := gatherSubscriptionMetricFamilies(t, collector)

	rails := model.SubscriptionMetricsUncreditedRails()
	if got := len(metricFamilies["urnetwork_subscription_payments_uncredited"].Metric); got != len(rails) {
		t.Fatalf("uncredited series = %d, want one per rail (%d)", got, len(rails))
	}
	for _, rail := range rails {
		want := float64(snapshot.UncreditedPayments[rail])
		if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_payments_uncredited", map[string]string{"rail": rail}); got != want {
			t.Errorf("uncredited %s = %v, want %v", rail, got, want)
		}
	}
	if got := len(metricFamilies["urnetwork_subscription_payments_uncredited_oldest_age_seconds"].Metric); got != 2 {
		t.Fatalf("oldest age series = %d, want the two rails that have a payment", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_payments_uncredited_oldest_age_seconds", map[string]string{"rail": model.CryptoAssetUsdcSolana}); got != 7200 {
		t.Fatalf("usdc_solana oldest age = %v, want 7200", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_payments_uncredited", map[string]string{"rail": model.SubscriptionMetricsRailGamma}); got != 0 {
		t.Fatalf("gamma uncredited = %v, want a known zero", got)
	}
}
