package controller

// Root-cause test of the card-failure watch (owner, 2026-11-13).
// charge.failed and setup_intent.setup_failed fell through StripeWebhook to
// "ignore and answer 200", so the 2026-11 card testing (20 cards in 92 seconds
// through two Checkout sessions) showed nowhere. This drives the public
// webhook through the real Redis window and reads the metrics from the default
// registry by name, so it reads the same before and after the change. Needs
// the test database and redis (server.DefaultTestEnv).

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The sum of the default registry's series of the counter whose labels
// include `labels`; 0 when the registry has no such counter.
func stripeCardFailureTestCounter(t testing.TB, name string, labels map[string]string) float64 {
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, metricFamily := range metricFamilies {
		if metricFamily.GetName() != name {
			continue
		}
		for _, metric := range metricFamily.GetMetric() {
			metricLabels := map[string]string{}
			for _, label := range metric.GetLabel() {
				metricLabels[label.GetName()] = label.GetValue()
			}
			matches := true
			for labelName, labelValue := range labels {
				matches = matches && metricLabels[labelName] == labelValue
			}
			if matches {
				total += metric.GetCounter().GetValue()
			}
		}
	}
	return total
}

// Six failed trial card checks through one Checkout session (one SetupIntent)
// are counted, a redelivered event once, and make one burst for the session.
func TestStripeCardTestingThroughOneCheckoutIsCountedAndAlerts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		setupIntentId := "seti_synthetic_" + server.NewId().String()
		failureLabels := map[string]string{"kind": "setup", "reason_class": "invalid_card"}
		burstLabels := map[string]string{"owner": "intent"}
		failuresBefore := stripeCardFailureTestCounter(t, "urnetwork_subscription_stripe_failures_total", failureLabels)
		burstsBefore := stripeCardFailureTestCounter(t, "urnetwork_subscription_stripe_failure_bursts_total", burstLabels)

		events := []*StripeWebhookArgs{}
		for range 6 {
			events = append(events, stripeWebhookEvent(t, "setup_intent.setup_failed", map[string]any{
				"id":     setupIntentId,
				"object": "setup_intent",
				"status": "requires_payment_method",
				"last_setup_error": map[string]any{
					"code":           "incorrect_number",
					"type":           "card_error",
					"payment_method": map[string]any{"id": "pm_synthetic", "type": "card"},
				},
			}))
		}
		// Stripe delivers at least once
		events = append(events, events[0])
		for _, event := range events {
			result, err := StripeWebhook(event, clientSession)
			if err != nil || result == nil {
				t.Fatalf("setup_intent.setup_failed: %+v %v", result, err)
			}
		}

		if got := stripeCardFailureTestCounter(t, "urnetwork_subscription_stripe_failures_total", failureLabels) - failuresBefore; got != 6 {
			t.Fatalf("failed card checks counted %v, want 6", got)
		}
		if got := stripeCardFailureTestCounter(t, "urnetwork_subscription_stripe_failure_bursts_total", burstLabels) - burstsBefore; got != 1 {
			t.Fatalf("bursts counted %v, want 1 for the six failures of one checkout", got)
		}
	})
}
