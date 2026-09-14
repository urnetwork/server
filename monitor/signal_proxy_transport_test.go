// Exercises carrier-budget classification using deterministic synthetic scrapes.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// Describes one synthetic process, including omitted families and scrape skew.
type proxyTransportFixtureProcess struct {
	host          string
	block         string
	instance      string
	sampleTime    time.Time
	sourceTime    time.Time
	values        map[string]float64
	omit          map[string]bool
	sourceOffsets map[string]time.Duration
	omitRates     bool
}

// Keeps coherent private budgets healthy when admission is clear.
func TestProxyTransportSignalSyntheticHealthyPrivateBudgets(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, process))
	if len(alerts) != 0 {
		t.Fatalf("healthy private carrier budgets alerted: %+v", alerts)
	}
}

// Excludes a draining generation before evaluating its missing telemetry.
func TestProxyTransportSignalSyntheticRolloutSelectsNewestGeneration(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 0, 30, 0, time.UTC)
	old := healthyProxyTransportFixture(now)
	old.instance = "generation-old"
	old.values["process_start_time_seconds"] = float64(now.Add(-time.Hour).Unix())
	old.omit["urnetwork_proxy_platform_transport_h3_preemptions_total"] = true
	current := healthyProxyTransportFixture(now)
	current.instance = "generation-current"
	current.values["process_start_time_seconds"] = float64(now.Add(-time.Minute).Unix())

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, old, current))
	if len(alerts) != 0 {
		t.Fatalf("draining generation contaminated current carrier state: %+v", alerts)
	}
}

// Detects a process-wide slot cap shared by multiple hosted devices.
func TestProxyTransportSignalSyntheticLegacySharedBudget(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 1, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.values["urnetwork_proxy_devices_live"] = 3
	process.values["urnetwork_proxy_device_memory_target_bytes"] = 72 * 1024 * 1024
	process.values["urnetwork_proxy_platform_transport_budget_bytes"] = 18 * 1024 * 1024
	process.values["urnetwork_proxy_platform_transports_max"] = 16

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, process))
	alert := requireAlertClass(t, alerts, "proxy-transport-budget-isolation")
	if alert.SignalNumber != "14.6" || alert.SignalKey != "proxy-transport" || alert.Sustain != 2 {
		t.Fatalf("wrong isolation alert identity: %+v", alert)
	}
	for _, want := range []string{
		"must own one target-derived carrier budget",
		"devices=3",
		"max-carriers=16-want=48",
		"software correctness",
		"not proof that the proxy fleet needs more hardware",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("isolation alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// Keeps stable pending admission distinct from recent preemption pressure.
func TestProxyTransportSignalSyntheticSustainedPendingWithoutChurn(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 2, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.values["urnetwork_proxy_platform_transports_used"] = 32
	process.values["urnetwork_proxy_platform_transports_pending_h1"] = 2
	process.values["urnetwork_proxy_platform_transports_pending_h1_bytes"] = 512 * 1024
	process.values["urnetwork_proxy_platform_transport_slot_full_pending_h1_devices"] = 1
	process.values[proxyTransportCpuRateMetric] = 0.9
	process.values[proxyTransportPreemptionRateMetric] = 0

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, process))
	alert := requireAlertClass(t, alerts, "proxy-transport-admission-pending")
	if alert.Sustain != 2 {
		t.Fatalf("pending sustain = %d, want 2", alert.Sustain)
	}
	for _, want := range []string{
		"waiting for private DeviceLocal admission",
		"pending_h1=2",
		"slot_full_pending_devices=1",
		"not by itself the connect#211 loop",
		"Do not blindly raise the cap",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("pending alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "proxy-transport-preemption-churn" {
			t.Fatalf("zero preemption rate was classified as churn: %+v", candidate)
		}
	}
}

// Reports the known loop's sampled signature while retaining causal limits.
func TestProxyTransportSignalSyntheticIssue211PreemptionLoop(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 3, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.values["urnetwork_proxy_platform_transports_used"] = 32
	process.values["urnetwork_proxy_platform_transports_pending_h1"] = 1
	process.values["urnetwork_proxy_platform_transports_pending_h1_bytes"] = 256 * 1024
	process.values["urnetwork_proxy_platform_transport_slot_full_pending_h1_devices"] = 1
	process.values["urnetwork_proxy_platform_transport_h3_preemptions_total"] = 900
	process.values["urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total"] = 850
	process.values[proxyTransportCpuRateMetric] = 0.91
	process.values[proxyTransportPreemptionRateMetric] = 4.5

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, process))
	alert := requireAlertClass(t, alerts, "proxy-transport-preemption-churn")
	if alert.Sustain != 2 {
		t.Fatalf("churn sustain = %d, want 2", alert.Sustain)
	}
	for _, want := range []string{
		"Slot-full pending-device count",
		"slot_full_h3_preemptions_per_second=4.5",
		"cpu_cores=0.91",
		"Connect f10a173",
		"urnetwork/connect#211",
		"suspected software loop",
		"Sampling cannot establish",
		"positive rate does not prove event-time saturation",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("churn alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	for _, candidate := range alerts {
		if candidate.Class == "proxy-transport-admission-pending" {
			t.Fatalf("proved churn also emitted the generic pending class: %+v", candidate)
		}
	}
}

