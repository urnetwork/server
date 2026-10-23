// Metadata availability is independent of exact-edge transport incident ownership.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// Only synthetic exact-address requests reach the in-process source callback.
func associationCompletenessSettings(t *testing.T, result func(string) (string, error)) (SignalSettings, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		calls.Add(1)
		if name != "curl" || len(args) == 0 {
			return "", errors.New("unexpected synthetic association command")
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "association.example:443:[2001:db8:19::10]") {
			return "", errors.New("synthetic association request lost exact-address binding")
		}
		path := strings.TrimPrefix(args[len(args)-1], "https://association.example")
		return result(path)
	}}
	settings := syntheticSettings(source)
	settings.WebsiteDomain = "association.example"
	settings.Hosts = []HostSettings{{
		Name:     "association-fixture-edge",
		EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "fixture0", Address: "2001:db8:19::10"}},
	}}
	return settings, calls
}

// Both platform contracts are real semantic fixtures, not arbitrary HTTP 200s.
func associationCompletenessHealthy(path string) (string, error) {
	body := validAppleAssociationFixture()
	if strings.HasSuffix(path, "/assetlinks.json") {
		body = validAssetlinksFixture()
	}
	return associationHTTPFixture(body, "200", "0", "application/json", "2001:db8:19::10"), nil
}

// One independently invalid document seeds the existing product identity.
func associationCompletenessBreach(path string) (string, error) {
	if strings.HasSuffix(path, "/assetlinks.json") {
		return associationHTTPFixture(`[]`, "200", "0", "application/json", "2001:db8:19::10"), nil
	}
	return associationCompletenessHealthy(path)
}

// Private transport bytes must not become metadata evidence or a product claim.
func associationCompletenessUnknown(exit string) (string, error) {
	return associationHTTPFixture("synthetic-private-association-body", "000", exit,
		"synthetic-private-association-type", "2001:db8:19::99"), errors.New("synthetic-private-association-error")
}

// The real notifier has an enabled local webhook and deterministic caller clock.
func associationCompletenessNotifier(t *testing.T) (*SlackAlertNotifier, *fakeSlackWebhook, *slackTestHarness) {
	t.Helper()
	webhook, url := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(url))
	t.Cleanup(func() { closeSlackNotifier(t, notifier) })
	return notifier, webhook, harness
}

// Exactly compose the public signal, actual cadence gate, and actual consumer.
func associationCompletenessTick(t *testing.T, signal Signal, settings SignalSettings, gate *cadenceAlertGate, notifier *SlackAlertNotifier, harness *slackTestHarness) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("association observation failed: %v", err)
	}
	ready, held := gate.split(signal, alerts)
	notifier.Handle(signal, SignalExecution{Alerts: ready, HeldAlerts: held, Failed: false})
	harness.advance(signal.Cadence())
	return alerts
}

// Read the real synchronous Handle state under its own lock, not a model reducer.
func associationCompletenessIncident(notifier *SlackAlertNotifier, signal Signal, alert Alert) (bool, int) {
	notifier.stateLock.Lock()
	defer notifier.stateLock.Unlock()
	incident := notifier.keyIncidents[slackIncidentKey(alert)]
	if incident == nil {
		return false, 0
	}
	return true, incident.signalKeyAbsentRuns[signal.Key()]
}

// Two unobserved documents cannot retire a previously established content incident.
func TestAssociationFilesTransportUnknownRetainsContentIncident(t *testing.T) {
	var phase atomic.Int32
	settings, calls := associationCompletenessSettings(t, func(path string) (string, error) {
		if phase.Load() == 0 {
			return associationCompletenessBreach(path)
		}
		return associationCompletenessUnknown(fmt.Sprint(phase.Load()))
	})
	signal := NewAssociationFilesSignal()
	gate := newCadenceAlertGate()
	notifier, _, harness := associationCompletenessNotifier(t)
	seed := requireAlertClass(t, associationCompletenessTick(t, signal, settings, gate, notifier, harness), "web-association-files")
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	if present, absent := associationCompletenessIncident(notifier, signal, seed); !present || absent != 0 || calls.Load() != 4 {
		t.Fatalf("seed prerequisites: present=%t absent=%d calls=%d", present, absent, calls.Load())
	}
	phase.Store(7)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	phase.Store(28)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	if calls.Load() != 8 {
		t.Fatalf("observations = %d, want two requests on all four public runs", calls.Load())
	}
	if present, absent := associationCompletenessIncident(notifier, signal, seed); !present || absent != 0 {
		t.Fatalf("unobserved metadata retired or advanced absence of prior content incident: present=%t absent=%d", present, absent)
	}
}

// Transport-only availability does not open a new static-file product episode.
func TestAssociationFilesTransportUnknownHasNoNewProductIncident(t *testing.T) {
	for _, exit := range []string{"7", "28"} {
		settings, calls := associationCompletenessSettings(t, func(string) (string, error) {
			return associationCompletenessUnknown(exit)
		})
		signal := NewAssociationFilesSignal()
		gate := newCadenceAlertGate()
		notifier, _, harness := associationCompletenessNotifier(t)
		for index := 0; index < 2; index++ {
			alerts := associationCompletenessTick(t, signal, settings, gate, notifier, harness)
			if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !isVisibilityAlert(alerts[0]) {
				t.Fatalf("transport metadata authority = %+v, want only own visibility", alerts)
			}
		}
		notifier.stateLock.Lock()
		products := 0
		for _, incident := range notifier.keyIncidents {
			if incident.alert.Class == "web-association-files" {
				products++
			}
		}
		notifier.stateLock.Unlock()
		if products != 0 || calls.Load() != 4 {
			t.Fatalf("transport-only products=%d requests=%d", products, calls.Load())
		}
	}
}

