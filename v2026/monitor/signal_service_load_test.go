package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type serviceLoadFixture struct {
	host               string
	service            string
	block              string
	instance           string
	rss                float64
	heap               float64
	objects            float64
	goroutines         float64
	start              float64
	cpu                float64
	allocation         float64
	gc                 float64
	age                time.Duration
	omit               string
	lazyForwardIngress *float64
	residentCount      *float64
}

func serviceLoadFixtureJSON(t testing.TB, now time.Time, hostCores map[string]float64, fixtures ...serviceLoadFixture) string {
	t.Helper()
	result := []any{}
	for host, cores := range hostCores {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"env": "synthetic", "host": host, "job": "node", "monitor_metric": "host_cores",
			},
			"value": []any{float64(now.Unix()), fmt.Sprintf("%.9f", cores)},
		})
	}
	for _, fixture := range fixtures {
		metrics := map[string]float64{
			"process_resident_memory_bytes": fixture.rss,
			"go_memstats_heap_alloc_bytes":  fixture.heap,
			"go_memstats_heap_objects":      fixture.objects,
			"go_goroutines":                 fixture.goroutines,
			"process_start_time_seconds":    fixture.start,
			"cpu":                           fixture.cpu,
			"allocation":                    fixture.allocation,
			"gc":                            fixture.gc,
		}
		if fixture.residentCount != nil {
			metrics["resident_clients"] = *fixture.residentCount
		}
		for metric, value := range metrics {
			if metric == fixture.omit {
				continue
			}
			result = append(result, map[string]any{
				"metric": map[string]string{
					"env":            "synthetic",
					"host":           fixture.host,
					"job":            fixture.service,
					"service":        fixture.service,
					"block":          fixture.block,
					"instance":       fixture.instance,
					"monitor_metric": metric,
				},
				"value": []any{float64(now.Add(-fixture.age).Unix()), fmt.Sprintf("%.9f", value)},
			})
		}
		if fixture.lazyForwardIngress != nil {
			result = append(result, map[string]any{
				"metric": map[string]string{
					"env":            "synthetic",
					"host":           fixture.host,
					"job":            fixture.service,
					"service":        fixture.service,
					"block":          fixture.block,
					"instance":       fixture.instance,
					"monitor_metric": "lazy_forward_ingress",
				},
				"value": []any{float64(now.Add(-fixture.age).Unix()), fmt.Sprintf("%.9f", *fixture.lazyForwardIngress)},
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

func serviceLoadSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "compute-a.example.test" || !strings.Contains(command, "process_resident_memory_bytes") ||
			!strings.Contains(command, "go_goroutines") || !strings.Contains(command, "process_cpu_seconds_total") ||
			!strings.Contains(command, "node_cpu_seconds_total") {
			t.Fatalf("unexpected service-load command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	return SignalSettings{
		Environment: "synthetic",
		LogServices: []string{"relay"},
		Source:      source,
		Now:         func() time.Time { return now },
		Hosts: []HostSettings{
			{Name: "compute-a.example.test", Roles: []string{"services"}},
			{Name: "compute-b.example.test", Roles: []string{"services"}},
		},
	}
}

func TestServiceLoadSignalSyntheticRunawayAndOverlap(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 10, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 72, "compute-b.example.test": 72},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "current-a",
			rss: 82 * gib, heap: 47 * gib, objects: 61_000_000, goroutines: 915_000,
			start: float64(now.Add(-10 * time.Hour).Unix()), cpu: 10.8, allocation: 1600 << 20, gc: 0.05,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "replacement-a",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 30_000,
			start: float64(now.Add(-5 * time.Minute).Unix()), cpu: 0.4, allocation: 8 << 20, gc: 0.01,
		},
		serviceLoadFixture{
			host: "compute-b.example.test", service: "relay", block: "g2", instance: "current-b",
			rss: 3 * gib, heap: 2 * gib, objects: 3_000_000, goroutines: 40_000,
			start: float64(now.Add(-2 * time.Hour).Unix()), cpu: 0.6, allocation: 10 << 20, gc: 0.01,
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "service-runtime-runaway")
	if alert.Target != "compute-a.example.test/relay/g1" || alert.Frame != "current-a" || alert.Severity != SeverityPage || alert.Sustain != 2 {
		t.Fatalf("wrong runaway identity: %+v", alert)
	}
	for _, want := range []string{
		"rss_bytes=88046829568",
		"goroutines=915000",
		"cpu_cores_5m=10.800",
		"alloc_bytes_per_s_5m=1677721600",
		"same_block_generations=2",
		"Hard-terminate only a proven-stuck old generation",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("runaway alert omitted %q: %s", want, alert.Markdown())
		}
	}
}

func TestServiceLoadQueryEnforcesSourceSampleFreshness(t *testing.T) {
	query := serviceLoadQuery("synthetic", []string{"relay"})
	if strings.Count(query, `timestamp(`) != 9 ||
		strings.Count(query, `>= time() - 90`) != 9 {
		t.Fatalf("service-load query does not enforce source-sample freshness for every input: %s", query)
	}
	for _, metric := range []string{
		"process_resident_memory_bytes",
		"go_memstats_heap_alloc_bytes",
		"go_memstats_heap_objects",
		"go_goroutines",
		"process_start_time_seconds",
		"process_cpu_seconds_total",
		"go_memstats_alloc_bytes_total",
		"go_gc_duration_seconds_count",
		"node_cpu_seconds_total",
	} {
		if !strings.Contains(query, `timestamp(`+metric+`{`) {
			t.Fatalf("service-load query omits source freshness for %s: %s", metric, query)
		}
	}
}

func TestServiceLoadQueryIncludesConnectLazyForwardCapability(t *testing.T) {
	query := serviceLoadQuery("synthetic", []string{"connect", "relay"})
	if !strings.Contains(query, `urnetwork_connect_resident_lazy_forward_ingress_enabled{env="synthetic",job="connect"}`) ||
		!strings.Contains(query, `monitor_metric","lazy_forward_ingress"`) ||
		!strings.Contains(query, `timestamp(urnetwork_connect_resident_clients{env="synthetic",job="connect"}) >= time() - 90`) ||
		!strings.Contains(query, `monitor_metric","resident_clients"`) ||
		strings.Count(query, `timestamp(`) != 11 || strings.Count(query, `>= time() - 90`) != 11 {
		t.Fatalf("service-load query does not retain the exact fresh Connect capability: %s", query)
	}
	withoutConnect := serviceLoadQuery("synthetic", []string{"relay"})
	if strings.Contains(withoutConnect, "resident_lazy_forward_ingress_enabled") || strings.Contains(withoutConnect, "urnetwork_connect_resident_clients") {
		t.Fatal("service-load queried a Connect-only capability without a configured Connect service")
	}
}

func TestServiceLoadConnectLazyForwardCapabilityAndRunawayDiagnosis(t *testing.T) {
	now := time.Date(2026, 9, 16, 9, 20, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	run := func(capability *float64) Alerts {
		t.Helper()
		payload := serviceLoadFixtureJSON(t, now,
			map[string]float64{"compute-a.example.test": 72},
			serviceLoadFixture{
				host: "compute-a.example.test", service: "connect", block: "g1", instance: "current",
				rss: 74 * gib, heap: 56 * gib, objects: 95_000_000, goroutines: 900_000,
				start: float64(now.Add(-18 * time.Hour).Unix()), cpu: 7.5, allocation: 520 << 20, gc: 0.02,
				lazyForwardIngress: capability,
			},
		)
		settings := serviceLoadSettings(t, now, payload)
		settings.LogServices = []string{"connect"}
		alerts, err := NewServiceLoadSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}

	missingAlerts := run(nil)
	capabilityAlert := requireAlertClass(t, missingAlerts, "connect-resident-ingress-capability-unobservable")
	for _, want := range []string{
		"newest_connect_processes=1 capability_enabled=0 capability_missing=1",
		serviceLoadLazyForwardCommit,
		"modified build",
		"does not by itself prove legacy code",
	} {
		if !strings.Contains(capabilityAlert.Markdown(), want) {
			t.Fatalf("missing-capability alert omitted %q: %s", want, capabilityAlert.Markdown())
		}
	}
	runaway := requireAlertClass(t, missingAlerts, "service-runtime-runaway")
	for _, want := range []string{"resident_lazy_forward_ingress_capability=missing", serviceLoadLazyForwardCommit, "Deploy Connect"} {
		if !strings.Contains(runaway.Markdown(), want) {
			t.Fatalf("Connect runaway diagnosis omitted %q: %s", want, runaway.Markdown())
		}
	}

	enabled := 1.0
	enabledAlerts := run(&enabled)
	for _, alert := range enabledAlerts {
		if alert.Class == "connect-resident-ingress-capability-unobservable" {
			t.Fatalf("proven current capability remained unobservable: %+v", alert)
		}
	}
	runaway = requireAlertClass(t, enabledAlerts, "service-runtime-runaway")
	for _, want := range []string{"resident_lazy_forward_ingress_capability=enabled", "bounded aggregate profile", "remaining resident"} {
		if !strings.Contains(runaway.Markdown(), want) {
			t.Fatalf("capability-proven diagnosis omitted %q: %s", want, runaway.Markdown())
		}
	}
}

func TestServiceLoadConnectCapabilitySelectsNewestGeneration(t *testing.T) {
	now := time.Date(2026, 9, 16, 9, 21, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	enabled := 1.0
	residentCount := 1000.0
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 72},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "connect", block: "g1", instance: "draining",
			rss: 72 * gib, heap: 50 * gib, objects: 80_000_000, goroutines: 800_000,
			start: float64(now.Add(-18 * time.Hour).Unix()), cpu: 7, allocation: 500 << 20, gc: 0.02,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "connect", block: "g1", instance: "replacement",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-5 * time.Minute).Unix()), cpu: 0.3, allocation: 4 << 20, gc: 0.01,
			lazyForwardIngress: &enabled,
			residentCount:      &residentCount,
		},
	)
	settings := serviceLoadSettings(t, now, payload)
	settings.LogServices = []string{"connect"}
	alerts, err := NewServiceLoadSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		if alert.Class == "connect-resident-ingress-capability-unobservable" || alert.Class == "connect-resident-cost-unobservable" {
			t.Fatalf("draining legacy generation overrode newest capability: %+v", alert)
		}
	}
	if runaway := requireAlertClass(t, alerts, "service-runtime-runaway"); runaway.Frame != "draining" ||
		!strings.Contains(runaway.Observed, "resident_count_status=missing") {
		t.Fatalf("draining resource owner disappeared from runtime findings: %+v", runaway)
	}
}