// Refuses to treat a missing preemption counter as zero pressure.
func TestProxyTransportSignalSyntheticMissingTelemetryIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 4, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.omit["urnetwork_proxy_platform_transport_h3_preemptions_total"] = true

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, process))
	alert := requireAlertClass(t, alerts, "proxy-transport-unobservable")
	for _, want := range []string{
		"platform_transport_h3_preemptions_total",
		"must not be interpreted as zero pressure",
		"not carrier starvation",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing-metric alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// Refuses to infer fleet health from a healthy sibling when an expected block
// has no fresh process series at all.
func TestProxyTransportSignalSyntheticMissingExpectedBlockIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 4, 30, 0, time.UTC)
	process := healthyProxyTransportFixture(now)

	alerts := runProxyTransportFixture(
		t,
		now,
		proxyTransportFixtureJson(t, process),
		"lane-a",
		"lane-b",
	)
	alert := requireAlertClass(t, alerts, "proxy-transport-unobservable")
	for _, want := range []string{
		"1 of 2 expected Proxy host/block identities",
		"expected_proxy_identities=2",
		"absent_identities=1",
		"proxy-node.invalid/lane-b",
		"must not be interpreted as zero pressure",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing-identity alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// Refuses to build the expected-process denominator from a partial monitor
// host join when active services.yml has another Proxy placement.
func TestProxyTransportSignalSyntheticMissingExpectedHostIsUnknown(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	configureProxyTransportFixture(&settings, []string{"lane-a"})
	settings.ProxyPathExpectedHosts = 2

	alerts, err := NewProxyTransportSignal().Run(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "proxy-transport-unobservable")
	for _, want := range []string{
		"does not contain every active Proxy placement",
		"expected_proxy_hosts=2",
		"armed_proxy_hosts=1",
		"hide an entirely missing Proxy host",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing-host alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// Classifies mixed producer timestamps before comparing budget equations.
func TestProxyTransportSignalSyntheticMixedScrapeIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 5, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.sourceOffsets["urnetwork_proxy_platform_transport_budget_bytes"] = -15 * time.Second

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, process))
	alert := requireAlertClass(t, alerts, "proxy-transport-snapshot-unobservable")
	for _, want := range []string{
		"same-scrape carrier-budget snapshot",
		"source-time-skew",
		"backend visibility skew",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("mixed-scrape alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// Rejects used carrier counts above the aggregate maximum.
func TestProxyTransportSignalSyntheticImpossibleCounts(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 6, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.values["urnetwork_proxy_platform_transports_used"] = 33

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, process))
	alert := requireAlertClass(t, alerts, "proxy-transport-metrics-invalid")
	for _, want := range []string{
		"used-carriers-exceed-maximum",
		"internally inconsistent carrier accounting",
		"Do not call this a shared budget",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("invalid-metric alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

// Preserves pending admission while counter-rate ranges warm.
func TestProxyTransportSignalSyntheticRateWarmupDoesNotHidePending(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 7, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.omitRates = true
	process.values["urnetwork_proxy_platform_transports_pending_h1"] = 1
	process.values["urnetwork_proxy_platform_transports_pending_h1_bytes"] = 256 * 1024

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJson(t, process))
	alert := requireAlertClass(t, alerts, "proxy-transport-admission-pending")
	if !strings.Contains(alert.Markdown(), "churn_rates=warming-or-unobservable") {
		t.Fatalf("rate warmup was hidden:\n%s", alert.Markdown())
	}
}

// Parses the actual remote shell arguments without starting curl or any network.
func TestProxyTransportSignalQuotesApostropheEnvironment(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 8, 0, 0, time.UTC)
	environment := "synthetic'quoted"
	payload := proxyTransportFixtureJson(t, healthyProxyTransportFixture(now))
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		arguments, ok := strings.CutPrefix(command, "curl ")
		if !ok {
			return "", fmt.Errorf("unexpected query command")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "/bin/sh", "-c",
			"set -- "+arguments+"; printf '%s\\000' \"$@\"").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("parse query arguments: %w: %s", err, out)
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		want := []string{
			"-fsS", "--max-time", "15", "--data-urlencode",
			"query=" + proxyTransportQuery(environment),
			proxyTransportMimirEndpoint,
		}
		if !slices.Equal(got, want) {
			return "", fmt.Errorf("query arguments = %q, want %q", got, want)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = environment
	settings.Now = func() time.Time { return now }
	configureProxyTransportFixture(&settings, []string{"lane-a"})
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.example", Roles: []string{"services"}})
	alerts, err := NewProxyTransportSignal().Run(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy quoted environment alerted: %+v", alerts)
	}
}

// Propagates cancellation instead of translating an interrupted Mimir query
// into a healthy or provider-pressure observation.
func TestProxyTransportSignalSyntheticCancellation(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "", context.Canceled
	}}
	settings := syntheticSettings(source)
	configureProxyTransportFixture(&settings, []string{"lane-a"})
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.invalid", Roles: []string{"services"}})
	_, err := NewProxyTransportSignal().Run(t.Context(), settings)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v, want context.Canceled", err)
	}
}

