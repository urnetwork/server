// Query-shaped source families exercise the real response parser and public
// signal, including the healthy findings used to clear earlier observations.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Each row has the labels and value pair returned by the instant query.
type hostLoadSourceRow struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

// Preserve the load metric name and I/O-wait mode that the real query retains.
func hostLoadSourceRows(now time.Time, fixture hostLoadFixture, sourceLabels map[string]string) []hostLoadSourceRow {
	rows := []hostLoadSourceRow{}
	for _, metric := range []struct {
		name  string
		value float64
	}{
		{"load1", fixture.load1},
		{"cores", fixture.cores},
		{"cpu_execution", fixture.cpuExecution},
		{"io_wait", fixture.ioWait},
		{"memory_available_ratio", fixture.memoryAvailableRatio},
	} {
		if metric.name == fixture.omit {
			continue
		}
		labels := map[string]string{
			"env": "synthetic", "host": fixture.host, "job": "node", "monitor_metric": metric.name,
		}
		for name, value := range sourceLabels {
			labels[name] = value
		}
		if metric.name == "load1" {
			labels["__name__"] = "node_load1"
		} else if metric.name == "io_wait" {
			labels["mode"] = "iowait"
		}
		rows = append(rows, hostLoadSourceRow{
			Metric: labels,
			Value:  []any{float64(now.Add(-fixture.age).Unix()), fmt.Sprintf("%.9f", metric.value)},
		})
	}
	return rows
}

// Run both production entry points over the same serialized response, with a
// synthetic host transport so no command or metrics connection is executed.
func hostLoadSourceResults(t testing.TB, now time.Time, rows []hostLoadSourceRow) ([]finding, Alerts) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"status": "success", "data": map[string]any{"resultType": "vector", "result": rows},
	})
	if err != nil {
		t.Fatal(err)
	}
	settings := hostLoadSettings(t, now, string(payload))
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (hostLoadProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewHostLoadSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return findings, alerts
}

// Rotate and reverse every row boundary, including both whole-family orders
// and mixed metric orders. Findings and public alert bytes must stay stable.
func hostLoadSourceOrders(rows []hostLoadSourceRow) [][]hostLoadSourceRow {
	orders := make([][]hostLoadSourceRow, 0, 2*len(rows))
	for offset := range rows {
		for _, direction := range []int{1, -1} {
			ordered := make([]hostLoadSourceRow, len(rows))
			for i := range rows {
				ordered[i] = rows[(offset+direction*i+len(rows))%len(rows)]
			}
			orders = append(orders, ordered)
		}
	}
	return orders
}

