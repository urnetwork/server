// Required exact-edge image coverage is independent of public transport classification.
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

// Only synthetic public and pinned image requests reach the source callback.
func emailCompletenessSettings(t *testing.T, result func(bool, string) (string, error)) (SignalSettings, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	source := &syntheticSource{localFn: func(name string, args ...string) (string, error) {
		calls.Add(1)
		if name != "curl" || len(args) == 0 {
			return "", errors.New("unexpected synthetic image command")
		}
		joined := strings.Join(args, " ")
		edge := strings.Contains(joined, "--resolve")
		if edge && !strings.Contains(joined, "email.example:443:[2001:db8:19::20]") {
			return "", errors.New("synthetic image lost exact-address binding")
		}
		if !strings.HasPrefix(args[len(args)-1], "https://email.example/images/emails/") {
			return "", errors.New("synthetic image lost domain or path")
		}
		return result(edge, strings.TrimPrefix(args[len(args)-1], "https://email.example"))
	}}
	settings := syntheticSettings(source)
	settings.WebsiteDomain = "email.example"
	settings.Hosts = []HostSettings{{Name: "email-fixture-edge", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "fixture0", Address: "2001:db8:19::20"}}}}
	return settings, calls
}

// A valid image is byte-bearing at every requested layer.
func emailCompletenessHealthy() (string, error) {
	return emailAssetHTTPFixture("200", "0", "image/png", "1024", "2001:db8:19::20"), nil
}

// Excluded exact-edge bytes are private and carry no image-content authority.
func emailCompletenessUnknown(exit string) (string, error) {
	return emailAssetHTTPFixture("000", exit, "synthetic-private-image-type", "0", "2001:db8:19::29"), errors.New("synthetic-private-image-error")
}

// One observed exact-edge response establishes the existing product identity.
func emailCompletenessBreach(edge bool, path string) (string, error) {
	if edge && path == emailAssets[0].path {
		return emailAssetHTTPFixture("429", "0", "text/html", "100", "2001:db8:19::20"), nil
	}
	return emailCompletenessHealthy()
}

// Two excluded exact-edge frames cannot retire a previously observed product failure.
func TestEmailAssetsExactUnknownRetainsContentIncident(t *testing.T) {
	var phase atomic.Int32
	settings, calls := emailCompletenessSettings(t, func(edge bool, path string) (string, error) {
		if phase.Load() == 0 {
			return emailCompletenessBreach(edge, path)
		}
		if edge {
			return emailCompletenessUnknown(fmt.Sprint(phase.Load()))
		}
		return emailCompletenessHealthy()
	})
	signal := NewEmailAssetsSignal()
	gate := newCadenceAlertGate()
	notifier, _, harness := associationCompletenessNotifier(t)
	seed := requireAlertClass(t, associationCompletenessTick(t, signal, settings, gate, notifier, harness), "web-email-assets")
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	if present, absent := associationCompletenessIncident(notifier, signal, seed); !present || absent != 0 || calls.Load() != 8 {
		t.Fatalf("seed prerequisites: present=%t absent=%d requests=%d", present, absent, calls.Load())
	}
	phase.Store(7)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	phase.Store(28)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	if calls.Load() != 16 {
		t.Fatalf("four complete source batches = %d, want16", calls.Load())
	}
	if present, absent := associationCompletenessIncident(notifier, signal, seed); !present || absent != 0 {
		t.Fatalf("unobserved exact edges retired or advanced prior product: present=%t absent=%d", present, absent)
	}
}

