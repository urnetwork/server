// Exercise finite branch attribution through the public adapter using synthetic sources.
package monitor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// Require the public visibility identity, finite origin, and private-value omission.
func requireMimirPublisherOrigin(t *testing.T, alerts Alerts, target, origin string) Alert {
	t.Helper()
	var selected Alert
	count := 0
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-private", "database.invalid", "cache.invalid", "front-a.invalid", "front-b.invalid", "192.0.2.")
		if alert.Target == target && alert.Class == "cannot-observe" {
			selected = alert
			count++
		}
	}
	if count != 1 {
		t.Fatalf("origin target count=%d; want1", count)
	}
	want := "error_class=observation-unclassified publisher_observation_origin=" + origin
	if selected.Observed != want {
		t.Fatalf("publisher origin=%q; want %q", selected.Observed, want)
	}
	if selected.Identity() != "monitor/visibility|cannot-observe|"+target+"|" || selected.Severity != SeverityWarn || selected.Sustain != 2 || selected.PageSustain != 0 || selected.SignalKey != "mimir-publishers" || selected.SignalNumber != "11.20c" || !selected.unobservable {
		t.Fatal("origin metadata changed identity, tier, sustain or unknown state")
	}
	if !strings.Contains(selected.Markdown(), want) {
		t.Fatal("rendering discarded finite origin")
	}
	return selected
}

// Return role-matched synthetic samples without source or policy discovery.
func originMimirPublisherHealthy(target HostSettings) string {
	if target.Name == "cache.invalid" {
		return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"})
	}
	return mimirPublisherFixture(nil)
}

// Nonready desired policy never enters a host source, including unknown state values.
func TestMimirPublishersOriginDesiredLoadStateBeforeContact(t *testing.T) {
	for _, state := range []struct{ name, value string }{{"unavailable", "unavailable"}, {"invalid", "invalid"}, {"empty", ""}, {"unknown", "synthetic-private-state"}} {
		t.Run(state.name, func(t *testing.T) {
			var calls atomic.Int32
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				calls.Add(1)
				return "", errors.New("synthetic-private-unexpected-contact")
			}}
			settings := mimirPublisherSettings(source)
			settings.MimirPublishers.LoadState = state.value
			alerts, err := NewMimirPublishersSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 0 {
				t.Fatal("desired-policy failure contacted a host")
			}
			requireMimirPublisherOrigin(t, alerts, "pg-primary/mimir-publisher", "desired-load-state")
			requireMimirPublisherOrigin(t, alerts, "redis-cluster/mimir-publisher", "desired-load-state")
			requireMimirPublisherOrigin(t, alerts, "publisher-fleet", "fleet-preference-coverage")
			if len(alerts) != 3 {
				t.Fatal("desired-policy failure changed findings")
			}
		})
	}
}

// Missing and unmatched preferences refuse only the affected host before contact.
func TestMimirPublishersOriginDesiredMappingBeforeContact(t *testing.T) {
	for _, tc := range []struct{ name, preference, origin string }{{"missing", "", "desired-preference-missing"}, {"unmatched", "synthetic-private-front.invalid", "desired-front-unmatched"}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
				calls.Add(1)
				if target.Name != "cache.invalid" {
					t.Error("invalid desired preference contacted affected host")
					return "", errors.New("synthetic-private-contact")
				}
				return originMimirPublisherHealthy(target), nil
			}}
			settings := mimirPublisherSettings(source)
			if tc.preference == "" {
				delete(settings.MimirPublishers.PreferredFronts, "database.invalid")
			} else {
				settings.MimirPublishers.PreferredFronts["database.invalid"] = tc.preference
			}
			alerts, err := NewMimirPublishersSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("desired mapping changed independent source call count")
			}
			requireMimirPublisherOrigin(t, alerts, "pg-primary/mimir-publisher", tc.origin)
			requireMimirPublisherOrigin(t, alerts, "publisher-fleet", "fleet-preference-coverage")
			if len(alerts) != 2 {
				t.Fatal("missing mapping invented sibling drift")
			}
		})
	}
}

// Separate returned errors, malformed samples, and front counts without extra reads.
func TestMimirPublishersOriginSourceParseAndFrontCount(t *testing.T) {
	for _, tc := range []struct {
		name, output, origin string
		err                  error
	}{
		{name: "source", output: mimirPublisherFixture(nil), origin: "source-return", err: errors.New("synthetic-private-source-error")},
		{name: "parser", output: "synthetic-private-malformed", origin: "sample-parse"},
		{name: "front-count", output: mimirPublisherFixture(map[string]string{"expected_fronts": "3", "missing_fronts": "1"}), origin: "sample-front-count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
				calls.Add(1)
				if target.Name == "database.invalid" {
					return tc.output, tc.err
				}
				return originMimirPublisherHealthy(target), nil
			}}
			alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatal("origin metadata added or skipped source calls")
			}
			requireMimirPublisherOrigin(t, alerts, "pg-primary/mimir-publisher", tc.origin)
			requireMimirPublisherOrigin(t, alerts, "publisher-fleet", "fleet-preference-coverage")
			if len(alerts) != 2 {
				t.Fatal("partial source invented or cleared a fleet verdict")
			}
		})
	}
}

