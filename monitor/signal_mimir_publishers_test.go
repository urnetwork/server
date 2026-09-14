package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func mimirPublisherFixture(overrides map[string]string) string {
	keys := []string{
		"observation_schema", "expected_fronts", "alias_entries", "recognized_fronts",
		"missing_fronts", "unknown_fronts", "duplicate_fronts", "preferred_ordinal",
		"fluent_bit_active", "process_after_hosts", "connections_total",
		"connections_unknown", "connections_preferred", "connections_distinct_fronts",
	}
	values := map[string]string{
		"observation_schema": "1", "expected_fronts": "2", "alias_entries": "2",
		"recognized_fronts": "2", "missing_fronts": "0", "unknown_fronts": "0",
		"duplicate_fronts": "0", "preferred_ordinal": "1", "fluent_bit_active": "true",
		"process_after_hosts": "true", "connections_total": "2", "connections_unknown": "0",
		"connections_preferred": "2", "connections_distinct_fronts": "1",
	}
	for key, value := range overrides {
		values[key] = value
	}
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := values[key]; ok {
			lines = append(lines, key+"="+value)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func mimirPublisherSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "front-a.invalid", LANAddress: "192.0.2.10", Roles: []string{"services", "grafana"}},
		{Name: "front-b.invalid", LANAddress: "192.0.2.11", Roles: []string{"services", "grafana"}},
		{Name: "database.invalid", LANAddress: "192.0.2.20", Roles: []string{"pg-primary"}},
		{Name: "cache.invalid", LANAddress: "192.0.2.21", Roles: []string{"redis-cluster"}},
	}
	return settings
}

func TestMimirPublishersHealthySyntheticTopology(t *testing.T) {
	observations := map[string]string{
		"database.invalid": mimirPublisherFixture(nil),
		"cache.invalid": mimirPublisherFixture(map[string]string{
			"preferred_ordinal": "2", "connections_preferred": "2",
		}),
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			mimirPublishersMarker, "synthetic-grafana.local", "process_after_hosts",
			"connections_distinct_fronts", "state established", ":3100",
		} {
			if !strings.Contains(command, want) {
				t.Fatalf("publisher reducer lacks %q", want)
			}
		}
		return observations[host.Name], nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy publisher topology alerts=%+v", alerts)
	}
}

func TestMimirPublishersDetectCommonPreferenceWithoutTraffic(t *testing.T) {
	zeroTraffic := mimirPublisherFixture(map[string]string{
		"connections_total": "0", "connections_preferred": "0", "connections_distinct_fronts": "0",
	})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return zeroTraffic, nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "mimir-publisher-placement-drift")
	if alert.Target != "publisher-fleet" {
		t.Fatalf("common preference target=%q", alert.Target)
	}
	for _, want := range []string{"low traffic", "expected_distinct=2", "Do not raise Mimir limits"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("common-preference alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestMimirPublishersDetectMembershipAndGenerationDrift(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{
			name: "obsolete front",
			overrides: map[string]string{
				"alias_entries": "3", "unknown_fronts": "1",
			},
			want: "unknown_fronts=1",
		},
		{
			name:      "shipper predates policy",
			overrides: map[string]string{"process_after_hosts": "false"},
			want:      "process_after_hosts=false",
		},
		{
			name:      "shipper inactive",
			overrides: map[string]string{"fluent_bit_active": "false"},
			want:      "fluent_bit_active=false",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
				if host.Name == "database.invalid" {
					return mimirPublisherFixture(test.overrides), nil
				}
				return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
			}}
			alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "mimir-publisher-placement-drift")
			if !strings.Contains(alert.Markdown(), test.want) {
				t.Fatalf("placement alert lacks %q:\n%s", test.want, alert.Markdown())
			}
			requireAlertOmits(t, alert, "database.invalid", "cache.invalid", "192.0.2.10", "192.0.2.20")
		})
	}
}

func TestMimirPublishersDetectLiveConnectionDrift(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "database.invalid" {
			return mimirPublisherFixture(map[string]string{
				"connections_total": "4", "connections_preferred": "0", "connections_distinct_fronts": "1",
			}), nil
		}
		return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "mimir-publisher-connection-drift")
	for _, want := range []string{"connections_total=4", "connections_preferred=0", "port-3100"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("connection alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, "database.invalid", "192.0.2.10", "192.0.2.20")
}

func TestMimirPublishersMalformedObservationIsVisibilityOnly(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "database.invalid" {
			return mimirPublisherFixture(map[string]string{
				"connections_total": "private-fixture\nraw_address=192.0.2.99",
			}), nil
		}
		return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	requireAlertOmits(t, alert, "private-fixture", "raw_address", "192.0.2.99", "database.invalid")
}

func TestMimirPublishersCancellationReturnsContextErrorWithoutAlert(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "cache.invalid" {
			return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
		}
		return mimirPublisherFixture(nil), nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewMimirPublishersSignal().Run(ctx, mimirPublisherSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled run alerts=%+v err=%v", alerts, err)
	}
}

func TestMimirPublishersMetadataAndRegistration(t *testing.T) {
	signal := NewMimirPublishersSignal()
	if signal.Number() != "11.20c" || signal.Key() != "mimir-publishers" ||
		signal.ID() != "observability/mimir-publishers" || signal.Cadence() != 5*time.Minute {
		t.Fatalf("unexpected metadata: number=%s key=%s id=%s cadence=%s", signal.Number(), signal.Key(), signal.ID(), signal.Cadence())
	}
	selected, err := IncludeSignals(NewSignals(), "mimir-publishers")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Key() != "mimir-publishers" {
		t.Fatalf("registry selection=%v", selected)
	}
}
