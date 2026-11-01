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
		"count": 1000, "requests": 1000, "canceled": 800, "seconds": 12000, "discarded": 0, "discarded_seconds": 0, "discarded_canceled": 0,
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
		"urnetwork_connect_control_http_discarded_seconds_count", "urnetwork_connect_control_http_discarded_seconds_sum",
		"urnetwork_connect_control_http_discarded_canceled_requests_total",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("query missing %q", required)
		}
	}
	if strings.Contains(query, `status="none"`) || strings.Contains(query, "quantile") || strings.Contains(query, "provider_id") {
		t.Fatalf("query narrows cancellation or invents identity/quantile: %s", query)
	}
}

// A request the api answers from its body alone (an audit-only pack,
// controller/connect_control_discard.go) spends no handler time. Discards
// leave both denominators, so they cannot dilute a hot route's mean or
// canceled share below the thresholds, and the thresholds stay as they are.
func TestControlRoutePressureSubtractsDiscards(t *testing.T) {
	now := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	process := controlRouteFixture(now, "synthetic-current")
	// 1,000 requests that do work at a 6 s mean with 25% canceled, and 333
	// discards: a quarter of all completions
	for name, value := range map[string]float64{
		"count": 1333, "requests": 1333, "discarded": 333, "seconds": 6000, "canceled": 250,
	} {
		process.values[name] = value
	}
	alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process))
	alert := requireAlertClass(t, alerts, "control-route-pressure")
	if len(alerts) != 1 || alert.Severity != Severity(tierWarn) || alert.Sustain != 1 {
		t.Fatalf("discards diluted the route below its thresholds: %+v", alerts)
	}
	for _, want := range []string{"requests_5m=1000", "discarded_5m=333", "canceled_share=25.0%", "mean_seconds=6.000"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing %q: %s", want, alert.Markdown())
		}
	}

	// fewer than 100 requests that do work is not traffic-bearing, however
	// many were discarded, and a discard count ahead of a scrape boundary
	// neither alerts nor makes the slot unobservable
	for _, values := range []map[string]float64{
		{"count": 5099, "requests": 5099, "discarded": 5000, "seconds": 1980, "canceled": 99},
		{"count": 50, "requests": 50, "discarded": 60, "seconds": 1000, "canceled": 40},
	} {
		quiet := controlRouteFixture(now, "synthetic-current")
		for name, value := range values {
			quiet.values[name] = value
		}
		if alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, quiet)); len(alerts) != 0 {
			t.Fatalf("discards made a quiet process alert: %+v", alerts)
		}
	}

	// the cells are required like every other: a process without one is
	// incomplete, not one with no discards
	for _, cell := range []string{"discarded", "discarded_seconds", "discarded_canceled"} {
		missing := controlRouteFixture(now, "synthetic-current")
		delete(missing.values, cell)
		alerts = runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, missing))
		requireAlertClass(t, alerts, "control-route-pressure-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("a process without its %s cell became evidence: %+v", cell, alerts)
		}
	}
}

// No alert carries a share or mean that is not a number.
func requireControlRouteFiniteAlerts(t *testing.T, alerts Alerts) {
	t.Helper()
	for _, alert := range alerts {
		for _, bad := range []string{"NaN", "Inf"} {
			if strings.Contains(alert.Markdown(), bad) {
				t.Fatalf("an alert carries %s: %s", bad, alert.Markdown())
			}
		}
	}
}

// When discards make up nearly every request, the request denominator can
// reach zero while the duration denominator, 3% larger within the scrape
// tolerance, keeps more than 100: the canceled share was 0/0, a NaN WARN, or
// 200/0, a +Inf PAGE (review of CPUOPT3 D3). Both adjusted denominators must
// reach 100, so such a process is not traffic-bearing.
func TestControlRoutePressureDiscardsCannotExhaustADenominator(t *testing.T) {
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	for _, canceled := range []float64{0, 200} {
		process := controlRouteFixture(now, "synthetic-current")
		for name, value := range map[string]float64{
			"count": 10000, "requests": 9700, "discarded": 9700, "canceled": canceled, "seconds": 100000,
		} {
			process.values[name] = value
		}
		alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process))
		requireControlRouteFiniteAlerts(t, alerts)
		if len(alerts) != 0 {
			t.Fatalf("canceled=%.0f: a process of discards alerted: %+v", canceled, alerts)
		}
	}
}

// The discarded requests' own seconds leave the mean with their count: 300
// requests that do work at 2 s each, half canceled, beside 1,000 discards
// that took 3 s each, are healthy, not a 12 s PAGE.
func TestControlRoutePressureExcludesDiscardedSeconds(t *testing.T) {
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	process := controlRouteFixture(now, "synthetic-current")
	for name, value := range map[string]float64{
		"count": 1300, "requests": 1300, "discarded": 1000, "discarded_seconds": 3000, "seconds": 3600, "canceled": 150,
	} {
		process.values[name] = value
	}
	if alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process)); len(alerts) != 0 {
		t.Fatalf("the discards' seconds raised the mean: %+v", alerts)
	}
	// the same requests slow in their own right still page
	process.values["seconds"] = 3000 + 300*12
	alert := requireAlertClass(t, runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process)), "control-route-pressure")
	if alert.Severity != Severity(tierPage) || !strings.Contains(alert.Markdown(), "mean_seconds=12.000") {
		t.Fatalf("slow requests beside discards = %s", alert.Markdown())
	}
}