// A valid sibling cannot make the other platform document observable.
func TestAssociationFilesValidSiblingKeepsMetadataIncomplete(t *testing.T) {
	settings, calls := associationCompletenessSettings(t, func(path string) (string, error) {
		if strings.HasSuffix(path, "/assetlinks.json") {
			return associationCompletenessHealthy(path)
		}
		return associationCompletenessUnknown("28")
	})
	alerts, err := NewAssociationFilesSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || calls.Load() != 2 {
		t.Fatalf("mixed observation: alerts=%+v error=%v requests=%d", alerts, err, calls.Load())
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if !isVisibilityAlert(unknown) || unknown.Observed != "expected=2 checked=1 unobserved=1" {
		t.Fatalf("mixed metadata authority = %+v", unknown)
	}
}

// Independently observed HTTP failures survive beside unknown contract coverage.
func TestAssociationFilesPartialMetadataFailureRemainsPositive(t *testing.T) {
	settings, _ := associationCompletenessSettings(t, func(path string) (string, error) {
		if strings.HasSuffix(path, "/assetlinks.json") {
			return associationHTTPFixture("synthetic-public-refusal", "429", "0", "text/html", "2001:db8:19::10"), nil
		}
		return associationCompletenessUnknown("7")
	})
	alerts, err := NewAssociationFilesSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("partial positive = %+v error=%v", alerts, err)
	}
	positive := requireAlertClass(t, alerts, "web-association-files")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if isVisibilityAlert(positive) || !isVisibilityAlert(unknown) ||
		!strings.Contains(positive.Observed, "other_response_failures=1") ||
		!strings.Contains(positive.Evidence, "synthetic-public-refusal") {
		t.Fatalf("independent response contract changed: positive=%+v unknown=%+v", positive, unknown)
	}
}

// Only complete semantically valid observations supply the ordinary clear credit.
func TestAssociationFilesMeasuredRecoveryClearsAfterUnknown(t *testing.T) {
	var phase atomic.Int32
	settings, calls := associationCompletenessSettings(t, func(path string) (string, error) {
		switch phase.Load() {
		case 0:
			return associationCompletenessBreach(path)
		case 1:
			return associationCompletenessUnknown("28")
		default:
			return associationCompletenessHealthy(path)
		}
	})
	signal := NewAssociationFilesSignal()
	gate := newCadenceAlertGate()
	notifier, webhook, harness := associationCompletenessNotifier(t)
	seed := requireAlertClass(t, associationCompletenessTick(t, signal, settings, gate, notifier, harness), "web-association-files")
	phase.Store(1)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	phase.Store(2)
	if alerts := associationCompletenessTick(t, signal, settings, gate, notifier, harness); len(alerts) != 0 {
		t.Fatalf("valid metadata = %+v", alerts)
	}
	if present, absent := associationCompletenessIncident(notifier, signal, seed); !present || absent != 1 {
		t.Fatalf("first measured recovery: present=%t absent=%d", present, absent)
	}
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	if present, _ := associationCompletenessIncident(notifier, signal, seed); present {
		t.Fatal("two complete measured observations did not release the prior incident")
	}
	closeSlackNotifier(t, notifier)
	clears := 0
	for _, text := range webhook.posted() {
		if strings.Contains(text, "[CLEARED]") && strings.Contains(text, " · web-association-files · ") {
			clears++
		}
	}
	if clears != 1 || calls.Load() != 10 {
		t.Fatalf("product clears=%d observation requests=%d", clears, calls.Load())
	}
}

// Unobserved source output is never copied into public metadata availability.
func TestAssociationFilesTransportUnknownKeepsPrivateBytesOut(t *testing.T) {
	settings, _ := associationCompletenessSettings(t, func(string) (string, error) {
		return associationCompletenessUnknown("28")
	})
	signal := NewAssociationFilesSignal()
	gate := newCadenceAlertGate()
	notifier, webhook, harness := associationCompletenessNotifier(t)
	alerts := associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	raw, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, alert := range alerts {
		text += alert.Markdown()
	}
	closeSlackNotifier(t, notifier)
	text += strings.Join(webhook.posted(), "\n")
	for _, private := range []string{"synthetic-private-association-body", "synthetic-private-association-error", "synthetic-private-association-type", "2001:db8:19::99"} {
		if strings.Contains(text, private) {
			t.Fatalf("metadata availability leaked synthetic private bytes %q", private)
		}
	}
}

// Existing live-parent cancellation still suppresses partial probe publication.
func TestAssociationFilesTransportCompletenessPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings, calls := associationCompletenessSettings(t, func(string) (string, error) {
		cancel()
		return associationCompletenessUnknown("28")
	})
	alerts, err := NewAssociationFilesSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls.Load() < 1 || calls.Load() > 2 {
		t.Fatalf("canceled owning observation: alerts=%+v error=%v requests=%d", alerts, err, calls.Load())
	}
}

// Applicability remains separate from an enabled but unobserved document.
func TestAssociationFilesTransportCompletenessPreservesInapplicability(t *testing.T) {
	for _, unconfiguredDomain := range []bool{false, true} {
		settings, calls := associationCompletenessSettings(t, func(string) (string, error) {
			return "", errors.New("inapplicable association source must not run")
		})
		if unconfiguredDomain {
			settings.WebsiteDomain = ""
		} else {
			settings.Hosts[0].EdgeIPv6 = nil
		}
		alerts, err := NewAssociationFilesSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 || calls.Load() != 0 {
			t.Fatalf("inapplicable association observation: alerts=%+v error=%v requests=%d", alerts, err, calls.Load())
		}
	}
}
