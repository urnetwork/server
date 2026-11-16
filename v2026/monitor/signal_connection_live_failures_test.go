package monitor

import (
	"context"
	"encoding/json"
	"errors"
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

// Mutates only synthetic vector rows, preserving the owning query and wire parser.
func connectionLiveFailuresMutatedPayload(t *testing.T, mutate func([]map[string]any) []map[string]any) (time.Time, string) {
	t.Helper()
	now := time.Date(2026, 10, 14, 17, 30, 0, 0, time.UTC)
	payload := connectionLiveFailuresFixtureJSON(t, now, []connectionLiveFailuresFixture{
		{host: "synthetic-private-host", block: "synthetic-private-block", operation: "synthetic-private-operation", windowMin: 1, total: 3000},
	})
	var frame struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string           `json:"resultType"`
			Result     []map[string]any `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatal(err)
	}
	frame.Data.Result = mutate(frame.Data.Result)
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return now, string(encoded)
}

// Missing is distinct from a measured zero, for either member and with healthy peers.
func TestConnectionLiveFailuresSignalMissingCompanionsAreUnobservable(t *testing.T) {
	for _, keep := range []int{0, 1} {
		for _, healthyPeer := range []bool{false, true} {
			now, payload := connectionLiveFailuresMutatedPayload(t, func(rows []map[string]any) []map[string]any {
				partial := rows[keep]
				if !healthyPeer {
					return []map[string]any{partial}
				}
				peers := []map[string]any{}
				for _, metric := range []string{"window_min", "total"} {
					peers = append(peers, map[string]any{
						"metric": map[string]any{"host": "synthetic-healthy", "block": "synthetic-block", "operation": "sync_read", "monitor_metric": metric},
						"value":  []any{float64(nowForConnectionLiveFailuresTest().Unix()), "0"},
					})
				}
				return append(peers, partial)
			})
			alerts := runConnectionLiveFailuresPayload(t, now, payload)
			alert := requireAlertClass(t, alerts, "connection-live-failures-unobservable")
			if len(alerts) != 1 || alert.Sustain != 2 || !strings.Contains(alert.Markdown(), "visibility_reason=incomplete_operation_pair") {
				t.Fatalf("keep=%d peer=%t: missing companion was not solely unknown", keep, healthyPeer)
			}
			encoded, err := json.Marshal(alerts)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(alert.Markdown()+string(encoded), "synthetic-private") {
				t.Fatal("partial sample identity escaped fixed visibility reason")
			}
		}
	}
}

// The shared fixed source clock avoids timing-dependent frame validity.
func nowForConnectionLiveFailuresTest() time.Time {
	return time.Date(2026, 10, 14, 17, 30, 0, 0, time.UTC)
}

// Repeated metric kinds are ambiguous even when equal, independently of row order.
func TestConnectionLiveFailuresSignalDuplicateMetricsAreUnobservable(t *testing.T) {
	for _, metricIndex := range []int{0, 1} {
		for _, duplicateValue := range []string{"0", "1", "3000"} {
			for _, reverse := range []bool{false, true} {
				now, payload := connectionLiveFailuresMutatedPayload(t, func(rows []map[string]any) []map[string]any {
					duplicate := map[string]any{"metric": rows[metricIndex]["metric"], "value": []any{float64(nowForConnectionLiveFailuresTest().Unix()), duplicateValue}}
					rows = append(rows, duplicate)
					if reverse {
						for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
							rows[i], rows[j] = rows[j], rows[i]
						}
					}
					return rows
				})
				alerts := runConnectionLiveFailuresPayload(t, now, payload)
				alert := requireAlertClass(t, alerts, "connection-live-failures-unobservable")
				if len(alerts) != 1 || !strings.Contains(alert.Markdown(), "visibility_reason=duplicate_operation_metric") {
					t.Fatalf("metric=%d value=%s reverse=%t: duplicate accepted", metricIndex, duplicateValue, reverse)
				}
			}
		}
	}
}

// Unknown peers cannot hide pressure proved by a different complete operation.
func TestConnectionLiveFailuresSignalCompletePressureSurvivesUnknownPeer(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		now, payload := connectionLiveFailuresMutatedPayload(t, func(rows []map[string]any) []map[string]any {
			peer := map[string]any{"metric": map[string]any{"host": "synthetic-peer", "block": "synthetic-block", "operation": "sync_read", "monitor_metric": "total"}, "value": rows[1]["value"]}
			rows = append(rows, peer)
			if duplicate {
				rows = append(rows, peer)
			}
			return rows
		})
		alerts := runConnectionLiveFailuresPayload(t, now, payload)
		requireAlertClass(t, alerts, "connection-live-failures-unobservable")
		pressure := requireAlertClass(t, alerts, "connection-live-failures-sustained")
		if len(alerts) != 2 || pressure.Sustain != 2 || strings.Contains(pressure.Markdown(), "synthetic-peer") || !strings.Contains(pressure.Markdown(), "1 Connect block operation(s)") {
			t.Fatal("complete pressure lost or ambiguous peer counted")
		}
	}
}

// Complete zero values and the inclusive floor retain their original meaning.
func TestConnectionLiveFailuresSignalCompletePairBoundaries(t *testing.T) {
	for _, fixture := range []connectionLiveFailuresFixture{
		{host: "synthetic-control", block: "synthetic-block", operation: "sync_read", windowMin: 0, total: 3000},
		{host: "synthetic-control", block: "synthetic-block", operation: "sync_read", windowMin: 1, total: 0},
		{host: "synthetic-control", block: "synthetic-block", operation: "sync_read", windowMin: 1, total: 29.999},
		{host: "synthetic-control", block: "synthetic-block", operation: "sync_read", windowMin: 1, total: 30},
	} {
		alerts := runConnectionLiveFailuresFixture(t, []connectionLiveFailuresFixture{fixture})
		if fixture.total == 30 {
			requireAlertClass(t, alerts, "connection-live-failures-sustained")
		} else if len(alerts) != 0 {
			t.Fatal("complete healthy pair became unknown or pressured")
		}
	}
}

// A partial frame must not emit a healthy finding that can clear prior pressure.
func TestConnectionLiveFailuresProbePartialFrameDoesNotEmitHealthySustained(t *testing.T) {
	now, payload := connectionLiveFailuresMutatedPayload(t, func(rows []map[string]any) []map[string]any { return rows[1:] })
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "synthetic-metrics", Roles: []string{"services"}})
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (connectionLiveFailuresProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].healthy || findings[0].class != "connection-live-failures-unobservable" {
		t.Fatal("partial evidence emitted a healthy conclusion")
	}
}

// A malformed member still invalidates the whole frame, including valid peers.
func TestConnectionLiveFailuresSignalInvalidMemberRemainsUnknown(t *testing.T) {
	for _, kind := range []string{"stale", "future", "negative", "nan", "infinite", "malformed", "unlabeled", "unknown-class"} {
		now, payload := connectionLiveFailuresMutatedPayload(t, func(rows []map[string]any) []map[string]any {
			row := map[string]any{"metric": map[string]any{"host": "synthetic-invalid", "block": "synthetic-block", "operation": "sync_read", "monitor_metric": "total"}, "value": []any{float64(nowForConnectionLiveFailuresTest().Unix()), "3000"}}
			sample := row["value"].([]any)
			switch kind {
			case "stale":
				sample[0] = float64(nowForConnectionLiveFailuresTest().Add(-6 * time.Minute).Unix())
			case "future":
				sample[0] = float64(nowForConnectionLiveFailuresTest().Add(time.Minute).Unix())
			case "negative":
				sample[1] = "-1"
			case "nan":
				sample[1] = "NaN"
			case "infinite":
				sample[1] = "+Inf"
			case "malformed":
				row["value"] = []any{"synthetic-invalid"}
			case "unlabeled":
				delete(row["metric"].(map[string]any), "host")
			case "unknown-class":
				row["metric"].(map[string]any)["monitor_metric"] = "synthetic-unknown"
			}
			return append(rows, row)
		})
		alerts := runConnectionLiveFailuresPayload(t, now, payload)
		requireAlertClass(t, alerts, "connection-live-failures-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("%s invalid member allowed pressure inference", kind)
		}
	}
}

// Query failures and cancellation cannot become a healthy or partial reduction.
func TestConnectionLiveFailuresSignalQueryErrorsRemainErrors(t *testing.T) {
	for _, kind := range []string{"no-services", "transport", "canceled", "malformed", "non-vector"} {
		calls := 0
		ctx, cancel := context.WithCancel(context.Background())
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			switch kind {
			case "transport":
				return "", errors.New("synthetic query unavailable")
			case "canceled":
				cancel()
				return "", context.Canceled
			case "malformed":
				return "{", nil
			default:
				return `{"status":"success","data":{"resultType":"matrix","result":[]}}`, nil
			}
		}}
		settings := syntheticSettings(source)
		if kind != "no-services" {
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "synthetic-metrics-a", Roles: []string{"services"}}, HostSettings{Name: "synthetic-metrics-b", Roles: []string{"services"}})
		}
		alerts, err := connectionLiveFailuresSignalForTest(t).Run(ctx, settings)
		cancel()
		if err == nil || len(alerts) != 0 {
			t.Fatalf("%s did not remain an error", kind)
		}
		if kind == "canceled" && (!errors.Is(err, context.Canceled) || calls != 1) {
			t.Fatal("cancellation retried or lost its identity")
		}
		if kind == "no-services" && calls != 0 {
			t.Fatal("missing inventory caused a query")
		}
	}
}

// Entirely absent counters retain the separate reconcile signal's policy.
func TestConnectionLiveFailuresSignalAbsentCountersKeepDelegation(t *testing.T) {
	if alerts := runConnectionLiveFailuresFixture(t, nil); len(alerts) != 0 {
		t.Fatal("empty frame changed absent-counter policy")
	}
}
