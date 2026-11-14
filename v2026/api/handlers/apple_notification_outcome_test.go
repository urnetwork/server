package handlers

// What became of each App Store Server Notification at the route
// (controller/store_notification_flow.go): a body that is not a
// notification, a JWS that does not verify against the pinned roots, a
// verified notification of another app, and an apple.yml that cannot build the
// verifier each count as their own outcome, once. The verifier is built from
// a test certificate chain; no request reaches the controller's database.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// One outcome of urnetwork_store_notifications_total for the App Store from
// the default registry: zero when the series is absent.
func appleNotificationOutcomeCountForTest(t testing.TB, outcome string) float64 {
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
			if labelValues["store"] == model.SubscriptionMarketApple && labelValues["outcome"] == outcome && metric.Counter != nil {
				return metric.Counter.GetValue()
			}
		}
	}
	return 0
}

// Each refused notification answers its status and counts as its outcome,
// once; no other outcome moves.
func TestAppleNotificationRefusalOutcomes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	chain := newAppleTestCertificateChain(t, now, now.Add(24*time.Hour))
	previousSource := appleNotificationVerifierSource
	appleNotificationVerifierSource = func() *appleNotificationVerifier { return appleTestVerifier(chain, now) }
	t.Cleanup(func() { appleNotificationVerifierSource = previousSource })

	signedEnvelope := func(signedPayload string) string {
		return `{"signedPayload":"` + signedPayload + `"}`
	}
	otherAppPayload := appleTestSignedNotification(t, chain, now, appleTestNotificationOptions{
		bundleId:    "network.example.other",
		environment: "Production",
	})
	// its signature fails before its claims are read
	tamperedPayload := tamperAppleTestJws(appleTestSignedNotification(t, chain, now, appleTestNotificationOptions{
		bundleId:    "network.example.app",
		environment: "Production",
	}))

	type refusalCase struct {
		name        string
		body        string
		unavailable bool
		status      int
		outcome     string
	}
	for _, refusal := range []refusalCase{
		{name: "not a notification", body: "synthetic", status: http.StatusBadRequest, outcome: controller.StoreNotificationRefusedMalformed},
		{name: "a JWS that does not verify", body: signedEnvelope(tamperedPayload), status: http.StatusBadRequest, outcome: controller.StoreNotificationRefusedSignature},
		{name: "another app's notification", body: signedEnvelope(otherAppPayload), status: http.StatusBadRequest, outcome: controller.StoreNotificationRefusedClaims},
		{name: "no verifier config", body: signedEnvelope(otherAppPayload), unavailable: true, status: http.StatusServiceUnavailable, outcome: controller.StoreNotificationRefusedUnavailable},
	} {
		if refusal.unavailable {
			appleNotificationVerifierSource = func() *appleNotificationVerifier {
				panic("apple.yml app_store_notifications is required")
			}
		} else {
			appleNotificationVerifierSource = func() *appleNotificationVerifier { return appleTestVerifier(chain, now) }
		}
		beforeOutcomeCounts := map[string]float64{}
		for _, outcome := range controller.StoreNotificationOutcomes {
			beforeOutcomeCounts[outcome] = appleNotificationOutcomeCountForTest(t, outcome)
		}

		request := httptest.NewRequest(http.MethodPost, "/apple/notification", strings.NewReader(refusal.body))
		recorder := httptest.NewRecorder()
		AppleNotification(recorder, request)
		if recorder.Code != refusal.status {
			t.Errorf("%s: answered %d, want %d", refusal.name, recorder.Code, refusal.status)
		}
		for _, outcome := range controller.StoreNotificationOutcomes {
			want := 0.0
			if outcome == refusal.outcome {
				want = 1
			}
			if got := appleNotificationOutcomeCountForTest(t, outcome) - beforeOutcomeCounts[outcome]; got != want {
				t.Errorf("%s: %s counted %v, want %v", refusal.name, outcome, got, want)
			}
		}
	}
}