// Unknown source coverage must not publish a healthy finding that could clear
// an earlier incident; public alerts must retain each independent positive once.
func requireHostLoadSourceClasses(t testing.TB, findings []finding, alerts Alerts, target string, unknown bool, classes ...string) {
	t.Helper()
	want := map[string]int{}
	for _, class := range classes {
		want[class]++
	}
	if unknown {
		want["host-metrics-missing"]++
	}
	got := map[string]int{}
	for _, f := range findings {
		if f.target != target {
			continue
		}
		if f.healthy {
			if unknown {
				t.Fatalf("unknown source coverage cleared %s", f.class)
			}
			continue
		}
		got[f.class]++
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrong source findings for %s: got %v, want %v", target, got, want)
	}
	got = map[string]int{}
	for _, alert := range alerts {
		if alert.Target == target {
			got[alert.Class]++
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrong public source alerts for %s: got %v, want %v", target, got, want)
	}
}

// A hot source cannot be overwritten by a healthy sibling or lose its positive
// when the host's publisher identity is ambiguous.
func TestHostLoadSourcesCompleteConflictPreservesPositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	healthy := hostLoadFixture{host: "compute-a.example.test", load1: 2, cores: 16, cpuExecution: 0.2, ioWait: 0.01, memoryAvailableRatio: 0.7}
	peer := healthy
	peer.host = "compute-b.example.test"
	for _, c := range []struct {
		cpuExecution float64
		ioWait       float64
		memoryRatio  float64
		secondHot    bool
		classes      []string
	}{
		{0.96, 0.01, 0.7, false, []string{"host-cpu-saturation"}},
		{0.6, 0.3, 0.07, false, []string{"host-io-saturation", "host-memory-pressure"}},
		{0.96, 0.01, 0.7, true, []string{"host-cpu-saturation"}},
	} {
		hot := healthy
		hot.load1, hot.cpuExecution, hot.ioWait, hot.memoryAvailableRatio = 64, c.cpuExecution, c.ioWait, c.memoryRatio
		second := healthy
		if c.secondHot {
			second.load1, second.cpuExecution = 32, 0.94
		}
		rows := append(hostLoadSourceRows(now, hot, map[string]string{"instance": "exporter-a.example.test:9100"}),
			hostLoadSourceRows(now, second, map[string]string{"instance": "exporter-b.example.test:9100"})...)
		var firstFindings []finding
		var firstAlerts Alerts
		for index, ordered := range hostLoadSourceOrders(rows) {
			findings, alerts := hostLoadSourceResults(t, now, append(ordered, hostLoadSourceRows(now, peer, nil)...))
			for _, class := range c.classes {
				found := false
				for _, alert := range alerts {
					found = found || (alert.Target == healthy.host && alert.Class == class)
				}
				if !found {
					t.Fatalf("complete source positive %s was erased by source order %d", class, index)
				}
			}
			requireHostLoadSourceClasses(t, findings, alerts, healthy.host, true, c.classes...)
			requireHostLoadSourceClasses(t, findings, alerts, peer.host, false)
			missing := requireAlertClass(t, alerts, "host-metrics-missing")
			if !strings.Contains(missing.Observed, "source_count=2 complete_source_count=2") {
				t.Fatalf("missing coherent-source coverage counts: %s", missing.Observed)
			}
			for _, class := range c.classes {
				alert := requireAlertClass(t, alerts, class)
				if !strings.Contains(alert.Observed, "load1=64.00 logical_cpu_count=16 normalized_load1=4.000") {
					t.Fatalf("positive mixed source capacity: %s", alert.Observed)
				}
				requireAlertOmits(t, alert, "exporter-a.example.test", "exporter-b.example.test")
			}
			if index == 0 {
				firstFindings, firstAlerts = findings, alerts
			} else if !reflect.DeepEqual(firstFindings, findings) || !reflect.DeepEqual(firstAlerts, alerts) {
				t.Fatalf("source order %d changed complete-source results", index)
			}
		}
	}
}

// Partial families cannot complete one another, whether identity differs by
// exporter instance or another label that the query preserves.
func TestHostLoadSourcesSplitFamiliesStayUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 1, 0, 0, time.UTC)
	fixture := hostLoadFixture{host: "compute-a.example.test", load1: 64, cores: 16, cpuExecution: 0.96, ioWait: 0.01, memoryAvailableRatio: 0.7}
	peer := hostLoadFixture{host: "compute-b.example.test", load1: 2, cores: 16, cpuExecution: 0.2, ioWait: 0.01, memoryAvailableRatio: 0.7}
	for _, label := range []string{"instance", "publisher"} {
		for _, hot := range []bool{false, true} {
			candidate := fixture
			if !hot {
				candidate.load1, candidate.cpuExecution = 2, 0.2
			}
			first := hostLoadSourceRows(now, candidate, map[string]string{label: "synthetic-a"})
			second := hostLoadSourceRows(now, candidate, map[string]string{label: "synthetic-b"})
			rows := append(first[:2:2], second[2:]...)
			for _, ordered := range hostLoadSourceOrders(rows) {
				findings, alerts := hostLoadSourceResults(t, now, append(ordered, hostLoadSourceRows(now, peer, nil)...))
				requireHostLoadSourceClasses(t, findings, alerts, fixture.host, true)
				missing := requireAlertClass(t, alerts, "host-metrics-missing")
				if !strings.Contains(missing.Observed, "source_count=2 complete_source_count=0") {
					t.Fatalf("partial masks formed a complete source: %s", missing.Observed)
				}
			}
		}
	}
}

