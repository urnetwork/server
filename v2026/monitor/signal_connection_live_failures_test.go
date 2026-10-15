package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The registered signal for the live announcement failure counters. Before
// §2.16b nothing watched them, so a block whose reads and writes kept failing
// raised no warning.
func connectionLiveFailuresSignalForTest(t *testing.T) Signal {
	t.Helper()
	for _, signal := range NewSignals() {
		if signal.Key() == "connection-live-failures" {
			return signal
		}
	}
	t.Fatal("no registered signal watches urnetwork_connect_connection_sync_read_failures_total or urnetwork_connect_connection_worker_failures_total; sustained live connection failures raise no warning")
	return nil
}

// One synthetic block operation over the probe's range: its failures in the
// fewest-failure 2 minute window, and in the whole range.
type connectionLiveFailuresFixture struct {
	host      string
	block     string
	operation string
	windowMin float64
	total     float64
}

// A Mimir vector for the probe's query, sampled at now.
func connectionLiveFailuresFixtureJSON(t testing.TB, now time.Time, fixtures []connectionLiveFailuresFixture) string {
	t.Helper()
	result := []map[string]any{}
	sample := func(fixture connectionLiveFailuresFixture, class string, value float64) {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"host": fixture.host, "block": fixture.block, "operation": fixture.operation, "monitor_metric": class,
			},
			"value": []any{float64(now.Unix()), fmt.Sprintf("%g", value)},
		})
	}
	for _, fixture := range fixtures {
		sample(fixture, "window_min", fixture.windowMin)
		sample(fixture, "total", fixture.total)
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// Runs the signal over a Mimir payload, checking the query it sends.
func runConnectionLiveFailuresPayload(t *testing.T, now time.Time, payload string) Alerts {
	t.Helper()
	signal := connectionLiveFailuresSignalForTest(t)
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"urnetwork_connect_connection_sync_read_failures_total",
			"urnetwork_connect_connection_worker_failures_total",
			"%5B15m%3A1m%5D",
			"%5B2m%5D",
			"%5B15m%5D",
			"%22synthetic%22",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("connection live failures query missing %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func runConnectionLiveFailuresFixture(t *testing.T, fixtures []connectionLiveFailuresFixture) Alerts {
	t.Helper()
	now := time.Date(2026, 10, 14, 17, 30, 0, 0, time.UTC)
	return runConnectionLiveFailuresPayload(t, now, connectionLiveFailuresFixtureJSON(t, now, fixtures))
}

// Isolated failures, and blocks that fail nothing, are healthy.
func TestConnectionLiveFailuresSignalSyntheticHealthyFleet(t *testing.T) {
	alerts := runConnectionLiveFailuresFixture(t, []connectionLiveFailuresFixture{
		{host: "connect-a", block: "g1", operation: "sync_read", windowMin: 0, total: 40},
		{host: "connect-a", block: "g1", operation: "measurement_write", windowMin: 0, total: 0},
		{host: "connect-b", block: "g2", operation: "verify_egress_lease", windowMin: 0, total: 3},
	})
	if len(alerts) != 0 {
		t.Fatalf("isolated live failures alerted: %+v", alerts)
	}
}

// One connection whose lease key is corrupted fails in every window, but
// alone; it stays below the floor.
func TestConnectionLiveFailuresSignalSyntheticOneConnectionIsBelowTheFloor(t *testing.T) {
	alerts := runConnectionLiveFailuresFixture(t, []connectionLiveFailuresFixture{
		{host: "connect-a", block: "g1", operation: "verify_egress_lease", windowMin: 1, total: 8},
	})
	if len(alerts) != 0 {
		t.Fatalf("one failing connection alerted: %+v", alerts)
	}
}

// A block whose reads and writes fail in every window warns, naming the
// block's operations and not the healthy block.
func TestConnectionLiveFailuresSignalSyntheticSustainedBlock(t *testing.T) {
	alerts := runConnectionLiveFailuresFixture(t, []connectionLiveFailuresFixture{
		{host: "connect-a", block: "g1", operation: "sync_read", windowMin: 40, total: 3000},
		{host: "connect-a", block: "g1", operation: "measurement_write", windowMin: 12, total: 900},
		{host: "connect-a", block: "g1", operation: "verify_egress_lease", windowMin: 0, total: 2},
		{host: "connect-b", block: "g2", operation: "sync_read", windowMin: 0, total: 5},
	})
	alert := requireAlertClass(t, alerts, "connection-live-failures-sustained")
	if alert.SignalNumber != "2.16b" || alert.SignalKey != "connection-live-failures" || alert.Sustain != 2 {
		t.Fatalf("wrong sustained alert identity: %+v", alert)
	}
	for _, want := range []string{
		"2 Connect block operation(s) failed in every 2 minute window of the last 15m",
		"connect-a/g1 sync_read failures_15m=3000 window_min=40",
		"connect-a/g1 measurement_write failures_15m=900 window_min=12",
		"live connection failures retried with the transport kept",
		"SIGNALS.md §2.16b",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("sustained alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	for _, unwanted := range []string{"connect-b/g2", "verify_egress_lease failures_15m"} {
		if strings.Contains(alert.Markdown(), unwanted) {
			t.Fatalf("sustained alert names %q:\n%s", unwanted, alert.Markdown())
		}
	}
}

// A malformed or stale sample cannot be read as no failures.
func TestConnectionLiveFailuresSignalSyntheticStaleSampleIsUnobservable(t *testing.T) {
	now := time.Date(2026, 10, 14, 17, 30, 0, 0, time.UTC)
	payload := connectionLiveFailuresFixtureJSON(t, now.Add(-time.Hour), []connectionLiveFailuresFixture{
		{host: "connect-a", block: "g1", operation: "sync_read", windowMin: 40, total: 3000},
	})
	alert := requireAlertClass(t, runConnectionLiveFailuresPayload(t, now, payload), "connection-live-failures-unobservable")
	if !strings.Contains(alert.Markdown(), "visibility_reason=invalid_or_stale_metric") {
		t.Fatalf("unobservable alert has no structural reason:\n%s", alert.Markdown())
	}
}