// Distinguish completed unknown route from fleet coverage; retain the connection tuple.
func TestMimirPublishersOriginRuntimeRouteAndDerivedFleet(t *testing.T) {
	var calls atomic.Int32
	source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
		calls.Add(1)
		if target.Name == "database.invalid" {
			return mimirPublisherFixture(map[string]string{"route_state": "unobservable"}), nil
		}
		return originMimirPublisherHealthy(target), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("derived fleet made an independent source call")
	}
	requireMimirPublisherOrigin(t, alerts, "pg-primary/mimir-publisher", "runtime-route-inputs")
	requireMimirPublisherOrigin(t, alerts, "publisher-fleet", "fleet-runtime-placement")
	requireMimirPublisherConnectionVisibility(t, alerts, "fluent_bit_active=true process_observable=true route_state=unobservable connections_observable=true connections_total=2")
	if len(alerts) != 3 {
		t.Fatal("completed unknown route changed independent visibility findings")
	}
}

// A failed member must preserve an independent known connection-drift positive.
func TestMimirPublishersOriginKeepsIndependentConnectionDrift(t *testing.T) {
	source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
		if target.Name == "database.invalid" {
			return "", errors.New("synthetic-private-source-error")
		}
		return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2", "connections_preferred": "0"}), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	// Check the independent positive first so an unknown-origin failure cannot hide it.
	found := false
	for _, alert := range alerts {
		if alert.Class == "mimir-publisher-connection-drift" && alert.Target == "redis-cluster/mimir-publisher" {
			found = true
			if strings.Contains(alert.Observed, "publisher_observation_origin=") {
				t.Fatal("origin modified independent positive")
			}
		}
	}
	if !found {
		t.Fatal("source-origin metadata suppressed independent connection drift")
	}
	requireMimirPublisherOrigin(t, alerts, "pg-primary/mimir-publisher", "source-return")
	requireMimirPublisherOrigin(t, alerts, "publisher-fleet", "fleet-preference-coverage")
}

// Explicit and intentionally shared policies retain healthy verdicts and read counts.
func TestMimirPublishersOriginHealthyAndSharedPolicyControls(t *testing.T) {
	for _, shared := range []bool{false, true} {
		var calls atomic.Int32
		source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
			calls.Add(1)
			if shared {
				return mimirPublisherFixture(nil), nil
			}
			return originMimirPublisherHealthy(target), nil
		}}
		settings := mimirPublisherSettings(source)
		if shared {
			settings.MimirPublishers.PreferredFronts["cache.invalid"] = "front-a.invalid"
		}
		alerts, err := NewMimirPublishersSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 || calls.Load() != 2 {
			t.Fatal("origin changed healthy explicit/shared desired policy")
		}
	}
}

// Global inventory refusal and completed connection visibility retain their fields.
func TestMimirPublishersOriginLeavesGlobalInventoryAndConnectionsUnchanged(t *testing.T) {
	var calls atomic.Int32
	source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
		calls.Add(1)
		return originMimirPublisherHealthy(target), nil
	}}
	settings := mimirPublisherSettings(source)
	settings.Hosts[0].LANAddress = ""
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), settings)
	if err != nil || calls.Load() != 0 || len(alerts) != 1 {
		t.Fatal("global inventory boundary changed")
	}
	if alerts[0].Target != "mimir-publishers/inventory" || alerts[0].Observed != "error_class=observation-unclassified" {
		t.Fatal("global inventory finding changed")
	}
	source = &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
		if target.Name == "database.invalid" {
			return mimirPublisherFixture(map[string]string{"connections_observable": "false"}), nil
		}
		return originMimirPublisherHealthy(target), nil
	}}
	alerts, err = NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireMimirPublisherConnectionVisibility(t, alerts, "fluent_bit_active=true process_observable=true route_state=expected connections_observable=false connections_total=2")
	if len(alerts) != 1 {
		t.Fatal("completed connection tuple gained extra metadata finding")
	}
}

// Unknown values stay private; a late canceled callback publishes no findings.
func TestMimirPublishersOriginUnknownEnumAndCanceledCallback(t *testing.T) {
	for _, origin := range []mimirPublisherObservationOrigin{mimirPublisherOriginUnknown, mimirPublisherObservationOrigin(255)} {
		finding := mimirPublisherCannotObserve("synthetic-target", errors.New("synthetic-private-origin-canary"), origin)
		if finding.observed != "error_class=observation-unclassified publisher_observation_origin=unknown" {
			t.Fatal("unknown enum exposed an arbitrary value or lost unknown state")
		}
		if strings.Contains(finding.observed, "synthetic-private") {
			t.Fatal("unknown origin leaked source error")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
		calls.Add(1)
		cancel()
		return originMimirPublisherHealthy(target), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(ctx, mimirPublisherSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls.Load() == 0 {
		t.Fatal("late canceled callback published origin findings or was not exercised")
	}
}

// Preserve the classified source error while adding only local origin metadata.
func TestMimirPublishersOriginPreservesExistingCommandErrorClass(t *testing.T) {
	heldErr := errors.New("permission denied synthetic-private-command")
	source := &syntheticSource{hostFn: func(target HostSettings, _ string) (string, error) {
		if target.Name == "database.invalid" {
			return mimirPublisherFixture(nil), heldErr
		}
		return originMimirPublisherHealthy(target), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	baseline := cannotObserveFinding("pg-primary/mimir-publisher", heldErr)
	found := false
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-private-command", "permission denied")
		if alert.Target != "pg-primary/mimir-publisher" {
			continue
		}
		found = true
		if alert.Observed != baseline.observed+" publisher_observation_origin=source-return" || alert.Action != baseline.action || alert.Class != baseline.class || alert.Sustain != baseline.sustain {
			t.Fatal("origin reclassified or changed existing command error behavior")
		}
	}
	if !found {
		t.Fatal("classified command source result missing")
	}
}