// Neither complete source meets a saturation predicate. A joined load, core
// count, or ratio from different sources could nevertheless fabricate a page.
func TestHostLoadSourcesCoherentHealthyTuplesCannotCreateSaturation(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 2, 0, 0, time.UTC)
	first := hostLoadFixture{host: "compute-a.example.test", load1: 64, cores: 128, cpuExecution: 0.96, ioWait: 0.01, memoryAvailableRatio: 0.7}
	second := hostLoadFixture{host: first.host, load1: 2, cores: 1, cpuExecution: 0.2, ioWait: 0.01, memoryAvailableRatio: 0.7}
	peer := second
	peer.host = "compute-b.example.test"
	for _, ioPressure := range []bool{false, true} {
		candidate := first
		if ioPressure {
			candidate.cpuExecution, candidate.ioWait = 0.6, 0.3
		}
		rows := append(hostLoadSourceRows(now, candidate, map[string]string{"instance": "synthetic-a"}),
			hostLoadSourceRows(now, second, map[string]string{"instance": "synthetic-b"})...)
		for _, ordered := range hostLoadSourceOrders(rows) {
			findings, alerts := hostLoadSourceResults(t, now, append(ordered, hostLoadSourceRows(now, peer, nil)...))
			requireHostLoadSourceClasses(t, findings, alerts, first.host, true)
		}
	}
}

// Repeated contributions invalidate their source even when values agree; an
// arbitrary result order must not choose the observation used for a verdict.
func TestHostLoadSourcesDuplicateContributionIsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 3, 0, 0, time.UTC)
	fixture := hostLoadFixture{host: "compute-a.example.test", load1: 64, cores: 16, cpuExecution: 0.96, ioWait: 0.01, memoryAvailableRatio: 0.7}
	peer := hostLoadFixture{host: "compute-b.example.test", load1: 2, cores: 16, cpuExecution: 0.2, ioWait: 0.01, memoryAvailableRatio: 0.7}
	for _, value := range []string{"64", "2", "NaN"} {
		rows := hostLoadSourceRows(now, fixture, nil)
		duplicate := rows[0]
		duplicate.Value = []any{float64(now.Unix()), value}
		rows = append(rows, duplicate)
		for _, ordered := range hostLoadSourceOrders(rows) {
			findings, alerts := hostLoadSourceResults(t, now, append(ordered, hostLoadSourceRows(now, peer, nil)...))
			requireHostLoadSourceClasses(t, findings, alerts, fixture.host, true)
			missing := requireAlertClass(t, alerts, "host-metrics-missing")
			if !strings.Contains(missing.Observed, "complete_source_count=0 invalid_source_count=1") {
				t.Fatalf("duplicate source remained authoritative: %s", missing.Observed)
			}
		}
	}
}

// A missing, invalid or stale source remains unknown while a separate complete
// source still supplies positive evidence for that same host.
func TestHostLoadSourcesInvalidSourcePreservesIndependentPositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 4, 0, 0, time.UTC)
	fixture := hostLoadFixture{host: "compute-a.example.test", load1: 64, cores: 16, cpuExecution: 0.96, ioWait: 0.01, memoryAvailableRatio: 0.7}
	peer := hostLoadFixture{host: "compute-b.example.test", load1: 2, cores: 16, cpuExecution: 0.2, ioWait: 0.01, memoryAvailableRatio: 0.7}
	for _, corrupt := range []func([]hostLoadSourceRow) []hostLoadSourceRow{
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			return rows[1:]
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value[1] = "NaN"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value[1] = "+Inf"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value[1] = "-Inf"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value[1] = "-1"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value[1] = "invalid"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value = []any{}
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value[0] = "invalid"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value[0] = float64(now.Add(-2 * time.Minute).Unix())
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[0].Value[0] = float64(now.Add(time.Minute).Unix())
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[1].Value[1] = "0"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[2].Value[1] = "1.2"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[3].Value[1] = "1.2"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			rows[4].Value[1] = "1.2"
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			delete(rows[0].Metric, "env")
			return rows
		},
		func(rows []hostLoadSourceRow) []hostLoadSourceRow {
			delete(rows[0].Metric, "job")
			return rows
		},
	} {
		rows := corrupt(hostLoadSourceRows(now, fixture, map[string]string{"instance": "synthetic-invalid"}))
		rows = append(rows, hostLoadSourceRows(now, fixture, map[string]string{"instance": "synthetic-valid"})...)
		for _, ordered := range hostLoadSourceOrders(rows) {
			findings, alerts := hostLoadSourceResults(t, now, append(ordered, hostLoadSourceRows(now, peer, nil)...))
			requireHostLoadSourceClasses(t, findings, alerts, fixture.host, true, "host-cpu-saturation")
		}
	}
}

