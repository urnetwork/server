// Payment refusal headers outrank untrusted loader-detail signatures while
// retaining finite vendor attribution and existing notifier recovery ownership.
package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// One synthetic successful minute read exercises the public signal adapter.
func paymentWebhookPriorityAlerts(t testing.TB, lines string) Alerts {
	t.Helper()
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-api", nil
		}
		return lines, nil
	}}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal("synthetic payment log read failed")
	}
	return alerts
}

// A valid fixed producer header remains one PAGE when loader details also
// match an earlier generic resource/transport/panic signature. The private
// suffix never becomes another class's public sample or vendor identity.
func TestPaymentWebhookRefusalHeaderPrecedesLoaderDetails(t *testing.T) {
	for _, vendor := range []string{"play", "stripe", "coinbase", "helius"} {
		for _, detail := range []string{
			"Resource not found in vault (private-resource.yml)",
			"connection reset by peer private-endpoint-canary",
			"READONLY private-loader-canary",
			"panic: private-loader-canary",
		} {
			line := "W1027 01:02:03.000001 1 subscription_handlers.go:51] [" + vendor + "]webhook refused, verification secret unavailable: " + detail + " private-field-canary\n"
			alerts := paymentWebhookPriorityAlerts(t, line)
			if len(alerts) != 1 {
				t.Fatal("refusal suffix changed class or duplicated counting")
			}
			alert := alerts[0]
			if alert.Class != "payment-webhook-refused" || alert.Severity != SeverityPage || alert.Frame != vendor || alert.Sustain != 1 || alert.unobservable || !strings.Contains(alert.Observed, "rate=1/min") {
				t.Fatal("refusal lost fixed vendor PAGE policy")
			}
			requireAlertOmits(t, alert, detail, "private-resource.yml", "private-field-canary", "private-endpoint-canary", "private-loader-canary")
		}
	}
}

// Publisher/audience diagnostics must not manufacture a missing-secret fact.
// Details containing another vendor token cannot replace the first header.
func TestPaymentWebhookRefusalSamplePreservesVerificationAuthority(t *testing.T) {
	for _, kind := range []string{"secret", "publisher", "audience"} {
		alerts := paymentWebhookPriorityAlerts(t, "W1027 01:02:03.000001 1 subscription_controller.go:1] [play]webhook refused, verification "+kind+" unavailable: private-detail-canary [stripe]webhook refused, verification secret unavailable\n")
		if len(alerts) != 1 || alerts[0].Frame != "play" {
			t.Fatal("loader detail changed finite vendor attribution")
		}
		if !strings.Contains(alerts[0].Evidence, "sample: [play]webhook refused, verification input unavailable (details omitted)") {
			t.Fatal("fixed sample invented verifier kind")
		}
		requireAlertOmits(t, alerts[0], "private-detail-canary", "[stripe]", "verification secret unavailable")
	}
}

// Non-refusal and unknown-vendor records retain prior behavior; the moved
// rule cannot steal a plain generic resource or transport record.
func TestPaymentWebhookPriorityLeavesOtherRecordsUnchanged(t *testing.T) {
	for _, line := range []string{
		"[play-extra]webhook refused, verification secret unavailable: synthetic",
		"[unknown]webhook refused, verification secret unavailable: synthetic",
		"[play]webhook accepted",
		"verifyPlayAuth failed: Missing authorization.",
	} {
		tailer := newLogTailer("api", nil)
		tailer.classify(line)
		for _, finding := range tailer.drainWindow() {
			if finding.class == "payment-webhook-refused" && !finding.healthy {
				t.Fatal("nonmatching record became payment refusal")
			}
		}
	}
	alerts := paymentWebhookPriorityAlerts(t, "Resource not found in vault (synthetic.yml)\n")
	if len(alerts) != 1 || alerts[0].Class != "required-vault-resource" || alerts[0].Severity != SeverityWarn {
		t.Fatal("generic resource policy changed")
	}
	tailer := newLogTailer("api", nil)
	for range 50 {
		tailer.classify("connection reset by peer")
	}
	matched := false
	for _, finding := range tailer.drainWindow() {
		if finding.class == "conn-reset" && !finding.healthy {
			matched = finding.tier == tierWarn
		}
		if finding.class == "payment-webhook-refused" && !finding.healthy {
			t.Fatal("generic reset became payment refusal")
		}
	}
	if !matched {
		t.Fatal("generic transport policy changed")
	}
}

// Alert frames remain vendor-specific, while the actual notifier deliberately
// owns one probe/class/service incident and requires complete recovery runs.
// This uses only its in-memory reducer: no HTTP sender, credentials or network.
func TestPaymentWebhookFramesKeepNotifierIncidentAndRecovery(t *testing.T) {
	now := time.Date(2037, 1, 2, 3, 4, 0, 0, time.UTC)
	frames := map[string]bool{}
	identities := map[string]bool{}
	allAlerts := Alerts{}
	for _, vendor := range []string{"play", "stripe", "coinbase", "helius"} {
		alerts := paymentWebhookPriorityAlerts(t, "["+vendor+"]webhook refused, verification secret unavailable: Resource not found in vault (private-resource.yml)\n")
		if len(alerts) != 1 || alerts[0].Class != "payment-webhook-refused" || alerts[0].unobservable {
			t.Fatal("payment alert missing or visibility classified")
		}
		frames[alerts[0].Frame] = true
		identities[alerts[0].Identity()] = true
		allAlerts = append(allAlerts, alerts[0])
	}
	if len(frames) != 4 || len(identities) != 4 {
		t.Fatal("vendor frames merged in rendered alerts")
	}
	notifier := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	signalKey := NewLogErrorsSignal().Key()
	raised := notifier.observeWithLock(now, signalKey, SignalExecution{Alerts: allAlerts})
	if len(raised) != 1 || raised[0].kind != slackRaised || raised[0].alert.Severity != SeverityPage || len(notifier.keyIncidents) != 1 {
		t.Fatal("notifier class/service ownership changed")
	}
	if len(notifier.observeWithLock(now, signalKey, SignalExecution{})) != 0 {
		t.Fatal("one absence cleared payment incident")
	}
	if len(notifier.observeWithLock(now, signalKey, SignalExecution{Failed: true})) != 0 || len(notifier.keyIncidents) != 1 {
		t.Fatal("failed log window cleared incident")
	}
	if len(notifier.observeWithLock(now, signalKey, SignalExecution{})) != 0 {
		t.Fatal("unknown window retained recovery credit")
	}
	cleared := notifier.observeWithLock(now, signalKey, SignalExecution{})
	if len(cleared) != 1 || cleared[0].kind != slackCleared || cleared[0].alert.Severity != SeverityPage || len(notifier.keyIncidents) != 0 {
		t.Fatal("complete absence did not recover existing payment identity")
	}
}
