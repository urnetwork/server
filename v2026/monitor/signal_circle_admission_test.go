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
	for _, metric := range circleAdmissionDeltaMetricNames {
		if !strings.Contains(query, "increase("+metric+selector+"[5m])") {
			t.Fatalf("Circle admission query lost reset-aware per-instance increase for %s", metric)
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
	alerts := runCircleAdmissionFixture(t, now, circleAdmissionFixtureJSON(t, now, process))
	alert := requireAlertClass(t, alerts, "circle-transfer-admission-unobservable")
	if len(alerts) != 1 || !strings.Contains(alert.Markdown(), "urnetwork_circle_transfer_admissions_total:invalid value") {
		t.Fatalf("invalid metric must remain unobservable: %+v", alerts)
	}
}

func TestCircleAdmissionSignalRejectsInvalidObservableCapability(t *testing.T) {
	now := time.Date(2026, 9, 12, 5, 21, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "worker-c.invalid", block: "generation-c", instance: "invalid-observable",
		start: now.Add(-time.Hour), observable: 2,
	}
	alerts := runCircleAdmissionFixture(t, now, circleAdmissionFixtureJSON(t, now, process))
	alert := requireAlertClass(t, alerts, "circle-transfer-admission-unobservable")
	if len(alerts) != 1 || !strings.Contains(alert.Markdown(), "capability is not one") {
		t.Fatalf("invalid capability must remain unobservable: %+v", alerts)
	}
}

// Change the real Mimir envelope while retaining the public adapter path.
func editCircleAdmissionFixtureJson(t testing.TB, payload string, edit func(*mimirInstantResponse)) string {
	t.Helper()
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		t.Fatal(err)
	}
	edit(&response)
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Exercise both row orders and compare complete rendered findings, not helpers.
func requireCircleAdmissionOrderInvariant(t *testing.T, now time.Time, payload string, classes ...string) Alerts {
	t.Helper()
	reversed := editCircleAdmissionFixtureJson(t, payload, func(response *mimirInstantResponse) {
		for left, right := 0, len(response.Data.Result)-1; left < right; left, right = left+1, right-1 {
			response.Data.Result[left], response.Data.Result[right] = response.Data.Result[right], response.Data.Result[left]
		}
	})
	first := runCircleAdmissionFixture(t, now, payload)
	second := runCircleAdmissionFixture(t, now, reversed)
	for _, alerts := range []Alerts{first, second} {
		if len(alerts) != len(classes) {
			t.Fatalf("Circle classes: got %+v, want %v", alerts, classes)
		}
		for _, class := range classes {
			requireAlertClass(t, alerts, class)
		}
	}
	for index := range first {
		if first[index].Markdown() != second[index].Markdown() {
			t.Fatalf("Circle result changed with series order:\n%s\n%s", first[index].Markdown(), second[index].Markdown())
		}
	}
	return first
}

// Duplicate exact runtime/error series cannot choose a diagnosis by row order.
func TestCircleAdmissionSignalDuplicateErrorAuthority(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour), errors: 1,
	}
	for _, value := range []string{"0", "1", "NaN"} {
		for _, extraLabel := range []bool{false, true} {
			payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, process), func(response *mimirInstantResponse) {
				for _, series := range response.Data.Result {
					if series.Metric["monitor_metric"] != "urnetwork_circle_transfer_admission_errors_total" {
						continue
					}
					duplicate := series
					duplicate.Metric = map[string]string{}
					for key, label := range series.Metric {
						duplicate.Metric[key] = label
					}
					if extraLabel {
						duplicate.Metric["replica"] = "synthetic-extra-label"
					}
					duplicate.Value = []json.RawMessage{series.Value[0], json.RawMessage(fmt.Sprintf("%q", value))}
					response.Data.Result = append(response.Data.Result, duplicate)
					break
				}
			})
			alerts := requireCircleAdmissionOrderInvariant(t, now, payload, "circle-transfer-admission-unobservable")
			if !strings.Contains(alerts[0].Markdown(), "urnetwork_circle_transfer_admission_errors_total:duplicate") ||
				strings.Contains(alerts[0].Markdown(), "synthetic-extra-label") {
				t.Fatalf("duplicate error authority or extra-label privacy lost: %s", alerts[0].Markdown())
			}
		}
	}
}

