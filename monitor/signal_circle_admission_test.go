package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type circleAdmissionFixture struct {
	host       string
	block      string
	instance   string
	start      time.Time
	age        time.Duration
	admissions float64
	deferrals  float64
	errors     float64
	waitCount  float64
	waitSum    float64
	omit       map[string]bool
}

func circleAdmissionFixtureJSON(
	t testing.TB,
	now time.Time,
	processes ...circleAdmissionFixture,
) string {
	t.Helper()
	result := []any{}
	for _, process := range processes {
		labels := map[string]string{
			"env": "synthetic", "job": "taskworker", "host": process.host,
			"block": process.block, "instance": process.instance,
		}
		observedAt := now.Add(-process.age)
		add := func(metric string, value float64) {
			if process.omit[metric] {
				return
			}
			metricLabels := map[string]string{"monitor_metric": metric}
			for key, label := range labels {
				metricLabels[key] = label
			}
			result = append(result, map[string]any{
				"metric": metricLabels,
				"value":  []any{float64(observedAt.Unix()), fmt.Sprintf("%.9f", value)},
			})
		}
		add("process_start_time_seconds", float64(process.start.Unix()))
		add("urnetwork_circle_transfer_admissions_total", process.admissions)
		add("urnetwork_circle_transfer_deferrals_total", process.deferrals)
		add("urnetwork_circle_transfer_admission_errors_total", process.errors)
		add("urnetwork_circle_transfer_admission_wait_seconds_count", process.waitCount)
		add("urnetwork_circle_transfer_admission_wait_seconds_sum", process.waitSum)
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

func runCircleAdmissionFixture(t testing.TB, now time.Time, payload string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			"process_start_time_seconds",
			"urnetwork_circle_transfer_admissions_total",
			"urnetwork_circle_transfer_deferrals_total",
			"urnetwork_circle_transfer_admission_errors_total",
			"urnetwork_circle_transfer_admission_wait_seconds_count",
			"urnetwork_circle_transfer_admission_wait_seconds_sum",
			"increase%28",
			"%5B5m%5D",
			"timestamp%28",
			"monitor_metric",
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("Circle admission Mimir command omitted %q: %s", want, command)
			}
		}
		if host.Name != "metrics-1" || !strings.Contains(command, "%22synthetic%22") {
			return "", fmt.Errorf("unexpected Mimir command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	alerts, err := NewCircleAdmissionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func TestCircleAdmissionQueryRequiresFreshCurrentSamples(t *testing.T) {
	query := circleAdmissionQuery("synthetic")
	selector := `{env="synthetic",job="taskworker"}`
	for _, metric := range circleAdmissionMetricNames {
		want := fmt.Sprintf(
			`timestamp(%s%s) >= time() - %d`,
			metric,
			selector,
			int64(circleAdmissionFreshness/time.Second),
		)
		if !strings.Contains(query, want) {
			t.Fatalf("Circle admission query does not require a fresh %s sample:\n%s", metric, query)
		}
	}
}

func TestCircleAdmissionSignalSyntheticHealthyCurrentFleet(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	old := circleAdmissionFixture{
		host: "edge-0", block: "g1", instance: "old", start: now.Add(-2 * time.Hour),
		omit: map[string]bool{"urnetwork_circle_transfer_admissions_total": true},
	}
	current := circleAdmissionFixture{
		host: "edge-0", block: "g1", instance: "current", start: now.Add(-time.Hour),
		admissions: 8, deferrals: 2, waitCount: 8, waitSum: 1.5,
	}
	peer := circleAdmissionFixture{
		host: "edge-1", block: "g2", instance: "peer", start: now.Add(-time.Hour),
		admissions: 4, waitCount: 4,
	}

	alerts := runCircleAdmissionFixture(t, now, circleAdmissionFixtureJSON(t, now, old, current, peer))
	if len(alerts) != 0 {
		t.Fatalf("healthy current Circle admission fleet alerted: %+v", alerts)
	}
}

func TestCircleAdmissionSignalSyntheticMissingCollector(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 1, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "edge-3", block: "g2", instance: "missing", start: now.Add(-time.Hour),
		omit: map[string]bool{
			"urnetwork_circle_transfer_deferrals_total":              true,
			"urnetwork_circle_transfer_admission_wait_seconds_sum":   true,
			"urnetwork_circle_transfer_admission_wait_seconds_count": false,
			"urnetwork_circle_transfer_admission_errors_total":       false,
			"urnetwork_circle_transfer_admissions_total":             false,
		},
	}

	alert := requireAlertClass(
		t,
		runCircleAdmissionFixture(t, now, circleAdmissionFixtureJSON(t, now, process)),
		"circle-transfer-admission-unobservable",
	)
	if alert.SignalNumber != "2.14" || alert.SignalKey != "circle-admission" ||
		alert.SignalID != "task/circle-transfer-admission" {
		t.Fatalf("wrong Circle admission signal identity: %+v", alert)
	}
	for _, want := range []string{
		"edge-3/g2#missing[deferrals,wait-sum]",
		"at most three transfer submits",
		"commit 66525afc",
		"mutable version string",
		"SIGNALS.md §2.14",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing-collector alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "b8718420") {
		t.Fatalf("missing-collector alert retained former non-ancestor deployment guidance:\n%s", alert.Markdown())
	}
}

func TestCircleAdmissionSignalSyntheticFailClosedAndPressure(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 2, 0, 0, time.UTC)
	first := circleAdmissionFixture{
		host: "edge-0", block: "g1", instance: "first", start: now.Add(-time.Hour),
		admissions: 4, deferrals: 16, errors: 2, waitCount: 4, waitSum: 40,
	}
	second := circleAdmissionFixture{
		host: "edge-1", block: "g2", instance: "second", start: now.Add(-time.Hour),
		admissions: 6, deferrals: 2, waitCount: 6, waitSum: 0,
	}
	alerts := runCircleAdmissionFixture(t, now, circleAdmissionFixtureJSON(t, now, first, second))

	errorAlert := requireAlertClass(t, alerts, "circle-transfer-admission-error")
	for _, want := range []string{
		"failed closed 2 time(s)",
		"without contacting Circle",
		"admission_errors_5m=2.000",
		"stable",
		"do not manually replay",
	} {
		if !strings.Contains(errorAlert.Markdown(), want) {
			t.Fatalf("fail-closed alert lacks %q:\n%s", want, errorAlert.Markdown())
		}
	}

	pressureAlert := requireAlertClass(t, alerts, "circle-transfer-admission-pressure")
	for _, want := range []string{
		"4.000s on average fleet-wide",
		"10.000s on edge-0/g1#first",
		"three-per-rolling-second safety envelope",
		"two-minute execution budget",
		"software cannot create liquidity",
		"authoritative Circle quota",
	} {
		if !strings.Contains(pressureAlert.Markdown(), want) {
			t.Fatalf("pressure alert lacks %q:\n%s", want, pressureAlert.Markdown())
		}
	}
}

func TestCircleAdmissionSignalSyntheticRejectsInvalidMetric(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 3, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "edge-4", block: "g1", instance: "invalid", start: now.Add(-time.Hour),
		admissions: -1, waitCount: 1,
	}
	payload := circleAdmissionFixtureJSON(t, now, process)
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	if _, err := NewCircleAdmissionSignal().Run(context.Background(), settings); err == nil ||
		!strings.Contains(err.Error(), "invalid urnetwork_circle_transfer_admissions_total value -1") {
		t.Fatalf("invalid metric error = %v", err)
	}
}
