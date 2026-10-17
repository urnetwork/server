// Bind analytics activation uncertainty to the class registry and the actual
// credential-alert/notifier boundary; no external webhook participates.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The conservative fallback for unregistered classes is not a declaration of
// ownership. Register this unknown state explicitly, preserving measured
// credential failures as incidents.
func TestAnalyticsActivationClassHasVisibilityDisposition(t *testing.T) {
	class := "credential-activation-unobservable"
	kind, registered := alertClassKinds[class]
	if !registered || kind != visibilityAlertClass || !isVisibilityAlertClass(class) {
		t.Fatal("analytics activation unknown lacks an explicit visibility-class disposition")
	}
	for _, measuredClass := range []string{"credential-resource-missing", "credential-resource-malformed", "credential-fields-missing"} {
		kind, registered := alertClassKinds[measuredClass]
		if !registered || kind != incidentAlertClass || isVisibilityAlertClass(measuredClass) {
			t.Fatal("measured credential readiness failure lost its incident disposition")
		}
	}
}

// A real unknown activation alert, including a held execution, cannot clear a
// previously observed credential PAGE. Two fresh complete disabled snapshots
// can clear it under the notifier's normal recovery rule.
func TestAnalyticsActivationVisibilityPreservesCredentialPage(t *testing.T) {
	analyticsActivationFixtureRoot(t)
	signal := NewCredentialsSignal()
	pageSettings := syntheticSettings(&syntheticSource{})
	pageSettings.Credentials = []CredentialRequirement{{
		Key: "analytics", Resource: "analytics.yml", Required: true, Present: true,
		MissingFields: []string{"bing_webmaster.api_key", "google_search_console.service_account_json"},
	}}
	if err := pageSettings.Validate(); err != nil {
		t.Fatalf("synthetic credential fixture preflight failed: %v", err)
	}
	pageAlerts, err := signal.Run(context.Background(), pageSettings)
	if err != nil {
		t.Fatal("validated synthetic credential signal returned an execution error")
	}
	if len(pageAlerts) != 1 {
		t.Fatal("measured missing analytics credentials did not produce exactly one alert")
	}
	if pageAlerts[0].Severity != SeverityPage || isVisibilityAlert(pageAlerts[0]) {
		t.Fatal("measured missing analytics credentials did not produce an ordinary PAGE")
	}
	popInvalid := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: [\n"))
	unknownSettings := analyticsActivationFixtureSettings(t, []string{"taskworker"})
	popInvalid()
	unknownAlerts, err := signal.Run(context.Background(), unknownSettings)
	if err != nil || len(unknownAlerts) != 1 || unknownAlerts[0].Class != "credential-activation-unobservable" ||
		unknownAlerts[0].Severity != SeverityWarn || !isVisibilityAlert(unknownAlerts[0]) {
		t.Fatal("analytics activation constructor did not retain WARN visibility semantics")
	}
	popDisabled := server.Config.PushSimpleResource("analytics.yml", []byte("enabled: false\n"))
	disabledSettings := analyticsActivationFixtureSettings(t, []string{"taskworker"})
	popDisabled()
	disabledAlerts, err := signal.Run(context.Background(), disabledSettings)
	if err != nil || len(disabledAlerts) != 0 {
		t.Fatal("fresh explicit disable did not establish the consumer's no-op")
	}

	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	notifier.Handle(signal, SignalExecution{Alerts: pageAlerts})
	for i := 0; i < 2; i++ {
		notifier.Handle(signal, SignalExecution{HeldAlerts: unknownAlerts})
	}
	for i := 0; i < 2; i++ {
		notifier.Handle(signal, SignalExecution{Alerts: unknownAlerts})
	}
	// A premature clear would raise this original PAGE a second time.
	notifier.Handle(signal, SignalExecution{Alerts: pageAlerts})
	for i := 0; i < 2; i++ {
		notifier.Handle(signal, SignalExecution{Alerts: disabledAlerts})
	}
	closeSlackNotifier(t, notifier)

	posts := webhook.posted()
	pageIdentity := "deployment/credentials · credential-fields-missing · analytics"
	unknownIdentity := "deployment/credentials · credential-activation-unobservable · analytics"
	if len(posts) != 4 || countSlackPosts(posts, "[PAGE] raised", pageIdentity) != 1 ||
		countSlackPosts(posts, "[WARN] raised", unknownIdentity) != 1 ||
		countSlackPosts(posts, "[CLEARED] PAGE", pageIdentity) != 1 ||
		countSlackPosts(posts, "[CLEARED] WARN", unknownIdentity) != 1 {
		t.Fatal("analytics activation visibility cleared or reraised a credential PAGE before complete recovery")
	}
}
