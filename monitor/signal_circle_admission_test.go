package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type circleAdmissionFixture struct {
	host         string
	block        string
	instance     string
	start        time.Time
	age          time.Duration
	admissions   float64
	deferrals    float64
	errors       float64
	waitCount    float64
	waitSum      float64
	observable   float64
	omit         map[string]bool
	rangeSamples map[string]float64
	omitDelta    map[string]bool
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
		addRange := func(metric string, value float64) {
			if process.omit[metric] {
				return
			}
			if !process.omitDelta[metric] {
				add(metric, value)
			}
			samples := process.rangeSamples[metric]
			if samples == 0 {
				samples = 2
			}
			add(metric+circleAdmissionSamplesSuffix, samples)
		}
		addRange("urnetwork_circle_transfer_admissions_total", process.admissions)
		addRange("urnetwork_circle_transfer_deferrals_total", process.deferrals)
		addRange("urnetwork_circle_transfer_admission_errors_total", process.errors)
		addRange("urnetwork_circle_transfer_admission_wait_seconds_count", process.waitCount)
		addRange("urnetwork_circle_transfer_admission_wait_seconds_sum", process.waitSum)
		observable := process.observable
		if observable == 0 {
			observable = 1
		}
		add(circleAdmissionObservableMetricName, observable)
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
			circleAdmissionObservableMetricName,
			"increase%28",
			"count_over_time%28",
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

func TestCircleAdmissionSignalSeparatesCollectorPresenceFromRangeCoverage(t *testing.T) {
	now := time.Date(2026, 9, 12, 5, 19, 33, 0, time.UTC)
	oneSample := map[string]float64{}
	omitDelta := map[string]bool{}
	for _, metric := range circleAdmissionDeltaMetricNames {
		oneSample[metric] = 1
		omitDelta[metric] = true
	}
	process := circleAdmissionFixture{
		host: "worker-a.invalid", block: "generation-a", instance: "generated-instance", start: now.Add(-time.Hour),
		rangeSamples: oneSample, omitDelta: omitDelta,
	}

	alert := requireAlertClass(
		t,
		runCircleAdmissionFixture(t, now, circleAdmissionFixtureJSON(t, now, process)),
		"circle-transfer-admission-unobservable",
	)
	for _, want := range []string{
		"insufficient_range=worker-a.invalid/generation-a#generated-instance[admissions=1,deferrals=1,admission-errors=1,wait-count=1,wait-sum=1]",
		"collectors are registered",
		"telemetry admission or delivery loss",
		"Do not deploy the Taskworker merely because increase() had insufficient range samples",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("range-coverage alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "Deploy a Taskworker artifact") {
		t.Fatalf("range-coverage alert prescribed an unproved Taskworker deploy:\n%s", alert.Markdown())
	}
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
	if strings.Contains(query, "increase("+circleAdmissionObservableMetricName) ||
		strings.Contains(query, "count_over_time("+circleAdmissionObservableMetricName) {
		t.Fatalf("Circle admission query treated the fixed capability as an activity counter:\n%s", query)
	}
}

func TestCircleAdmissionSignalTreatsMixedObservableRolloutAsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 12, 5, 20, 0, 0, time.UTC)
	current := circleAdmissionFixture{
		host: "worker-a.invalid", block: "generation-a", instance: "current-a", start: now.Add(-time.Hour),
	}
	missing := circleAdmissionFixture{
		host: "worker-b.invalid", block: "generation-b", instance: "current-b", start: now.Add(-time.Hour),
		omit: map[string]bool{circleAdmissionObservableMetricName: true},
	}

	alert := requireAlertClass(
		t,
		runCircleAdmissionFixture(t, now, circleAdmissionFixtureJSON(t, now, current, missing)),
		"circle-transfer-admission-unobservable",
	)
	for _, want := range []string{
		"worker-b.invalid/generation-b#current-b[admission-observable]",
		"mixed rollout",
		"absence is unknown",
		"must never be rendered as zero admitted submissions",
		"same executable that emits one identifier-free marker",
		"marker-capable commit 928abfca",
		"fail-closed baseline 66525afc can expose all five activity families",
		"predate 928abfca",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("mixed-rollout alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "admissions=0") {
		t.Fatalf("mixed-rollout alert treated missing telemetry as zero:\n%s", alert.Markdown())
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

func TestCircleAdmissionSignalTreatsMissingSamplesAsAmbiguous(t *testing.T) {
	now := time.Date(2026, 9, 1, 8, 1, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "worker-c.invalid", block: "generation-b", instance: "no-samples", start: now.Add(-time.Hour),
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
		"no_range_samples=worker-c.invalid/generation-b#no-samples[deferrals,wait-sum]",
		"cannot distinguish an absent collector from stats delivery or admission loss",
		"Only §8.12 source and immutable artifact evidence can prove",
		"at most three transfer submits",
		"commit 14928f69",
		"Commit 66525afc",
		"commit 928abfca",
		"66525afc is only the fail-closed activity/error baseline",
		"does not prove the capability gauge or exact pre-POST marker",
		"mutable version string",
		"SIGNALS.md §2.14",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing-sample alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "genuinely lacks") {
		t.Fatalf("missing-sample alert overclaimed collector absence:\n%s", alert.Markdown())
	}
	if strings.Contains(alert.Markdown(), "b8718420") || strings.Contains(alert.Markdown(), "eb7e79b6") {
		t.Fatalf("missing-sample alert retained former non-ancestor deployment guidance:\n%s", alert.Markdown())
	}
}

func TestCircleAdmissionCatalogDoesNotTreatFailClosedArtifactAsMarkerCapable(t *testing.T) {
	if circleAdmissionFailClosedBaselineCommit != "66525afc" || circleAdmissionMarkerBaselineCommit != "928abfca" {
		t.Fatalf(
			"Circle admission ancestry boundaries = fail-closed %q marker %q",
			circleAdmissionFailClosedBaselineCommit,
			circleAdmissionMarkerBaselineCommit,
		)
	}
	catalog, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(catalog)
	start := strings.Index(text, "### 2.14 Circle transfer admission")
	if start < 0 {
		t.Fatal("SIGNALS.md §2.14 boundary is missing")
	}
	end := strings.Index(text[start:], "### 2.15 ")
	if end < 0 {
		t.Fatal("SIGNALS.md §2.15 boundary is missing")
	}
	section := text[start : start+end]
	normalizedSection := strings.Join(strings.Fields(section), " ")
	for _, want := range []string{
		"`66525afc` is the fail-closed activity/error baseline",
		"does **not** prove the capability gauge or exact pre-POST marker",
		"`928abfca` is the surviving current-main marker-capable baseline",
		"`1b9cacba` contains `66525afc` but predates `928abfca`",
	} {
		if !strings.Contains(normalizedSection, want) {
			t.Fatalf("SIGNALS.md §2.14 lacks %q", want)
		}
	}
	if strings.Contains(section, "66525afc` also converts the Redis wrapper's\npre-command connection panic into the same error/counter/log path; use that\ndescendant as the minimum observable deployment baseline") {
		t.Fatal("SIGNALS.md §2.14 still treats the fail-closed-only ancestry as marker-capable")
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

func TestCircleAdmissionSignalRejectsInvalidObservableCapability(t *testing.T) {
	now := time.Date(2026, 9, 12, 5, 21, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "worker-c.invalid", block: "generation-c", instance: "invalid-observable",
		start: now.Add(-time.Hour), observable: 2,
	}
	payload := circleAdmissionFixtureJSON(t, now, process)
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return payload, nil }}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	if _, err := NewCircleAdmissionSignal().Run(context.Background(), settings); err == nil ||
		!strings.Contains(err.Error(), "invalid urnetwork_circle_transfer_admission_observable_info value 2, want 1") {
		t.Fatalf("invalid observable error = %v", err)
	}
}
