// Exercises aggregate device-admission classification with synthetic Mimir vectors.
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

type proxyDeviceAdmissionFixtureProcess struct {
	host, block, instance string
	start                 time.Time
	values                map[string]float64
	omit                  map[string]bool
}

func TestProxyDeviceAdmissionSignalSyntheticHealthyStableWindow(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 4, 0, 0, time.UTC)
	alerts := runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, healthyProxyDeviceAdmissionFixture(now)))
	if len(alerts) != 0 {
		t.Fatalf("healthy stable admission window alerted: %+v", alerts)
	}
}

func TestProxyDeviceAdmissionSignalSyntheticRefusalBelowRatioOnePages(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 5, 0, 0, time.UTC)
	process := healthyProxyDeviceAdmissionFixture(now)
	process.values[proxyDeviceAdmissionMetricCounter] = 17
	process.values[proxyDeviceAdmissionMetricDelta] = 1
	process.values[proxyDeviceAdmissionMetricBudget] = 8 * 1024 * 1024 * 1024
	process.values[proxyDeviceAdmissionMetricUsed] = 341 * 24 * 1024 * 1024

	alert := requireAlertClass(t, runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, process)), "proxy-device-admission-refused")
	if alert.Severity != SeverityPage || alert.Sustain != 1 || alert.SignalNumber != "14.7d" {
		t.Fatalf("wrong refusal alert identity: %+v", alert)
	}
	for _, want := range []string{
		"refusal_attempt_delta=1",
		"remaining_bytes=8388608",
		"used_budget_ratio=0.9990234375",
		"context only",
		"not host or fleet hardware exhaustion",
		"§14.7 direct host RSS",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("refusal alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Markdown(), "toy-private-device-2099") {
		t.Fatalf("refusal alert exposed an unneeded private label: %s", alert.Markdown())
	}
}

func TestProxyDeviceAdmissionSignalSyntheticOverlapRefusalRemainsVisible(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 6, 0, 0, time.UTC)
	old := healthyProxyDeviceAdmissionFixture(now)
	old.instance = "generation-old"
	old.start = now.Add(-2 * time.Hour)
	old.values[proxyDeviceAdmissionMetricStart] = float64(old.start.Unix())
	old.values[proxyDeviceAdmissionMetricCounter] = 4
	old.values[proxyDeviceAdmissionMetricDelta] = 2
	current := healthyProxyDeviceAdmissionFixture(now)
	current.instance = "generation-current"

	alert := requireAlertClass(t, runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, old, current)), "proxy-device-admission-refused")
	if !strings.Contains(alert.Markdown(), "generation-old") || strings.Contains(alert.Markdown(), "refusing_generations=2") {
		t.Fatalf("overlap refusal attribution is wrong:\n%s", alert.Markdown())
	}
}

func TestProxyDeviceAdmissionSignalSyntheticMissingGaugeDoesNotHideRefusal(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 7, 0, 0, time.UTC)
	process := healthyProxyDeviceAdmissionFixture(now)
	process.values[proxyDeviceAdmissionMetricCounter] = 3
	process.values[proxyDeviceAdmissionMetricDelta] = 1
	process.omit[proxyDeviceAdmissionMetricBudget] = true

	alerts := runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, process))
	page := requireAlertClass(t, alerts, "proxy-device-admission-refused")
	visibility := requireAlertClass(t, alerts, "proxy-device-admission-unobservable")
	if !strings.Contains(page.Markdown(), "budget_bytes=unknown") || !strings.Contains(visibility.Markdown(), proxyDeviceAdmissionMetricBudget) {
		t.Fatalf("missing contextual gauge was not retained: page=%s visibility=%s", page.Markdown(), visibility.Markdown())
	}
}