// A bad enabled host must not erase an independently complete sibling alert.
func TestHostLoadSourcesInvalidHostDoesNotEraseSiblingPositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 5, 0, 0, time.UTC)
	fixture := hostLoadFixture{host: "compute-a.example.test", load1: 64, cores: 16, cpuExecution: 0.96, ioWait: 0.01, memoryAvailableRatio: 0.7}
	peer := fixture
	peer.host = "compute-b.example.test"
	for _, value := range []string{"NaN", "+Inf", "-1", "invalid"} {
		rows := hostLoadSourceRows(now, fixture, nil)
		rows[0].Value[1] = value
		rows = append(rows, hostLoadSourceRows(now, peer, nil)...)
		findings, alerts := hostLoadSourceResults(t, now, rows)
		requireHostLoadSourceClasses(t, findings, alerts, fixture.host, true)
		requireHostLoadSourceClasses(t, findings, alerts, peer.host, false, "host-cpu-saturation")
	}
}

// The owning Fluent Bit publisher supplies env/host/job without requiring an
// instance. Consistent optional source labels are equally valid.
func TestHostLoadSourcesHealthyPublisherShapes(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 6, 0, 0, time.UTC)
	fixture := hostLoadFixture{host: "compute-a.example.test", load1: 2, cores: 16, cpuExecution: 0.2, ioWait: 0.01, memoryAvailableRatio: 0.7}
	peer := fixture
	peer.host = "compute-b.example.test"
	for _, labels := range []map[string]string{nil, {"instance": "synthetic-exporter"}, {"instance": "synthetic-exporter", "publisher": "synthetic-agent"}} {
		rows := append(hostLoadSourceRows(now, fixture, labels), hostLoadSourceRows(now, peer, labels)...)
		findings, alerts := hostLoadSourceResults(t, now, rows)
		if len(alerts) != 0 || len(findings) != 8 {
			t.Fatalf("complete healthy publisher shape failed: findings=%d alerts=%d", len(findings), len(alerts))
		}
		for _, f := range findings {
			if !f.healthy {
				t.Fatalf("complete healthy source produced %s", f.class)
			}
		}
	}
}

// An empty successful vector retains every enabled host in the denominator.
func TestHostLoadSourcesAbsentResponseStaysUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 7, 0, 0, time.UTC)
	findings, alerts := hostLoadSourceResults(t, now, []hostLoadSourceRow{})
	for _, host := range []string{"compute-a.example.test", "compute-b.example.test"} {
		requireHostLoadSourceClasses(t, findings, alerts, host, true)
	}
}

// These query groupings preserve instance and every other publisher dimension;
// the reducer may remove only the metric-specific name and I/O-wait mode.
func TestHostLoadQueryPreservesPublisherIdentity(t *testing.T) {
	query := hostLoadQuery("synthetic")
	for _, fragment := range []string{
		`label_replace((node_load1{env="synthetic",job="node"}`,
		`count without(cpu) (count without(mode)`,
		`avg without(cpu,mode)`,
		`label_replace(avg without(cpu)`,
		`node_memory_MemAvailable_bytes{env="synthetic",job="node"}`,
		`node_memory_MemTotal_bytes{env="synthetic",job="node"}`,
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("host-load query lost source-preserving expression %q", fragment)
		}
	}
	if strings.Contains(query, " by (") || strings.Contains(query, "by(") || strings.Contains(query, "instance") {
		t.Fatal("host-load query collapsed or required a publisher label")
	}
}
