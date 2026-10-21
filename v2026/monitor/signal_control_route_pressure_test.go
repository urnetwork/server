package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Complete source-owned control fixture; individual tests remove or age exact
// cells rather than treating missing telemetry as numerical zero.
func controlRouteFixture(now time.Time, instance string) *controlRouteProcess {
	values := map[string]float64{
		"start": float64(now.Add(-time.Hour).Unix()),
		"count": 1000, "requests": 1000, "canceled": 800, "seconds": 12000,
		"inflight": 500, "inflight_min": 200, "samples": 10, "early_samples": 2,
		"seconds_samples": 10, "seconds_early_samples": 2, "inflight_samples": 10, "inflight_early_samples": 2,
	}
	for _, name := range []string{"start_time", "count_time", "seconds_time", "inflight_time", "requests_time"} {
		values[name] = float64(now.Add(-15 * time.Second).Unix())
	}
	for _, phase := range controlRoutePhases {
		values["phase:"+phase] = 0
	}
	values["phase:authenticate"] = 400
	values["phase:controller"] = 100
	for _, message := range controlRouteMessages {
		values["frame:"+message] = 0
	}
	values["frame:provide"] = 100
	return &controlRouteProcess{host: "api-a.invalid", block: "lane-a", instance: instance, values: values}
}

// Match the bounded PromQL response shape without real hosts or credentials.
func controlRouteFixtureJson(t testing.TB, now time.Time, processes ...*controlRouteProcess) string {
	t.Helper()
	rows := []any{}
	for _, process := range processes {
		for name, value := range process.values {
			labels := map[string]string{
				"env": "synthetic", "job": "api", "host": process.host,
				"block": process.block, "instance": process.instance,
			}
			parts := strings.SplitN(name, ":", 2)
			labels["monitor_metric"] = parts[0]
			if len(parts) > 1 {
				if parts[0] == "phase" {
					labels["phase"] = parts[1]
				} else {
					labels["message"] = parts[1]
					labels["ingress"] = "http"
				}
			}
			rows = append(rows, map[string]any{"metric": labels, "value": []any{float64(now.Unix()), fmt.Sprint(value)}})
		}
	}
	encoded, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The public signal boundary exercises inventory, transport, parsing and alert
// severity; the source is entirely synthetic and never queries PostgreSQL.
func runControlRouteFixture(t testing.TB, now time.Time, payload string, blocks ...string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" || !strings.Contains(command, "--max-filesize 2097152") || !strings.Contains(command, "--max-time 15") {
			t.Fatalf("unbounded/unconfigured query: %s", command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	settings.LogServices = []string{"api"}
	settings.LogServiceHosts = map[string][]string{"api": {"api-a.invalid"}}
	if len(blocks) == 0 {
		blocks = []string{"lane-a"}
	}
	settings.LogServiceBlocks = map[string][]string{"api": blocks}
	alerts, err := NewControlRoutePressureSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func TestControlRoutePressureDetectsSustainedHotRoute(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	process := controlRouteFixture(now, "synthetic-current")
	alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process))
	alert := requireAlertClass(t, alerts, "control-route-pressure")
	if len(alerts) != 1 || alert.Severity != Severity(tierPage) || alert.Sustain != 2 {
		t.Fatalf("pressure alert = %+v", alerts)
	}
	for _, want := range []string{"80.0%", "mean_seconds=12.000", "phase_authenticate_inflight=400", "frame_provide_inflight=100", "not an observed HTTP 499", "not evidence that the provider exit is dark", "Phases do not isolate PG pool wait"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing discriminator %q: %s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), process.instance) {
		t.Fatal("private instance join leaked into alert")
	}
}

func TestControlRoutePressureQuietAndHealthyControls(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	for _, values := range []map[string]float64{
		{"count": 0, "requests": 0, "canceled": 0, "seconds": 0, "inflight": 0, "inflight_min": 0},
		{"count": 1000, "requests": 1000, "canceled": 8, "seconds": 2200},
		{"count": 1000, "requests": 1000, "canceled": 800, "seconds": 2000},
		{"count": 1000, "requests": 1000, "canceled": 10, "seconds": 15000},
		{"count": 99, "requests": 99, "canceled": 99, "seconds": 1485},
	} {
		process := controlRouteFixture(now, "synthetic-current")
		for name, value := range values {
			process.values[name] = value
		}
		if alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process)); len(alerts) != 0 {
			t.Fatalf("quiet/healthy control paged: %+v", alerts)
		}
	}
}