// Healthy public images plus excluded edges create only own visibility, not a product claim.
func TestEmailAssetsExactUnknownHasNoNewProductIncident(t *testing.T) {
	for _, exit := range []string{"7", "28"} {
		settings, calls := emailCompletenessSettings(t, func(edge bool, _ string) (string, error) {
			if edge {
				return emailCompletenessUnknown(exit)
			}
			return emailCompletenessHealthy()
		})
		signal := NewEmailAssetsSignal()
		gate := newCadenceAlertGate()
		notifier, _, harness := associationCompletenessNotifier(t)
		for i := 0; i < 2; i++ {
			alerts := associationCompletenessTick(t, signal, settings, gate, notifier, harness)
			if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !isVisibilityAlert(alerts[0]) || alerts[0].Observed != "expected=4 checked=2 unobserved=2" {
				t.Fatalf("exact-edge unknown authority = %+v", alerts)
			}
		}
		notifier.stateLock.Lock()
		products := 0
		for _, incident := range notifier.keyIncidents {
			if incident.alert.Class == "web-email-assets" || incident.alert.Class == "web-email-assets-transport" {
				products++
			}
		}
		notifier.stateLock.Unlock()
		if products != 0 || calls.Load() != 8 {
			t.Fatalf("unknown products=%d requests=%d", products, calls.Load())
		}
	}
}

