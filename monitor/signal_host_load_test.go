package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type hostLoadFixture struct {
	host                 string
	load1                float64
	cores                float64
	cpuExecution         float64
	ioWait               float64
	memoryAvailableRatio float64
	age                  time.Duration
	omit                 string
}

func hostLoadFixtureJSON(t testing.TB, now time.Time, fixtures ...hostLoadFixture) string {
	t.Helper()
	result := []any{}
	for _, fixture := range fixtures {
		metrics := map[string]float64{
			"load1":                  fixture.load1,
			"cores":                  fixture.cores,
			"cpu_execution":          fixture.cpuExecution,
			"io_wait":                fixture.ioWait,
			"memory_available_ratio": fixture.memoryAvailableRatio,
		}
		for metric, value := range metrics {
			if metric == fixture.omit {
				continue
			}
			result = append(result, map[string]any{
				"metric": map[string]string{
					"env":            "synthetic",
					"host":           fixture.host,
					"job":            "node",
					"monitor_metric": metric,
				},
				"value": []any{float64(now.Add(-fixture.age).Unix()), fmt.Sprintf("%.9f", value)},
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

func hostLoadSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "compute-a.example.test" || !strings.Contains(command, "node_load1") ||
			!strings.Contains(command, "node_memory_MemAvailable_bytes") ||
			!strings.Contains(command, "node_cpu_seconds_total") {
			t.Fatalf("unexpected host-load command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	return SignalSettings{
		Environment: "synthetic",
		Source:      source,
		Now:         func() time.Time { return now },
		Hosts: []HostSettings{
			{Name: "compute-a.example.test", Roles: []string{"services"}},
			{Name: "compute-b.example.test", Roles: []string{"database"}},
		},
	}
}

func TestHostLoadSignalSyntheticCPUSaturation(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC)
	payload := hostLoadFixtureJSON(t, now,
		hostLoadFixture{host: "compute-a.example.test", load1: 216, cores: 72, cpuExecution: 0.96, ioWait: 0.002, memoryAvailableRatio: 0.62},
		hostLoadFixture{host: "compute-b.example.test", load1: 4, cores: 16, cpuExecution: 0.31, ioWait: 0.01, memoryAvailableRatio: 0.54},
	)
	alerts, err := NewHostLoadSignal().Run(context.Background(), hostLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "host-cpu-saturation")
	if alert.Target != "compute-a.example.test" || alert.Severity != SeverityPage || alert.Sustain != 2 {
		t.Fatalf("wrong CPU alert identity: %+v", alert)
	}
	for _, want := range []string{
		"load1=216.00",
		"logical_cpu_count=72",
		"normalized_load1=3.000",
		"cpu_execution_ratio_5m=0.9600",
		"do not reboot",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("CPU alert omitted %q: %s", want, alert.Markdown())
		}
	}
}

func TestHostLoadQuerySeparatesCPUExecutionFromIOWait(t *testing.T) {
	query := hostLoadQuery("synthetic")
	if !strings.Contains(query, `1 - avg without(cpu,mode)`) ||
		strings.Count(query, `rate(node_cpu_seconds_total{env="synthetic",job="node",mode="iowait"}[5m])`) != 2 ||
		!strings.Contains(query, `clamp_min(`) {
		t.Fatalf("CPU execution query does not subtract the separately reported I/O-wait ratio: %s", query)
	}
	if strings.Count(query, `timestamp(`) != 7 ||
		strings.Count(query, `>= time() - 90`) != 7 {
		t.Fatalf("host-load query does not enforce source-sample freshness for every input: %s", query)
	}
}

func TestHostLoadSignalSyntheticIOAndMemoryClasses(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 1, 0, 0, time.UTC)
	payload := hostLoadFixtureJSON(t, now,
		hostLoadFixture{host: "compute-a.example.test", load1: 28, cores: 16, cpuExecution: 0.45, ioWait: 0.31, memoryAvailableRatio: 0.07},
		hostLoadFixture{host: "compute-b.example.test", load1: 3, cores: 16, cpuExecution: 0.25, ioWait: 0.01, memoryAvailableRatio: 0.60},
	)
	alerts, err := NewHostLoadSignal().Run(context.Background(), hostLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	ioAlert := requireAlertClass(t, alerts, "host-io-saturation")
	if ioAlert.Target != "compute-a.example.test" || ioAlert.Severity != SeverityPage {
		t.Fatalf("wrong I/O alert: %+v", ioAlert)
	}
	memoryAlert := requireAlertClass(t, alerts, "host-memory-pressure")
	if memoryAlert.Target != "compute-a.example.test" || memoryAlert.Severity != SeverityWarn || memoryAlert.PageSustain != 5 {
		t.Fatalf("wrong memory alert: %+v", memoryAlert)
	}
}

func TestHostLoadSignalSyntheticMissingAndStaleAreNotGreen(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 2, 0, 0, time.UTC)
	payload := hostLoadFixtureJSON(t, now,
		hostLoadFixture{host: "compute-a.example.test", load1: 2, cores: 16, cpuExecution: 0.20, ioWait: 0.01, memoryAvailableRatio: 0.70},
		hostLoadFixture{host: "compute-b.example.test", load1: 300, cores: 16, cpuExecution: 1, ioWait: 0, memoryAvailableRatio: 0.50, age: 2 * time.Minute},
	)
	alerts, err := NewHostLoadSignal().Run(context.Background(), hostLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "host-metrics-missing")
	if alert.Target != "compute-b.example.test" || alert.Sustain != 2 || alert.PageSustain != 3 {
		t.Fatalf("wrong missing-host alert: %+v", alert)
	}
	if strings.Contains(alert.Observed, "300") {
		t.Fatalf("stale value escaped into observation: %+v", alert)
	}
}

func TestHostLoadSignalSyntheticIncompleteFamily(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 3, 0, 0, time.UTC)
	payload := hostLoadFixtureJSON(t, now,
		hostLoadFixture{host: "compute-a.example.test", load1: 2, cores: 16, cpuExecution: 0.20, ioWait: 0.01, memoryAvailableRatio: 0.70},
		hostLoadFixture{host: "compute-b.example.test", load1: 2, cores: 16, cpuExecution: 0.20, ioWait: 0.01, memoryAvailableRatio: 0.70, omit: "io_wait"},
	)
	alerts, err := NewHostLoadSignal().Run(context.Background(), hostLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "host-metrics-missing")
	if alert.Target != "compute-b.example.test" || !strings.Contains(alert.Observed, "required_metric_mask=1f") {
		t.Fatalf("wrong incomplete-family alert: %+v", alert)
	}
}

func TestHostLoadSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 15, 15, 4, 0, 0, time.UTC)
	payload := hostLoadFixtureJSON(t, now,
		hostLoadFixture{host: "compute-a.example.test", load1: 2, cores: 16, cpuExecution: 0.20, ioWait: 0.01, memoryAvailableRatio: 0.70},
		hostLoadFixture{host: "compute-b.example.test", load1: 3, cores: 16, cpuExecution: 0.25, ioWait: 0.02, memoryAvailableRatio: 0.66},
	)
	alerts, err := NewHostLoadSignal().Run(context.Background(), hostLoadSettings(t, now, payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy host fixture alerted: %+v", alerts)
	}
}