func TestServiceLoadResidentCostPopulationAndInflatedCounterexample(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	enabled, ordinaryCount, hotCount := 1.0, 5000.0, 20000.0
	payload := serviceLoadFixtureJSON(t, now, map[string]float64{"compute-a.example.test": 72},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "connect", block: "g1", instance: "ordinary",
			rss: 5000 * (2 << 20), heap: 5000 * (1 << 20), objects: 1_000_000, goroutines: 200_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 2, lazyForwardIngress: &enabled, residentCount: &ordinaryCount,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "connect", block: "g2", instance: "population-scaled",
			rss: 20000 * (2 << 20), heap: 20000 * (1 << 20), objects: 4_000_000, goroutines: 800_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 8, lazyForwardIngress: &enabled, residentCount: &hotCount,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "connect", block: "g3", instance: "per-resident-inflated",
			rss: 20000 * (2 << 20), heap: 20000 * (1 << 20), objects: 4_000_000, goroutines: 800_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 8, lazyForwardIngress: &enabled, residentCount: &ordinaryCount,
		},
	)
	settings := serviceLoadSettings(t, now, payload)
	settings.LogServices = []string{"connect"}
	alerts, err := NewServiceLoadSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("population evidence changed the raw runtime thresholds: %+v", alerts)
	}
	for _, alert := range alerts {
		if alert.Class != "service-runtime-runaway" || alert.Severity != SeverityPage || alert.Sustain != 2 {
			t.Fatalf("normalized evidence changed the PAGE contract: %+v", alert)
		}
		want := "resident_count=20000 rss_bytes_per_resident=2097152.00 heap_bytes_per_resident=1048576.00 goroutines_per_resident=40.000"
		if alert.Frame == "per-resident-inflated" {
			want = "resident_count=5000 rss_bytes_per_resident=8388608.00 heap_bytes_per_resident=4194304.00 goroutines_per_resident=160.000"
		} else if alert.Frame != "population-scaled" {
			t.Fatalf("healthy matched control alerted: %+v", alert)
		}
		if !strings.Contains(alert.Markdown(), want) || !strings.Contains(alert.Markdown(), "do not isolate resident allocations") {
			t.Fatalf("missing descriptive exact-process cost evidence %q: %s", want, alert.Markdown())
		}
	}
}