// Every scalar family, including capability, range count and start, has one owner.
func TestCircleAdmissionSignalDuplicateFamiliesAreUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 1, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour),
	}
	metricNames := append([]string{"process_start_time_seconds", circleAdmissionObservableMetricName}, circleAdmissionDeltaMetricNames...)
	for _, metricName := range circleAdmissionDeltaMetricNames {
		metricNames = append(metricNames, metricName+circleAdmissionSamplesSuffix)
	}
	for _, metricName := range metricNames {
		payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, process), func(response *mimirInstantResponse) {
			for _, series := range response.Data.Result {
				if series.Metric["monitor_metric"] == metricName {
					response.Data.Result = append(response.Data.Result, series)
					return
				}
			}
			t.Fatalf("fixture lacks %s", metricName)
		})
		requireCircleAdmissionOrderInvariant(t, now, payload, "circle-transfer-admission-unobservable")
	}
}

// Malformed observations stay scoped and cannot erase complete sibling failures.
func TestCircleAdmissionSignalMalformedFamilyPreservesSiblingFindings(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 2, 0, 0, time.UTC)
	broken := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "broken", start: now.Add(-time.Hour),
	}
	confirmed := circleAdmissionFixture{
		host: "worker-b.example", block: "synthetic", instance: "confirmed", start: now.Add(-time.Hour),
		errors: 2, waitCount: 1, waitSum: 6,
	}
	for _, metricName := range []string{"process_start_time_seconds", circleAdmissionObservableMetricName,
		"urnetwork_circle_transfer_admission_errors_total", "urnetwork_circle_transfer_admission_errors_total" + circleAdmissionSamplesSuffix,
		"urnetwork_circle_transfer_admission_wait_seconds_sum"} {
		for _, rawValue := range []string{`"NaN"`, `"+Inf"`, `"-Inf"`, `"-1"`, `"untrusted-value-canary"`, `null`} {
			payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, broken, confirmed), func(response *mimirInstantResponse) {
				for index := range response.Data.Result {
					series := &response.Data.Result[index]
					if series.Metric["instance"] == "broken" && series.Metric["monitor_metric"] == metricName {
						series.Value[1] = json.RawMessage(rawValue)
					}
				}
			})
			alerts := requireCircleAdmissionOrderInvariant(t, now, payload,
				"circle-transfer-admission-unobservable", "circle-transfer-admission-error", "circle-transfer-admission-pressure")
			errorAlert := requireAlertClass(t, alerts, "circle-transfer-admission-error")
			if !strings.Contains(errorAlert.Markdown(), "worker-b.example/synthetic#confirmed") ||
				!strings.Contains(errorAlert.Markdown(), "admission_errors_5m=2.000") {
				t.Fatalf("confirmed sibling error was erased: %s", errorAlert.Markdown())
			}
			for _, alert := range alerts {
				if strings.Contains(alert.Markdown(), "untrusted-value-canary") {
					t.Fatal("untrusted metric value escaped into alert detail")
				}
			}
		}
	}
}

