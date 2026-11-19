package monitor

import (
	"context"
	"strings"
	"testing"
)

// The api logs one fixed line when one network, Stripe customer or checkout
// runs more failed card checks than its burst count within its window
// (controller/subscription_stripe_card_failures.go stripeCardFailureBurstLine,
// whose test pins the same text). That is the shape of card testing, which in
// 2026-11 ran 20 cards in 92 seconds through two Checkout sessions with no
// alert. One line warns and three a minute for one kind of owner page; the
// alert names the kind of owner but never its id or the count.
func TestLogErrorsSignalAlertsOnACardFailureBurst(t *testing.T) {
	logErrors := func(lines ...string) []Alert {
		source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-api", nil
			}
			return strings.Join(lines, "\n") + "\n", nil
		}}
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}

	const intentLine = "W0829 11:59:42.879518       1 subscription_stripe_card_failures.go:360] [stripe]card failure burst: 6 failed card checks within 10 minutes for intent seti_synthetic_burst (latest setup invalid_card)"
	alert := requireAlertClass(t, logErrors(intentLine), "payment-card-failure-burst")
	if alert.Severity != SeverityWarn {
		t.Fatalf("one card failure burst severity = %s, want WARN", alert.Severity)
	}
	if !strings.Contains(alert.Observed, "frame=intent") {
		t.Fatalf("the burst is not attributed to its kind of owner: %s", alert.Observed)
	}
	requireAlertOmits(t, alert, "seti_synthetic_burst", "6 failed card checks")

	const networkId = "00000000-0000-4000-8000-00000000c4d1"
	networkLines := []string{
		"W0829 11:59:40.100000       1 subscription_stripe_card_failures.go:360] [stripe]card failure burst: 6 failed card checks within 10 minutes for network " + networkId + " (latest charge blocked)",
		"W0829 11:59:41.200000       1 subscription_stripe_card_failures.go:360] [stripe]card failure burst: 11 failed card checks within 10 minutes for network " + networkId + " (latest charge invalid_card)",
		"W0829 11:59:42.300000       1 subscription_stripe_card_failures.go:360] [stripe]card failure burst: 16 failed card checks within 10 minutes for network " + networkId + " (latest setup declined)",
	}
	alert = requireAlertClass(t, logErrors(networkLines...), "payment-card-failure-burst")
	if alert.Severity != SeverityPage {
		t.Fatalf("three card failure bursts in a minute severity = %s, want PAGE", alert.Severity)
	}
	if !strings.Contains(alert.Observed, "frame=network") {
		t.Fatalf("the bursts are not attributed to their kind of owner: %s", alert.Observed)
	}
	requireAlertOmits(t, alert, networkId)
}

// The api's other card failure lines, a webhook that only saw a failure, and
// a burst line of another shape are not a burst.
func TestLogErrorsSignalLeavesOtherCardFailureLinesAlone(t *testing.T) {
	for _, line := range []string{
		"I0829 11:59:42.879518       1 subscription_stripe_controller.go:390] type: setup_intent.setup_failed",
		"W0829 11:59:42.879518       1 subscription_stripe_card_failures.go:370] [stripe]card failure evt_synthetic (setup invalid_card) was counted but not added to its owner's window: redis: connection pool timeout",
		"W0829 11:59:42.879518       1 subscription_stripe_card_failures.go:350] [stripe]card failure burst: failed card checks for one network",
	} {
		tailer := newLogTailer("api", nil)
		tailer.classify(line)
		for _, finding := range tailer.drainWindow() {
			if finding.class == "payment-card-failure-burst" && !finding.healthy {
				t.Fatalf("line %q was classified as a card failure burst", line)
			}
		}
	}
}
