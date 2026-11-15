package monitor

import (
	"context"
	"strings"
	"testing"
)

// The API answers a payment webhook 503 when its verification secret is
// unavailable and logs one warning per refused push. The line carries no
// error-shaped word, so the log tailer used to ignore it entirely: from
// 2026-10-19 Main refused about 74,000 Google Play notifications in seven days
// and no alert fired. One refused push is a lost or delayed payment event.
func TestLogErrorsSignalPagesOnRefusedPaymentWebhook(t *testing.T) {
	for _, test := range []struct {
		line    string
		webhook string
	}{
		{
			line:    "W1026 19:29:42.879518       1 subscription_handlers.go:51] [play]webhook refused, verification secret unavailable: google.yml: webhook.audience is missing",
			webhook: "play",
		},
		{
			line:    "W1026 19:29:42.879518       1 subscription_controller.go:1697] [play]webhook refused, verification audience unavailable: google.yml: webhook.audience is blank",
			webhook: "play",
		},
		{
			line:    "W1026 19:29:42.879518       1 subscription_handlers.go:51] [stripe]webhook refused, verification secret unavailable: stripe.yml: webhook.signing_secret is missing",
			webhook: "stripe",
		},
		{
			line:    "W1026 19:29:42.879518       1 subscription_handlers.go:51] [coinbase]webhook refused, verification secret unavailable: coinbase.yml: webhook.shared_secret is missing",
			webhook: "coinbase",
		},
		{
			line:    "W1026 19:29:42.879518       1 subscription_handlers.go:51] [helius]webhook refused, verification secret unavailable: helius.yml: helius.webhook_auth_header is missing",
			webhook: "helius",
		},
	} {
		source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-api", nil
			}
			// a single refused push pages
			return test.line + "\n", nil
		}}
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "payment-webhook-refused")
		if alert.Severity != SeverityPage {
			t.Fatalf("%s: refused payment webhook severity = %s, want PAGE", test.webhook, alert.Severity)
		}
		if !strings.Contains(alert.Observed, "frame="+test.webhook) {
			t.Fatalf("%s: refused payment webhook not attributed to its webhook: %s", test.webhook, alert.Observed)
		}
		// the fixed sample names the webhook only; the resource and field
		// detail stays in the service log
		detail := test.line[strings.Index(test.line, "unavailable: ")+len("unavailable: "):]
		requireAlertOmits(t, alert, detail, ".yml:")
	}
}

// A webhook that verified and then rejected one request (a bad signature, an
// unknown event) is not a configuration outage and must not use this class.
func TestLogErrorsSignalLeavesOtherWebhookLinesUnpaged(t *testing.T) {
	for _, line := range []string{
		"I1026 19:29:42.879518       1 subscription_controller.go:1713] verifyPlayAuth failed: Missing authorization.",
		"I1026 19:29:42.879518       1 subscription_stripe_controller.go:265] Stripe webhook hit: evt_synthetic",
	} {
		tailer := newLogTailer("api", nil)
		tailer.classify(line)
		for _, finding := range tailer.drainWindow() {
			if finding.class == "payment-webhook-refused" && !finding.healthy {
				t.Fatalf("line %q was classified as a refused payment webhook", line)
			}
		}
	}
}