func TestProxyDeviceAdmissionSignalSyntheticMissingMetricsAreOneGapAndNotWarmup(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 7, 30, 0, time.UTC)
	process := healthyProxyDeviceAdmissionFixture(now)
	process.omit[proxyDeviceAdmissionMetricCounter] = true
	process.omit[proxyDeviceAdmissionMetricBudget] = true
	process.omit[proxyDeviceAdmissionMetricUsed] = true

	visibility := requireAlertClass(
		t,
		runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, process)),
		"proxy-device-admission-unobservable",
	)
	if strings.Contains(visibility.Observed, "quiet-window-warming") {
		t.Fatalf("missing metrics were also misclassified as warmup: %s", visibility.Observed)
	}
	if strings.Count(visibility.Observed, proxyDeviceAdmissionLabel(&proxyDeviceAdmissionMetrics{
		host: process.host, block: process.block, instance: process.instance,
	})) != 1 {
		t.Fatalf("one incomplete process emitted duplicate visibility gaps: %s", visibility.Observed)
	}
	for _, metric := range []string{
		proxyDeviceAdmissionMetricCounter,
		proxyDeviceAdmissionMetricBudget,
		proxyDeviceAdmissionMetricUsed,
	} {
		if !strings.Contains(visibility.Observed, metric) {
			t.Fatalf("visibility gap omitted %s: %s", metric, visibility.Observed)
		}
	}
}

func TestProxyDeviceAdmissionSignalSyntheticDisabledAndInvalidAreDistinct(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 8, 0, 0, time.UTC)
	disabled := healthyProxyDeviceAdmissionFixture(now)
	disabled.values[proxyDeviceAdmissionMetricBudget] = 0
	disabled.values[proxyDeviceAdmissionMetricUsed] = 0
	disabledAlerts := runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, disabled))
	requireAlertClass(t, disabledAlerts, "proxy-device-admission-disabled")
	requireNoProxyDeviceAdmissionClass(t, disabledAlerts, "proxy-device-admission-refused")

	invalid := healthyProxyDeviceAdmissionFixture(now)
	invalid.values[proxyDeviceAdmissionMetricBudget] = 100
	invalid.values[proxyDeviceAdmissionMetricUsed] = 101
	invalidAlerts := runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, invalid))
	contract := requireAlertClass(t, invalidAlerts, "proxy-device-admission-invalid")
	if !strings.Contains(contract.Markdown(), "used-exceeds-budget") {
		t.Fatalf("invalid gauge relation lost: %s", contract.Markdown())
	}
	requireNoProxyDeviceAdmissionClass(t, invalidAlerts, "proxy-device-admission-disabled")
}

func TestProxyDeviceAdmissionSignalSyntheticYoungGenerationIsUnknownUnlessRefused(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 9, 0, 0, time.UTC)
	young := healthyProxyDeviceAdmissionFixture(now)
	young.start = now.Add(-2 * time.Minute)
	young.values[proxyDeviceAdmissionMetricStart] = float64(young.start.Unix())
	unknown := requireAlertClass(t, runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, young)), "proxy-device-admission-unobservable")
	if !strings.Contains(unknown.Markdown(), "quiet-window-warming") {
		t.Fatalf("young quiet generation was not unknown: %s", unknown.Markdown())
	}

	young.values[proxyDeviceAdmissionMetricCounter] = 2
	page := requireAlertClass(t, runProxyDeviceAdmissionFixture(t, now, proxyDeviceAdmissionFixtureJSON(t, now, young)), "proxy-device-admission-refused")
	if !strings.Contains(page.Markdown(), "mode=since-process-start") || !strings.Contains(page.Markdown(), "refusal_attempt_delta=2") {
		t.Fatalf("young positive counter was not retained: %s", page.Markdown())
	}
}

func TestProxyDeviceAdmissionSignalSyntheticMissingExpectedBlockIsUnknown(t *testing.T) {
	now := time.Date(2099, 1, 2, 3, 10, 0, 0, time.UTC)
	alerts := runProxyDeviceAdmissionFixture(
		t,
		now,
		proxyDeviceAdmissionFixtureJSON(t, now, healthyProxyDeviceAdmissionFixture(now)),
		"lane-a", "lane-b",
	)
	visibility := requireAlertClass(t, alerts, "proxy-device-admission-unobservable")
	if !strings.Contains(visibility.Markdown(), "proxy.example.test/lane-b[absent-current-generation]") {
		t.Fatalf("missing expected topology was not visible: %s", visibility.Markdown())
	}
}

func TestProxyDeviceAdmissionSignalSyntheticCancellation(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "", context.Canceled }}
	settings := syntheticSettings(source)
	configureProxyDeviceAdmissionFixture(&settings, []string{"lane-a"})
	_, err := NewProxyDeviceAdmissionSignal().Run(t.Context(), settings)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}
}