// Builds one coherent, healthy process with two private carrier budgets.
func healthyProxyTransportFixture(now time.Time) proxyTransportFixtureProcess {
	return proxyTransportFixtureProcess{
		host: "proxy-node.invalid", block: "lane-a", instance: "generation-a",
		sampleTime: now, sourceTime: now,
		values: map[string]float64{
			"process_resident_memory_bytes":                                                1024 * 1024 * 1024,
			"process_start_time_seconds":                                                   float64(now.Add(-10 * time.Minute).Unix()),
			"process_cpu_seconds_total":                                                    100,
			"urnetwork_proxy_devices_live":                                                 2,
			"urnetwork_proxy_device_memory_target_bytes":                                   48 * 1024 * 1024,
			"urnetwork_proxy_platform_transport_budget_bytes":                              12 * 1024 * 1024,
			"urnetwork_proxy_platform_transport_used_bytes":                                2 * 1024 * 1024,
			"urnetwork_proxy_platform_transports_max":                                      32,
			"urnetwork_proxy_platform_transports_used":                                     4,
			"urnetwork_proxy_platform_transports_pending_h1":                               0,
			"urnetwork_proxy_platform_transports_pending_h1_bytes":                         0,
			"urnetwork_proxy_platform_transport_slot_full_pending_h1_devices":              0,
			"urnetwork_proxy_platform_transport_h3_preemptions_total":                      2,
			"urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total": 0,
			proxyTransportCpuRateMetric:                                                    0.2,
			proxyTransportPreemptionRateMetric:                                             0,
		},
		omit: map[string]bool{}, sourceOffsets: map[string]time.Duration{},
	}
}

// Supplies the generated query with a synthetic metrics gateway response.
func runProxyTransportFixture(t testing.TB, now time.Time, payload string, expectedBlocks ...string) Alerts {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics.invalid" ||
			!strings.Contains(command, "platform_transport_slot_full_pending_h1_devices") ||
			!strings.Contains(command, "platform_transport_h3_preemptions_total") ||
			!strings.Contains(command, "rate(process_cpu_seconds_total") ||
			!strings.Contains(command, `env="synthetic"`) ||
			!strings.Contains(command, "--data-urlencode 'query=") ||
			!strings.Contains(command, proxyTransportSourceSuffix) {
			return "", fmt.Errorf("unexpected Mimir command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	if len(expectedBlocks) == 0 {
		expectedBlocks = []string{"lane-a"}
	}
	configureProxyTransportFixture(&settings, expectedBlocks)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.invalid", Roles: []string{"services"}})
	alerts, err := NewProxyTransportSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

// Arms the exact active Proxy host/block denominator without embedding any
// production identity in synthetic tests.
func configureProxyTransportFixture(settings *SignalSettings, expectedBlocks []string) {
	settings.LogServices = []string{"proxy"}
	settings.LogServiceBlocks = map[string][]string{
		"proxy": append([]string(nil), expectedBlocks...),
	}
	settings.ProxyPathExpectedHosts = 1
	settings.Hosts = append(settings.Hosts, HostSettings{
		Name:  "proxy-node.invalid",
		Proxy: &ProxyHostSettings{},
	})
}

// Encodes independent values and producer timestamps as an instant vector.
func proxyTransportFixtureJson(t testing.TB, processes ...proxyTransportFixtureProcess) string {
	t.Helper()
	result := []map[string]any{}
	for _, process := range processes {
		for _, metricName := range proxyTransportMetricNames {
			if process.omit[metricName] {
				continue
			}
			value, ok := process.values[metricName]
			if !ok {
				t.Fatalf("fixture omits value for %s without marking it omitted", metricName)
			}
			labels := map[string]string{
				"host": process.host, "block": process.block, "instance": process.instance,
				"monitor_metric": metricName,
			}
			result = append(result, map[string]any{
				"metric": labels,
				"value":  []any{float64(process.sampleTime.Unix()), fmt.Sprintf("%.9g", value)},
			})
			sourceTime := process.sourceTime.Add(process.sourceOffsets[metricName])
			result = append(result, map[string]any{
				"metric": map[string]string{
					"host": process.host, "block": process.block, "instance": process.instance,
					"monitor_metric": metricName + proxyTransportSourceSuffix,
				},
				"value": []any{float64(process.sampleTime.Unix()), fmt.Sprintf("%d", sourceTime.Unix())},
			})
		}
		if !process.omitRates {
			for _, metricName := range []string{proxyTransportCpuRateMetric, proxyTransportPreemptionRateMetric} {
				result = append(result, map[string]any{
					"metric": map[string]string{
						"host": process.host, "block": process.block, "instance": process.instance,
						"monitor_metric": metricName,
					},
					"value": []any{float64(process.sampleTime.Unix()), fmt.Sprintf("%.9g", process.values[metricName])},
				})
			}
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