// Add only the optional gauge: a foreign generation must not become a phantom
// process or lend its population to the current raw runtime tuple.
func serviceLoadAppendResidentSample(t testing.TB, payload string, now time.Time, value string, age time.Duration, labels map[string]string, copies int) string {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		t.Fatal(err)
	}
	metric := map[string]string{
		"env": "synthetic", "host": "compute-a.example.test", "job": "connect", "service": "connect",
		"block": "g1", "instance": "current", "monitor_metric": "resident_clients",
	}
	for key, label := range labels {
		metric[key] = label
	}
	data := response["data"].(map[string]any)
	result := data["result"].([]any)
	for copyIndex := 0; copyIndex < copies; copyIndex++ {
		result = append(result, map[string]any{
			"metric": metric,
			"value":  []any{float64(now.Add(-age).Unix()), value},
		})
	}
	data["result"] = result
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestServiceLoadResidentCountFailuresCannotSuppressRawPage(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 1, 0, 0, time.UTC)
	enabled := 1.0
	base := serviceLoadFixtureJSON(t, now, map[string]float64{"compute-a.example.test": 72}, serviceLoadFixture{
		host: "compute-a.example.test", service: "connect", block: "g1", instance: "current",
		rss: 72 << 30, heap: 40 << 30, objects: 4_000_000, goroutines: 800_000,
		start: float64(now.Add(-time.Hour).Unix()), cpu: 8, lazyForwardIngress: &enabled,
	})
	for _, test := range []struct {
		name   string
		value  string
		age    time.Duration
		labels map[string]string
		copies int
		status string
		extra  string
	}{
		{name: "missing", value: "20000", copies: 0, status: "missing"},
		{name: "stale", value: "20000", age: 91 * time.Second, copies: 1, status: "missing"},
		{name: "future", value: "20000", age: -31 * time.Second, copies: 1, status: "missing"},
		{name: "duplicate", value: "20000", copies: 2, status: "mixed"},
		{name: "conflicting-counts", value: "20000", copies: 1, extra: "5000", status: "mixed"},
		{name: "malformed-plus-valid", value: "private-resident-value", copies: 1, extra: "20000", status: "mixed"},
		{name: "foreign-generation", value: "20000", labels: map[string]string{"instance": "private-foreign-generation"}, copies: 1, status: "missing"},
		{name: "foreign-service", value: "20000", labels: map[string]string{"service": "relay"}, copies: 1, status: "missing"},
		{name: "mixed-job", value: "20000", labels: map[string]string{"job": "relay"}, copies: 1, status: "invalid"},
		{name: "mixed-environment", value: "20000", labels: map[string]string{"env": "other-synthetic"}, copies: 1, status: "invalid"},
		{name: "negative", value: "-1", copies: 1, status: "invalid"},
		{name: "fractional", value: "0.5", copies: 1, status: "invalid"},
		{name: "not-finite", value: "NaN", copies: 1, status: "invalid"},
		{name: "infinite", value: "+Inf", copies: 1, status: "invalid"},
		{name: "out-of-range", value: "9007199254740992", copies: 1, status: "invalid"},
		{name: "malformed", value: "private-resident-value", copies: 1, status: "invalid"},
	} {
		payload := serviceLoadAppendResidentSample(t, base, now, test.value, test.age, test.labels, test.copies)
		if test.extra != "" {
			payload = serviceLoadAppendResidentSample(t, payload, now, test.extra, 0, nil, 1)
		}
		settings := serviceLoadSettings(t, now, payload)
		settings.LogServices = []string{"connect"}
		alerts, err := NewServiceLoadSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: optional resident telemetry suppressed raw evaluation: %v", test.name, err)
		}
		runaway := requireAlertClass(t, alerts, "service-runtime-runaway")
		if !strings.Contains(runaway.Observed, "resident_count_status="+test.status+" resident_cost_ratios=unobservable") {
			t.Fatalf("%s: denominator was borrowed or accepted: %s", test.name, runaway.Observed)
		}
		cost := requireAlertClass(t, alerts, "connect-resident-cost-unobservable")
		if cost.Severity != SeverityWarn || !strings.Contains(cost.Observed, "resident_count_"+test.status+"=1") {
			t.Fatalf("%s: wrong fixed unobservable class: %+v", test.name, cost)
		}
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-resident-value", "private-foreign-generation", "other-synthetic", "rss_bytes_per_resident=", "goroutines_per_resident=")
		}
	}
}

