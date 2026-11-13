package controller

// The root cause of the 2026-10-19 to 10-27 Play outage going unnoticed: a
// refused Google Play notification left no trace but an Info log line, so
// eight days of refusals counted nowhere and nothing alerted. A refusal is
// now counted by its outcome (urnetwork_store_notifications_total). The test
// reads the default Prometheus registry and names only the route's existing
// verification, so it runs unchanged against the code before the count.

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
)

// One store and outcome of urnetwork_store_notifications_total from the
// default registry: zero when the series is absent.
func storeNotificationCountForTest(t testing.TB, store string, outcome string) float64 {
	t.Helper()
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, metricFamily := range metricFamilies {
		if metricFamily.GetName() != "urnetwork_store_notifications_total" {
			continue
		}
		for _, metric := range metricFamily.Metric {
			labelValues := map[string]string{}
			for _, pair := range metric.Label {
				labelValues[pair.GetName()] = pair.GetValue()
			}
			if labelValues["store"] == store && labelValues["outcome"] == outcome && metric.Counter != nil {
				return metric.Counter.GetValue()
			}
		}
	}
	return 0
}

// The configured publisher's push for another audience than google.yml's (the
// Pub/Sub subscription and our config disagree) is refused, and the refusal
// is counted as refused_audience, once.
func TestPlayNotificationRefusedForItsAudienceIsCounted(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("google.yml", []byte("webhook:\n  publisher_email: publisher@fixture.example\n  audience: "+syntheticPlayAudience+"\n")))
	setPlayIdTokenClaims(t, func(context.Context, string) (map[string]any, error) {
		return map[string]any{
			"email":          "publisher@fixture.example",
			"email_verified": true,
			"aud":            "https://push.fixture.example/another-subscription",
			"iss":            "https://accounts.google.com",
		}, nil
	})

	beforeCount := storeNotificationCountForTest(t, "google", "refused_audience")
	if _, err := verifyWebhookBody(t, VerifyPlayBody, "{}", map[string]string{"Authorization": "Bearer synthetic-id-token"}); err == nil {
		t.Fatal("a push for another audience was accepted")
	}
	if got := storeNotificationCountForTest(t, "google", "refused_audience") - beforeCount; got != 1 {
		t.Fatalf("the refused Play notification was counted %v times as refused_audience, want 1", got)
	}
}