// A failed family does not invalidate another complete signal on that runtime.
func TestCircleAdmissionSignalIndependentFamiliesSurvivePartialRuntime(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 3, 0, 0, time.UTC)
	for _, test := range []struct {
		metricName string
		classes    []string
	}{
		{metricName: "urnetwork_circle_transfer_admissions_total", classes: []string{"circle-transfer-admission-error", "circle-transfer-admission-pressure"}},
		{metricName: "urnetwork_circle_transfer_deferrals_total", classes: []string{"circle-transfer-admission-error", "circle-transfer-admission-pressure"}},
		{metricName: circleAdmissionObservableMetricName, classes: []string{"circle-transfer-admission-error", "circle-transfer-admission-pressure"}},
		{metricName: "urnetwork_circle_transfer_admission_errors_total", classes: []string{"circle-transfer-admission-pressure"}},
		{metricName: "urnetwork_circle_transfer_admission_wait_seconds_count", classes: []string{"circle-transfer-admission-error"}},
		{metricName: "urnetwork_circle_transfer_admission_wait_seconds_sum", classes: []string{"circle-transfer-admission-error"}},
	} {
		process := circleAdmissionFixture{
			host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour),
			errors: 1, waitCount: 1, waitSum: 6, omit: map[string]bool{test.metricName: true},
		}
		classes := append([]string{"circle-transfer-admission-unobservable"}, test.classes...)
		requireCircleAdmissionOrderInvariant(t, now, circleAdmissionFixtureJSON(t, now, process), classes...)
	}
}

// Presence is independent of increase: one, zero or fractional samples cannot authorize a delta.
func TestCircleAdmissionSignalRequiresTwoValidRangeSamples(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 4, 0, 0, time.UTC)
	metricName := "urnetwork_circle_transfer_admission_errors_total"
	for _, sampleCount := range []string{"0", "1", "1.5", "2", "3"} {
		process := circleAdmissionFixture{
			host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour), errors: 1,
		}
		payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, process), func(response *mimirInstantResponse) {
			for index := range response.Data.Result {
				series := &response.Data.Result[index]
				if series.Metric["monitor_metric"] == metricName+circleAdmissionSamplesSuffix {
					series.Value[1] = json.RawMessage(fmt.Sprintf("%q", sampleCount))
				}
			}
		})
		class := "circle-transfer-admission-unobservable"
		if sampleCount == "2" || sampleCount == "3" {
			class = "circle-transfer-admission-error"
		}
		requireCircleAdmissionOrderInvariant(t, now, payload, class)
	}
	for _, omitted := range []string{metricName, metricName + circleAdmissionSamplesSuffix} {
		process := circleAdmissionFixture{
			host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour), errors: 1,
			omit: map[string]bool{omitted: true},
		}
		requireCircleAdmissionOrderInvariant(t, now, circleAdmissionFixtureJSON(t, now, process), "circle-transfer-admission-unobservable")
	}
}

// A replacement's missing start or metric cannot borrow the old generation's authority.
func TestCircleAdmissionSignalNewestGenerationAuthority(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 5, 0, 0, time.UTC)
	old := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "old", start: now.Add(-2 * time.Hour), errors: 100, waitCount: 1, waitSum: 30,
	}
	current := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour),
	}
	requireCircleAdmissionOrderInvariant(t, now, circleAdmissionFixtureJSON(t, now, old, current))
	for _, metricName := range []string{"process_start_time_seconds", "urnetwork_circle_transfer_admission_errors_total"} {
		missing := current
		missing.omit = map[string]bool{metricName: true}
		requireCircleAdmissionOrderInvariant(t, now, circleAdmissionFixtureJSON(t, now, old, missing), "circle-transfer-admission-unobservable")
	}
	tied := current
	tied.start = old.start
	requireCircleAdmissionOrderInvariant(t, now, circleAdmissionFixtureJSON(t, now, old, tied), "circle-transfer-admission-unobservable")
	future := current
	future.start = now.Add(31 * time.Second)
	requireCircleAdmissionOrderInvariant(t, now, circleAdmissionFixtureJSON(t, now, old, future), "circle-transfer-admission-unobservable")
	// A duplicate activity family on a proven older runtime does not poison its replacement.
	payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, old, current), func(response *mimirInstantResponse) {
		for _, series := range response.Data.Result {
			if series.Metric["instance"] == "old" && series.Metric["monitor_metric"] == "urnetwork_circle_transfer_admission_errors_total" {
				response.Data.Result = append(response.Data.Result, series)
				return
			}
		}
	})
	requireCircleAdmissionOrderInvariant(t, now, payload)
}