func TestServiceLoadZeroResidentsDoNotEraseProcessState(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 2, 0, 0, time.UTC)
	enabled, residents := 1.0, 0.0
	payload := serviceLoadFixtureJSON(t, now, map[string]float64{"compute-a.example.test": 72}, serviceLoadFixture{
		host: "compute-a.example.test", service: "connect", block: "g1", instance: "current",
		rss: 72 << 30, heap: 40 << 30, objects: 4_000_000, goroutines: 800_000,
		start: float64(now.Add(-time.Hour).Unix()), cpu: 8, lazyForwardIngress: &enabled, residentCount: &residents,
	})
	settings := serviceLoadSettings(t, now, payload)
	settings.LogServices = []string{"connect"}
	alerts, err := NewServiceLoadSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("valid zero count is not missing telemetry: %+v", alerts)
	}
	runaway := requireAlertClass(t, alerts, "service-runtime-runaway")
	if !strings.Contains(runaway.Markdown(), "resident_count_status=zero resident_count=0 resident_cost_ratios=undefined") ||
		!strings.Contains(runaway.Observed, "goroutines=800000") {
		t.Fatalf("zero denominator erased retained process state: %s", runaway.Markdown())
	}
	requireAlertOmits(t, runaway, "rss_bytes_per_resident=", "heap_bytes_per_resident=", "goroutines_per_resident=", "NaN", "+Inf")
}