// A valid exact-edge sibling cannot certify the omitted required image.
func TestEmailAssetsValidSiblingKeepsExactCoverageUnknown(t *testing.T) {
	settings, calls := emailCompletenessSettings(t, func(edge bool, path string) (string, error) {
		if edge && path == emailAssets[0].path {
			return emailCompletenessUnknown("28")
		}
		return emailCompletenessHealthy()
	})
	alerts, err := NewEmailAssetsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || calls.Load() != 4 {
		t.Fatalf("mixed image observation alerts=%+v error=%v calls=%d", alerts, err, calls.Load())
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if !isVisibilityAlert(unknown) || unknown.Observed != "expected=4 checked=3 unobserved=1" {
		t.Fatalf("mixed exact coverage = %+v", unknown)
	}
}

// Independently observed refusal remains a product positive beside skipped image coverage.
func TestEmailAssetsPartialProductFailureRemainsPositive(t *testing.T) {
	settings, _ := emailCompletenessSettings(t, func(edge bool, path string) (string, error) {
		if edge && path == emailAssets[1].path {
			return emailCompletenessUnknown("7")
		}
		return emailCompletenessBreach(edge, path)
	})
	alerts, err := NewEmailAssetsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 2 {
		t.Fatalf("partial image positive = %+v error=%v", alerts, err)
	}
	positive := requireAlertClass(t, alerts, "web-email-assets")
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if isVisibilityAlert(positive) || !isVisibilityAlert(unknown) || !strings.Contains(positive.Observed, "other_response_failures=1") || !strings.Contains(positive.Evidence, "http=429") {
		t.Fatalf("independent product lost: positive=%+v unknown=%+v", positive, unknown)
	}
}

// Existing public transport visibility already blocks recovery below and above its gate.
func TestEmailAssetsPublicTransportStillProtectsRecovery(t *testing.T) {
	var phase atomic.Int32
	settings, calls := emailCompletenessSettings(t, func(edge bool, path string) (string, error) {
		if phase.Load() == 0 {
			return emailCompletenessBreach(edge, path)
		}
		if !edge {
			return emailAssetHTTPFixture("000", "35", "", "0", "2001:db8:19::20"), errors.New("synthetic-public-tls-unavailable")
		}
		return emailCompletenessHealthy()
	})
	signal := NewEmailAssetsSignal()
	gate := newCadenceAlertGate()
	notifier, _, harness := associationCompletenessNotifier(t)
	seed := requireAlertClass(t, associationCompletenessTick(t, signal, settings, gate, notifier, harness), "web-email-assets")
	phase.Store(1)
	for i := 0; i < 2; i++ {
		alerts := associationCompletenessTick(t, signal, settings, gate, notifier, harness)
		transport := requireAlertClass(t, alerts, "web-email-assets-transport")
		if len(alerts) != 1 || !isVisibilityAlert(transport) || transport.Sustain != 2 {
			t.Fatalf("working public visibility changed: %+v", alerts)
		}
		if present, absent := associationCompletenessIncident(notifier, signal, seed); !present || absent != 0 {
			t.Fatalf("public transport recovery credit: present=%t absent=%d", present, absent)
		}
	}
	if calls.Load() != 12 {
		t.Fatalf("public control requests=%d, want12", calls.Load())
	}
}

// Two measured complete image observations recover normally after edge unknown.
func TestEmailAssetsMeasuredRecoveryClearsAfterExactUnknown(t *testing.T) {
	var phase atomic.Int32
	settings, calls := emailCompletenessSettings(t, func(edge bool, path string) (string, error) {
		if phase.Load() == 0 {
			return emailCompletenessBreach(edge, path)
		}
		if phase.Load() == 1 && edge {
			return emailCompletenessUnknown("28")
		}
		return emailCompletenessHealthy()
	})
	signal := NewEmailAssetsSignal()
	gate := newCadenceAlertGate()
	notifier, webhook, harness := associationCompletenessNotifier(t)
	seed := requireAlertClass(t, associationCompletenessTick(t, signal, settings, gate, notifier, harness), "web-email-assets")
	phase.Store(1)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	phase.Store(2)
	if alerts := associationCompletenessTick(t, signal, settings, gate, notifier, harness); len(alerts) != 0 {
		t.Fatalf("complete healthy images = %+v", alerts)
	}
	if present, absent := associationCompletenessIncident(notifier, signal, seed); !present || absent != 1 {
		t.Fatalf("first measured recovery present=%t absent=%d", present, absent)
	}
	associationCompletenessTick(t, signal, settings, gate, notifier, harness)
	if present, _ := associationCompletenessIncident(notifier, signal, seed); present {
		t.Fatal("two complete measured image frames did not recover")
	}
	closeSlackNotifier(t, notifier)
	clears := 0
	for _, text := range webhook.posted() {
		if strings.Contains(text, "[CLEARED]") && strings.Contains(text, " · web-email-assets · ") {
			clears++
		}
	}
	if clears != 1 || calls.Load() != 20 {
		t.Fatalf("image clears=%d requests=%d", clears, calls.Load())
	}
}

// Exact-edge private source fields do not enter availability alerts or queued posts.
func TestEmailAssetsExactUnknownKeepsPrivateBytesOut(t *testing.T) {
	settings, _ := emailCompletenessSettings(t, func(edge bool, _ string) (string, error) {
		if edge {
			return emailCompletenessUnknown("28")
		}
		return emailCompletenessHealthy()
	})
	signal := NewEmailAssetsSignal()
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
	for _, private := range []string{"synthetic-private-image-type", "synthetic-private-image-error", "2001:db8:19::29"} {
		if strings.Contains(text, private) {
			t.Fatalf("availability leaked synthetic private field %q", private)
		}
	}
}

// Existing live parent cancellation suppresses all partial image publication.
func TestEmailAssetsCompletenessPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings, calls := emailCompletenessSettings(t, func(bool, string) (string, error) { cancel(); return emailCompletenessUnknown("28") })
	alerts, err := NewEmailAssetsSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls.Load() < 1 || calls.Load() > 4 {
		t.Fatalf("canceled image observation alerts=%+v error=%v calls=%d", alerts, err, calls.Load())
	}
}

// No domain is inapplicable, while a configured public site still runs without exact edges.
func TestEmailAssetsCompletenessPreservesApplicability(t *testing.T) {
	settings, calls := emailCompletenessSettings(t, func(edge bool, _ string) (string, error) {
		if edge {
			return "", errors.New("no exact edges configured")
		}
		return emailCompletenessHealthy()
	})
	settings.Hosts[0].EdgeIPv6 = nil
	alerts, err := NewEmailAssetsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || calls.Load() != 2 {
		t.Fatalf("public-only images alerts=%+v error=%v calls=%d", alerts, err, calls.Load())
	}
	settings.WebsiteDomain = ""
	alerts, err = NewEmailAssetsSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || calls.Load() != 2 {
		t.Fatalf("unconfigured site alerts=%+v error=%v calls=%d", alerts, err, calls.Load())
	}
}