func TestControlRoutePressurePageRequiresSustainedInflight(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	process := controlRouteFixture(now, "synthetic-current")
	process.values["inflight_min"] = 99
	alert := requireAlertClass(t, runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process)), "control-route-pressure")
	if alert.Severity != Severity(tierWarn) || alert.Sustain != 1 {
		t.Fatalf("brief inflight spike paged: %+v", alert)
	}
	process.values["inflight_min"] = 100
	process.values["canceled"] = 200
	process.values["seconds"] = 5000
	alert = requireAlertClass(t, runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process)), "control-route-pressure")
	if alert.Severity != Severity(tierWarn) {
		t.Fatalf("WARN boundary not retained: %+v", alert)
	}
}

func TestControlRoutePressureNeverBorrowsDrainingGeneration(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	old := controlRouteFixture(now, "synthetic-old")
	current := controlRouteFixture(now, "synthetic-new")
	current.values["start"] = float64(now.Add(-10 * time.Minute).Unix())
	current.values["canceled"] = 0
	current.values["seconds"] = 2000
	if alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, old, current)); len(alerts) != 0 {
		t.Fatalf("old pressure leaked into current source: %+v", alerts)
	}
	delete(current.values, "count")
	alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, old, current))
	requireAlertClass(t, alerts, "control-route-pressure-unobservable")
	if len(alerts) != 1 {
		t.Fatalf("draining source replaced missing current evidence: %+v", alerts)
	}
	delete(current.values, "start")
	alerts = runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, old, current))
	if len(alerts) != 1 {
		t.Fatalf("unknown start fell back to draining source: %+v", alerts)
	}
	requireAlertClass(t, alerts, "control-route-pressure-unobservable")
}

func TestControlRoutePressureGapsDoNotEraseProvenHotSlot(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	process := controlRouteFixture(now, "synthetic-current")
	delete(process.values, "phase:authenticate")
	alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process), "lane-a", "lane-b")
	requireAlertClass(t, alerts, "control-route-pressure")
	gap := requireAlertClass(t, alerts, "control-route-pressure-unobservable")
	if len(alerts) != 2 || !strings.Contains(gap.Markdown(), "lane-b") || !strings.Contains(gap.Markdown(), "phase/frame telemetry unavailable") {
		t.Fatalf("coverage gap lost: %+v", alerts)
	}
}

func TestControlRoutePressureRejectsStaleYoungAndPartialRanges(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	for _, change := range []struct {
		name  string
		value float64
	}{
		{"count_time", float64(now.Add(-91 * time.Second).Unix())},
		{"start", float64(now.Add(-299 * time.Second).Unix())},
		{"samples", 4}, {"early_samples", 0}, {"requests", 1500}, {"canceled", 1001},
		{"inflight_samples", 1}, {"inflight_early_samples", 0}, {"seconds_samples", 1}, {"seconds_early_samples", 0},
	} {
		process := controlRouteFixture(now, "synthetic-current")
		process.values[change.name] = change.value
		alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process))
		requireAlertClass(t, alerts, "control-route-pressure-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("invalid %s source became pressure: %+v", change.name, alerts)
		}
	}
}

func TestControlRoutePressureMalformedAndAbsentSourceIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)
	process := controlRouteFixture(now, "synthetic-current")
	valid := controlRouteFixtureJson(t, now, process)
	for _, payload := range []string{
		"synthetic-invalid", controlRouteFixtureJson(t, now),
		strings.Replace(valid, `"status":"success"`, `"warnings":["synthetic warning"],"status":"success"`, 1),
		strings.ReplaceAll(valid, `"env":"synthetic"`, `"env":"foreign"`),
		controlRouteFixtureJson(t, now, process, process),
	} {
		alerts := runControlRouteFixture(t, now, payload)
		requireAlertClass(t, alerts, "control-route-pressure-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("bad source became healthy/pressure: %+v", alerts)
		}
	}
}

func TestControlRoutePressureQueryUsesBoundedFixedRouteAndHistory(t *testing.T) {
	query := controlRoutePressureQuery("synthetic")
	for _, required := range []string{
		`route="POST ^/connect/control$"`, `job="api"`, `outcome="canceled"`,
		"increase(", "[5m]", "count_over_time(", "[1m] offset 4m", "min_over_time(",
		"timestamp(", "process_start_time_seconds", `ingress="http"`,
		"urnetwork_connect_control_http_phase_inflight", "urnetwork_connect_control_frames_inflight",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("query missing %q", required)
		}
	}
	if strings.Contains(query, `status="none"`) || strings.Contains(query, "quantile") || strings.Contains(query, "provider_id") {
		t.Fatalf("query narrows cancellation or invents identity/quantile: %s", query)
	}
}