func TestServiceLoadResidentCountMarkdownPrivacyAndFreshnessBoundary(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 3, 0, 0, time.UTC)
	enabled := 1.0
	base := serviceLoadFixtureJSON(t, now, map[string]float64{"compute-a.example.test": 72}, serviceLoadFixture{
		host: "compute-a.example.test", service: "connect", block: "g1", instance: "current",
		rss: 72 << 30, heap: 40 << 30, objects: 4_000_000, goroutines: 800_000,
		start: float64(now.Add(-time.Hour).Unix()), cpu: 8, lazyForwardIngress: &enabled,
	})
	payload := serviceLoadAppendResidentSample(t, base, now, "20000", 90*time.Second, map[string]string{
		"device": "private-device-synthetic", "customer": "private-customer-synthetic", "payload": "[private](https://private.example.test)",
	}, 1)
	settings := serviceLoadSettings(t, now, payload)
	settings.LogServices = []string{"connect"}
	alerts, err := NewServiceLoadSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("exact freshness boundary did not retain the unique resident gauge: %+v", alerts)
	}
	runaway := requireAlertClass(t, alerts, "service-runtime-runaway")
	if !strings.Contains(runaway.Markdown(), "resident_count=20000") {
		t.Fatalf("Markdown omitted the bounded resident count: %s", runaway.Markdown())
	}
	requireAlertOmits(t, runaway, "private-device-synthetic", "private-customer-synthetic", "private.example.test", "\"device\"", "\"customer\"")
}

