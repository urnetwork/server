package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

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

func TestProxyTransportSignalSyntheticHealthyPrivateBudgets(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 0, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, process))
	if len(alerts) != 0 {
		t.Fatalf("healthy private carrier budgets alerted: %+v", alerts)
	}
}

func TestProxyTransportSignalSyntheticRolloutSelectsNewestGeneration(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 0, 30, 0, time.UTC)
	old := healthyProxyTransportFixture(now)
	old.instance = "generation-old"
	old.values["process_start_time_seconds"] = float64(now.Add(-time.Hour).Unix())
	old.omit["urnetwork_proxy_platform_transport_h3_preemptions_total"] = true
	current := healthyProxyTransportFixture(now)
	current.instance = "generation-current"
	current.values["process_start_time_seconds"] = float64(now.Add(-time.Minute).Unix())

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, old, current))
	if len(alerts) != 0 {
		t.Fatalf("draining generation contaminated current carrier state: %+v", alerts)
	}
}

func TestProxyTransportSignalSyntheticLegacySharedBudget(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 1, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.values["urnetwork_proxy_devices_live"] = 3
	process.values["urnetwork_proxy_device_memory_target_bytes"] = 72 << 20
	process.values["urnetwork_proxy_platform_transport_budget_bytes"] = 18 << 20
	process.values["urnetwork_proxy_platform_transports_max"] = 16

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, process))
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

func TestProxyTransportSignalSyntheticSustainedPendingWithoutChurn(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 2, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.values["urnetwork_proxy_platform_transports_used"] = 32
	process.values["urnetwork_proxy_platform_transports_pending_h1"] = 2
	process.values["urnetwork_proxy_platform_transports_pending_h1_bytes"] = 512 << 10
	process.values["urnetwork_proxy_platform_transport_slot_full_pending_h1_devices"] = 1
	process.values[proxyTransportCPURateMetric] = 0.9
	process.values[proxyTransportPreemptionRateMetric] = 0

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, process))
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

func TestProxyTransportSignalSyntheticIssue211PreemptionLoop(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 3, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.values["urnetwork_proxy_platform_transports_used"] = 32
	process.values["urnetwork_proxy_platform_transports_pending_h1"] = 1
	process.values["urnetwork_proxy_platform_transports_pending_h1_bytes"] = 256 << 10
	process.values["urnetwork_proxy_platform_transport_slot_full_pending_h1_devices"] = 1
	process.values["urnetwork_proxy_platform_transport_h3_preemptions_total"] = 900
	process.values["urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total"] = 850
	process.values[proxyTransportCPURateMetric] = 0.91
	process.values[proxyTransportPreemptionRateMetric] = 4.5

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, process))
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
		"software loop",
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

func TestProxyTransportSignalSyntheticMissingTelemetryIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 4, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.omit["urnetwork_proxy_platform_transport_h3_preemptions_total"] = true

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, process))
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

func TestProxyTransportSignalSyntheticMixedScrapeIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 5, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.sourceOffsets["urnetwork_proxy_platform_transport_budget_bytes"] = -15 * time.Second

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, process))
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

func TestProxyTransportSignalSyntheticImpossibleCounts(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 6, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.values["urnetwork_proxy_platform_transports_used"] = 33

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, process))
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

func TestProxyTransportSignalSyntheticRateWarmupDoesNotHidePending(t *testing.T) {
	now := time.Date(2026, 9, 13, 18, 7, 0, 0, time.UTC)
	process := healthyProxyTransportFixture(now)
	process.omitRates = true
	process.values["urnetwork_proxy_platform_transports_pending_h1"] = 1
	process.values["urnetwork_proxy_platform_transports_pending_h1_bytes"] = 256 << 10

	alerts := runProxyTransportFixture(t, now, proxyTransportFixtureJSON(t, process))
	alert := requireAlertClass(t, alerts, "proxy-transport-admission-pending")
	if !strings.Contains(alert.Markdown(), "churn_rates=warming-or-unobservable") {
		t.Fatalf("rate warmup was hidden:\n%s", alert.Markdown())
	}
}

func healthyProxyTransportFixture(now time.Time) proxyTransportFixtureProcess {
	return proxyTransportFixtureProcess{
		host: "proxy-node.invalid", block: "lane-a", instance: "generation-a",
		sampleTime: now, sourceTime: now,
		values: map[string]float64{
			"process_resident_memory_bytes":                                                1 << 30,
			"process_start_time_seconds":                                                   float64(now.Add(-10 * time.Minute).Unix()),
			"process_cpu_seconds_total":                                                    100,
			"urnetwork_proxy_devices_live":                                                 2,
			"urnetwork_proxy_device_memory_target_bytes":                                   48 << 20,
			"urnetwork_proxy_platform_transport_budget_bytes":                              12 << 20,
			"urnetwork_proxy_platform_transport_used_bytes":                                2 << 20,
			"urnetwork_proxy_platform_transports_max":                                      32,
			"urnetwork_proxy_platform_transports_used":                                     4,
			"urnetwork_proxy_platform_transports_pending_h1":                               0,
			"urnetwork_proxy_platform_transports_pending_h1_bytes":                         0,
			"urnetwork_proxy_platform_transport_slot_full_pending_h1_devices":              0,
			"urnetwork_proxy_platform_transport_h3_preemptions_total":                      2,
			"urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total": 0,
			proxyTransportCPURateMetric:                                                    0.2,
			proxyTransportPreemptionRateMetric:                                             0,
		},
		omit: map[string]bool{}, sourceOffsets: map[string]time.Duration{},
	}
}

func runProxyTransportFixture(t testing.TB, now time.Time, payload string) Alerts {
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
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.invalid", Roles: []string{"services"}})
	alerts, err := NewProxyTransportSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func proxyTransportFixtureJSON(t testing.TB, processes ...proxyTransportFixtureProcess) string {
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
			for _, metricName := range []string{proxyTransportCPURateMetric, proxyTransportPreemptionRateMetric} {
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