// Zero activity is healthy only with complete authority; the pressure edge remains strict.
func TestCircleAdmissionSignalIdleAndWaitBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 6, 0, 0, time.UTC)
	for _, test := range []struct {
		waitCount float64
		waitSum   float64
		classes   []string
	}{
		{waitCount: 0, waitSum: 0},
		{waitCount: 1, waitSum: 5},
		{waitCount: 1, waitSum: 5.000001, classes: []string{"circle-transfer-admission-pressure"}},
		{waitCount: 0, waitSum: 1, classes: []string{"circle-transfer-admission-unobservable"}},
	} {
		process := circleAdmissionFixture{
			host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour),
			waitCount: test.waitCount, waitSum: test.waitSum, deferrals: 3,
		}
		requireCircleAdmissionOrderInvariant(t, now, circleAdmissionFixtureJSON(t, now, process), test.classes...)
	}
}

// Ambiguity in one runtime cannot discard complete failures from another cohort.
func TestCircleAdmissionSignalDuplicateRuntimePreservesSiblingFindings(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 7, 0, 0, time.UTC)
	broken := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "broken", start: now.Add(-time.Hour), errors: 100,
	}
	confirmed := circleAdmissionFixture{
		host: "worker-b.example", block: "synthetic", instance: "confirmed", start: now.Add(-time.Hour),
		errors: 2, waitCount: 1, waitSum: 6,
	}
	for _, metricName := range []string{"process_start_time_seconds", circleAdmissionObservableMetricName,
		"urnetwork_circle_transfer_admission_errors_total", "urnetwork_circle_transfer_admission_errors_total" + circleAdmissionSamplesSuffix,
		"urnetwork_circle_transfer_admission_wait_seconds_sum"} {
		payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, broken, confirmed), func(response *mimirInstantResponse) {
			for _, series := range response.Data.Result {
				if series.Metric["instance"] == "broken" && series.Metric["monitor_metric"] == metricName {
					response.Data.Result = append(response.Data.Result, series)
					return
				}
			}
		})
		alerts := requireCircleAdmissionOrderInvariant(t, now, payload,
			"circle-transfer-admission-unobservable", "circle-transfer-admission-error", "circle-transfer-admission-pressure")
		for _, class := range []string{"circle-transfer-admission-error", "circle-transfer-admission-pressure"} {
			alert := requireAlertClass(t, alerts, class)
			if !strings.Contains(alert.Markdown(), "worker-b.example/synthetic#confirmed") ||
				!strings.Contains(alert.Markdown(), "admission_errors_5m=2.000") {
				t.Fatalf("duplicate runtime discarded independent evidence: %s", alert.Markdown())
			}
		}
		if metricName == "urnetwork_circle_transfer_admission_errors_total" {
			alert := requireAlertClass(t, alerts, "circle-transfer-admission-error")
			if !strings.Contains(alert.Markdown(), "fleet error total is unknown") ||
				strings.Contains(alert.Markdown(), "admission_errors_5m=100.000") {
				t.Fatalf("duplicate value supplied error authority: %s", alert.Markdown())
			}
		}
	}
}

// Sample shape and exact freshness edges cannot silently supply a healthy zero.
func TestCircleAdmissionSignalSampleAuthorityBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 8, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour), errors: 1,
	}
	for _, test := range []struct {
		name      string
		timestamp string
		want      string
	}{
		{name: "freshness-edge", timestamp: fmt.Sprint(now.Add(-90 * time.Second).Unix()), want: "circle-transfer-admission-error"},
		{name: "stale", timestamp: fmt.Sprint(now.Add(-91 * time.Second).Unix()), want: "circle-transfer-admission-unobservable"},
		{name: "future-edge", timestamp: fmt.Sprint(now.Add(30 * time.Second).Unix()), want: "circle-transfer-admission-error"},
		{name: "future", timestamp: fmt.Sprint(now.Add(31 * time.Second).Unix()), want: "circle-transfer-admission-unobservable"},
		{name: "null", timestamp: "null", want: "circle-transfer-admission-unobservable"},
		{name: "wrong-type", timestamp: `"timestamp-canary"`, want: "circle-transfer-admission-unobservable"},
	} {
		payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, process), func(response *mimirInstantResponse) {
			for index := range response.Data.Result {
				series := &response.Data.Result[index]
				if series.Metric["monitor_metric"] == "urnetwork_circle_transfer_admission_errors_total" {
					series.Value[0] = json.RawMessage(test.timestamp)
				}
			}
		})
		alerts := requireCircleAdmissionOrderInvariant(t, now, payload, test.want)
		if strings.Contains(alerts[0].Markdown(), "timestamp-canary") {
			t.Fatalf("%s leaked a raw timestamp", test.name)
		}
	}
	for _, label := range []string{"env", "job", "host", "block", "instance"} {
		payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, process), func(response *mimirInstantResponse) {
			for index := range response.Data.Result {
				delete(response.Data.Result[index].Metric, label)
			}
		})
		requireCircleAdmissionOrderInvariant(t, now, payload, "circle-transfer-admission-unobservable")
	}
}