func TestServiceLoadEqualNewestStartsRemainUnobservable(t *testing.T) {
	processes := map[string]*serviceLoadMetrics{}
	for _, instance := range []string{"generation-a", "generation-b"} {
		processes[instance] = &serviceLoadMetrics{
			host: "compute-a.example.test", service: "connect", block: "g1", instance: instance,
			start: 100, mask: serviceLoadMetricRSS | serviceLoadMetricStart | serviceLoadMetricLazyForwardIngress,
			lazyForwardIngress: 1, residents: &serviceLoadResidentSample{value: 20000, count: 1},
		}
	}
	for _, result := range []finding{
		serviceLoadConnectCapabilityFinding(processes, true),
		serviceLoadConnectResidentCostFinding(processes, true),
	} {
		if !strings.Contains(result.observed, "newest_connect_processes=0") || !strings.Contains(result.observed, "generation_unselectable=1") {
			t.Fatalf("equal starts selected an arbitrary generation: %+v", result)
		}
	}
}

func TestServiceLoadSignalSyntheticHardCeilingWithoutRates(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 11, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 72},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "oversize",
			rss: 70 * gib, heap: 40 * gib, objects: 50_000_000, goroutines: 100_000,
			start: float64(now.Add(-30 * time.Minute).Unix()), omit: "cpu",
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "service-runtime-runaway")
	if alert.Frame != "oversize" {
		t.Fatalf("hard RSS ceiling lost process identity: %+v", alert)
	}
}

func TestServiceLoadSignalSyntheticIncompleteRawFamily(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 12, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 16},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "partial",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 0.2, allocation: 4 << 20, gc: 0.01,
			omit: "go_goroutines",
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "service-runtime-metrics-incomplete")
	if alert.Target != "compute-a.example.test/relay/g1" || alert.Frame != "partial" {
		t.Fatalf("wrong incomplete process identity: %+v", alert)
	}
}

func TestServiceLoadSignalSyntheticInvalidLabelsAreReduced(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 13, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 16},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "healthy",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 0.2, allocation: 4 << 20, gc: 0.01,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "invalid/block", instance: "private-value",
			rss: 80 * gib, heap: 50 * gib, objects: 90_000_000, goroutines: 900_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 9, allocation: 2 << 30, gc: 0.1,
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "service-runtime-metrics-invalid")
	if !strings.Contains(alert.Observed, "invalid_series=8") {
		t.Fatalf("invalid series were not reduced to a count: %+v", alert)
	}
	requireAlertOmits(t, alert, "invalid/block", "private-value")
}

func TestServiceLoadSignalSyntheticStaleRunawayIsIgnored(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 14, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 16},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "healthy",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 0.2, allocation: 4 << 20, gc: 0.01,
		},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g2", instance: "stale",
			rss: 80 * gib, heap: 50 * gib, objects: 90_000_000, goroutines: 900_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 9, allocation: 2 << 30, gc: 0.1,
			age: 2 * time.Minute,
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("stale runaway contaminated current state: %+v", alerts)
	}
}

func TestServiceLoadSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 15, 0, 0, time.UTC)
	const gib = float64(uint64(1) << 30)
	payload := serviceLoadFixtureJSON(t, now,
		map[string]float64{"compute-a.example.test": 16},
		serviceLoadFixture{
			host: "compute-a.example.test", service: "relay", block: "g1", instance: "healthy",
			rss: 2 * gib, heap: 1 * gib, objects: 2_000_000, goroutines: 20_000,
			start: float64(now.Add(-time.Hour).Unix()), cpu: 0.2, allocation: 4 << 20, gc: 0.01,
		},
	)
	alerts, err := NewServiceLoadSignal().Run(context.Background(), serviceLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy service fixture alerted: %+v", alerts)
	}
}
