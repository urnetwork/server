package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// The active trials gauge publishes every tracked store and the deduplicated
// total, by plan, with a known zero where no trial runs, in the same snapshot
// generation as the subscriber counts it is part of.
func TestSubscriptionMetricsCollectorPublishesActiveTrials(t *testing.T) {
	refreshedAt := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	snapshot := &model.SubscriptionMetricsSnapshot{
		ActiveAccounts: map[string]int64{
			model.SubscriptionMarketGoogle:             81,
			model.SubscriptionMetricsStoreDeduplicated: 437,
		},
		TrialsActive: map[model.SubscriptionMetricsStorePlan]int64{
			{Store: model.SubscriptionMarketGoogle, Plan: model.PlanYearly}:             31,
			{Store: model.SubscriptionMarketStripe, Plan: model.PlanYearly}:             44,
			{Store: model.SubscriptionMetricsStoreDeduplicated, Plan: model.PlanYearly}: 75,
		},
	}
	collector := newSubscriptionMetricsCollector()
	collector.set(snapshot, refreshedAt)
	metricFamilies := gatherSubscriptionMetricFamilies(t, collector)

	for _, c := range []struct {
		store string
		plan  string
		want  float64
	}{
		{model.SubscriptionMarketGoogle, model.PlanYearly, 31},
		{model.SubscriptionMarketStripe, model.PlanYearly, 44},
		{model.SubscriptionMetricsStoreDeduplicated, model.PlanYearly, 75},
		{model.SubscriptionMarketApple, model.PlanYearly, 0},
		{model.SubscriptionMarketApple, model.PlanMonthly, 0},
		{model.SubscriptionMarketSolana, model.SubscriptionMetricsPlanUnknown, 0},
	} {
		got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_trials_active", map[string]string{"store": c.store, "plan": c.plan})
		if got != c.want {
			t.Errorf("trials %s/%s = %v, want %v", c.store, c.plan, got, c.want)
		}
	}
	// six tracked stores plus the deduplicated total, by yearly, monthly and
	// unknown plans
	if got := len(metricFamilies["urnetwork_subscription_trials_active"].Metric); got != 21 {
		t.Errorf("trial series = %d, want 7 stores x 3 plans", got)
	}
}