func healthyProxyDeviceAdmissionFixture(now time.Time) proxyDeviceAdmissionFixtureProcess {
	start := now.Add(-time.Hour)
	return proxyDeviceAdmissionFixtureProcess{
		host: "proxy.example.test", block: "lane-a", instance: "generation-a", start: start,
		values: map[string]float64{
			proxyDeviceAdmissionMetricRSS:         1024 * 1024 * 1024,
			proxyDeviceAdmissionMetricStart:       float64(start.Unix()),
			proxyDeviceAdmissionMetricCounter:     0,
			proxyDeviceAdmissionMetricBudget:      8 * 1024 * 1024 * 1024,
			proxyDeviceAdmissionMetricUsed:        128 * 1024 * 1024,
			proxyDeviceAdmissionMetricDelta:       0,
			proxyDeviceAdmissionMetricResets:      0,
			proxyDeviceAdmissionMetricSamples:     41,
			proxyDeviceAdmissionMetricFirstSource: float64(now.Add(-proxyDeviceAdmissionWindow).Unix()),
			proxyDeviceAdmissionMetricLastSource:  float64(now.Unix()),
		},
		omit: map[string]bool{},
	}
}

func runProxyDeviceAdmissionFixture(t testing.TB, now time.Time, payload string, expectedBlocks ...string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			proxyDeviceAdmissionMarker,
			proxyDeviceAdmissionMetricCounter,
			proxyDeviceAdmissionMetricBudget,
			proxyDeviceAdmissionMetricUsed,
			"max_over_time(",
			"resets(",
			"count_over_time(",
			"--max-filesize 4194304",
			"time=" + fmt.Sprint(now.UTC().Truncate(proxyDeviceAdmissionSubqueryStep).Unix()),
			proxyDeviceAdmissionEndpoint,
		} {
			if !strings.Contains(command, want) {
				return "", fmt.Errorf("synthetic Mimir command lacks %q", want)
			}
		}
		if host.Name != "metrics.example.test" {
			return "", fmt.Errorf("unexpected synthetic query host %q", host.Name)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	if len(expectedBlocks) == 0 {
		expectedBlocks = []string{"lane-a"}
	}
	configureProxyDeviceAdmissionFixture(&settings, expectedBlocks)
	alerts, err := NewProxyDeviceAdmissionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func configureProxyDeviceAdmissionFixture(settings *SignalSettings, expectedBlocks []string) {
	settings.LogServices = []string{"proxy"}
	settings.LogServiceBlocks = map[string][]string{"proxy": append([]string(nil), expectedBlocks...)}
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "proxy.example.test", Proxy: &ProxyHostSettings{}},
		HostSettings{Name: "metrics.example.test", Roles: []string{"services"}},
	)
}

func proxyDeviceAdmissionFixtureJSON(t testing.TB, now time.Time, processes ...proxyDeviceAdmissionFixtureProcess) string {
	t.Helper()
	result := []map[string]any{}
	for _, process := range processes {
		for _, metric := range proxyDeviceAdmissionRawMetrics {
			if process.omit[metric] {
				continue
			}
			value, ok := process.values[metric]
			if !ok {
				t.Fatalf("fixture lacks value for %s", metric)
			}
			labels := map[string]string{
				"host": process.host, "block": process.block, "instance": process.instance,
				"monitor_metric": metric, "private_device": "toy-private-device-2099",
			}
			result = append(result,
				map[string]any{"metric": labels, "value": []any{float64(now.Unix()), fmt.Sprint(value)}},
				map[string]any{"metric": map[string]string{
					"host": process.host, "block": process.block, "instance": process.instance,
					"monitor_metric": metric + proxyDeviceAdmissionSourceSuffix,
				}, "value": []any{float64(now.Unix()), fmt.Sprint(now.Unix())}},
			)
		}
		for _, metric := range proxyDeviceAdmissionDerivedMetrics {
			value, ok := process.values[metric]
			if !ok {
				t.Fatalf("fixture lacks derived value for %s", metric)
			}
			result = append(result, map[string]any{
				"metric": map[string]string{
					"host": process.host, "block": process.block, "instance": process.instance,
					"monitor_metric": metric,
				},
				"value": []any{float64(now.Unix()), fmt.Sprint(value)},
			})
		}
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

func requireNoProxyDeviceAdmissionClass(t testing.TB, alerts Alerts, class string) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class {
			t.Fatalf("unexpected %s alert: %+v", class, alert)
		}
	}
}