// Finite inputs can still overflow a ratio or aggregate; independent positives survive.
func TestCircleAdmissionSignalRejectsNonfiniteDerivedValues(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 9, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour),
	}
	payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, process), func(response *mimirInstantResponse) {
		for index := range response.Data.Result {
			series := &response.Data.Result[index]
			switch series.Metric["monitor_metric"] {
			case "urnetwork_circle_transfer_admission_wait_seconds_count":
				series.Value[1] = json.RawMessage(`"1e-300"`)
			case "urnetwork_circle_transfer_admission_wait_seconds_sum":
				series.Value[1] = json.RawMessage(`"1e300"`)
			}
		}
	})
	requireCircleAdmissionOrderInvariant(t, now, payload, "circle-transfer-admission-unobservable")
	peer := process
	peer.host = "worker-b.example"
	payload = editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, process, peer), func(response *mimirInstantResponse) {
		for index := range response.Data.Result {
			series := &response.Data.Result[index]
			if series.Metric["monitor_metric"] == "urnetwork_circle_transfer_admission_errors_total" {
				series.Value[1] = json.RawMessage(`"1e308"`)
			}
		}
	})
	alerts := requireCircleAdmissionOrderInvariant(t, now, payload, "circle-transfer-admission-unobservable", "circle-transfer-admission-error")
	for _, alert := range alerts {
		if strings.Contains(alert.Markdown(), "+Inf") || strings.Contains(alert.Markdown(), "NaN") {
			t.Fatalf("nonfinite derived value escaped into finding: %s", alert.Markdown())
		}
	}
}

// Invalidating a duplicate family must not discard another complete signal on that process.
func TestCircleAdmissionSignalDuplicateFamilyPreservesIndependentSignals(t *testing.T) {
	now := time.Date(2026, 10, 19, 8, 10, 0, 0, time.UTC)
	process := circleAdmissionFixture{
		host: "worker-a.example", block: "synthetic", instance: "current", start: now.Add(-time.Hour),
		errors: 1, waitCount: 1, waitSum: 6,
	}
	for _, test := range []struct {
		metricName string
		classes    []string
	}{
		{metricName: "urnetwork_circle_transfer_admissions_total", classes: []string{"circle-transfer-admission-error", "circle-transfer-admission-pressure"}},
		{metricName: "urnetwork_circle_transfer_admission_errors_total", classes: []string{"circle-transfer-admission-pressure"}},
		{metricName: "urnetwork_circle_transfer_admission_wait_seconds_sum", classes: []string{"circle-transfer-admission-error"}},
	} {
		payload := editCircleAdmissionFixtureJson(t, circleAdmissionFixtureJSON(t, now, process), func(response *mimirInstantResponse) {
			for _, series := range response.Data.Result {
				if series.Metric["monitor_metric"] == test.metricName {
					response.Data.Result = append(response.Data.Result, series)
					return
				}
			}
		})
		classes := append([]string{"circle-transfer-admission-unobservable"}, test.classes...)
		requireCircleAdmissionOrderInvariant(t, now, payload, classes...)
	}
}