// A canceled count ahead of its requests at a scrape boundary reads as every
// request canceled, never more.
func TestControlRoutePressureClampsTheCanceledShare(t *testing.T) {
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	process := controlRouteFixture(now, "synthetic-current")
	for name, value := range map[string]float64{
		"count": 1120, "requests": 1120, "discarded": 1000, "seconds": 120 * 12, "canceled": 150,
	} {
		process.values[name] = value
	}
	alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process))
	requireControlRouteFiniteAlerts(t, alerts)
	alert := requireAlertClass(t, alerts, "control-route-pressure")
	if !strings.Contains(alert.Markdown(), "canceled_share=100.0%") {
		t.Fatalf("share not clamped: %s", alert.Markdown())
	}
}

// Canceled audit-only requests leave the numerator with their denominator.
// Otherwise 333 canceled discards beside 1,000 successful 6 s requests WARN
// at 33.3%, even though no request that did work was canceled.
func TestControlRoutePressureExcludesDiscardedCancellations(t *testing.T) {
	completed := 0
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		name                        string
		canceled, discardedCanceled float64
		mean                        float64
		wantTier                    string
		wantCanceled, wantShare     string
	}{
		{name: "only audits canceled", canceled: 333, discardedCanceled: 333, mean: 6},
		{name: "only audits canceled on slow route", canceled: 600, discardedCanceled: 600, mean: 12},
		{name: "below warn", canceled: 333 + 199, discardedCanceled: 333, mean: 6},
		{name: "scrape skew", canceled: 333, discardedCanceled: 334, mean: 12},
		{name: "real warn", canceled: 333 + 200, discardedCanceled: 333, mean: 6, wantTier: tierWarn, wantCanceled: "200", wantShare: "20.0%"},
		{name: "below page", canceled: 333 + 499, discardedCanceled: 333, mean: 12, wantTier: tierWarn, wantCanceled: "499", wantShare: "49.9%"},
		{name: "real page", canceled: 333 + 500, discardedCanceled: 333, mean: 12, wantTier: tierPage, wantCanceled: "500", wantShare: "50.0%"},
		{name: "completed audits preserve cancellations", canceled: 500, mean: 12, wantTier: tierPage, wantCanceled: "500", wantShare: "50.0%"},
	} {
		func() {
			process := controlRouteFixture(now, "synthetic-current")
			discarded := float64(333)
			if discarded < fixture.discardedCanceled {
				discarded = fixture.discardedCanceled
			}
			for name, value := range map[string]float64{
				"count": 1000 + discarded, "requests": 1000 + discarded,
				"discarded": discarded, "discarded_seconds": discarded * 0.0001,
				"discarded_canceled": fixture.discardedCanceled, "canceled": fixture.canceled,
				"seconds": 1000*fixture.mean + discarded*0.0001,
			} {
				process.values[name] = value
			}
			// Exercise the arithmetic independently of parsing the new cell,
			// so the regression fails on the original false-pressure finding.
			findings := evaluateControlRoutePressure([]*controlRouteProcess{process}, map[string]bool{process.host + "\x00" + process.block: true}, now)
			if fixture.wantTier == "" && len(findings) != 0 {
				t.Errorf("%s: discarded cancellations produced findings: %+v", fixture.name, findings)
				return
			}
			alerts := runControlRouteFixture(t, now, controlRouteFixtureJson(t, now, process))
			requireControlRouteFiniteAlerts(t, alerts)
			if fixture.wantTier == "" {
				if len(alerts) != 0 {
					t.Errorf("%s: discarded cancellations produced alerts: %+v", fixture.name, alerts)
					return
				}
				return
			}
			alert := requireAlertClass(t, alerts, "control-route-pressure")
			if len(alerts) != 1 || alert.Severity != Severity(fixture.wantTier) {
				t.Errorf("%s: ordinary cancellations changed tier: %+v", fixture.name, alerts)
				return
			}
			for _, want := range []string{
				"requests_5m=1000", " canceled_5m=" + fixture.wantCanceled + " ",
				"canceled_share=" + fixture.wantShare, fmt.Sprintf("mean_seconds=%.3f", fixture.mean),
				fmt.Sprintf("discarded_canceled_5m=%.0f", fixture.discardedCanceled),
			} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Errorf("%s: missing %q: %s", fixture.name, want, alert.Markdown())
					return
				}
			}
		}()
		completed++
	}
	t.Logf("discarded cancellation arithmetic cases complete: count=%d", completed)
}
