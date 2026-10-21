// Capacity authority is tested through the real instant-response parser and
// public signal, including the private healthy findings that clear incidents.
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

// Synthetic wire rows preserve arbitrary publisher dimensions without deriving
// expectations from the production capacity reducer.
type serviceLoadCapacityRow struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

// Reuse the existing process-family fixture, retaining no implicit host count.
func serviceLoadCapacityProcessRows(t testing.TB, now time.Time, fixtures ...serviceLoadFixture) []serviceLoadCapacityRow {
	t.Helper()
	var response struct {
		Data struct {
			Result []serviceLoadCapacityRow `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(serviceLoadFixtureJSON(t, now, nil, fixtures...)), &response); err != nil {
		t.Fatal(err)
	}
	return response.Data.Result
}

// The owning publisher permits env/host/job without an instance label.
func serviceLoadCapacityNodeRow(now time.Time, host, instance, value string) serviceLoadCapacityRow {
	labels := map[string]string{"env": "synthetic", "host": host, "job": "node", "monitor_metric": "host_cores"}
	if instance != "" {
		labels["instance"] = instance
	}
	return serviceLoadCapacityRow{Metric: labels, Value: []any{float64(now.Unix()), value}}
}

// Only local synthetic source callbacks execute; the generated command is
// checked by the existing settings fixture and never sent to a metrics service.
func serviceLoadCapacityResults(t testing.TB, now time.Time, rows []serviceLoadCapacityRow) ([]finding, Alerts) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"status": "success", "data": map[string]any{"resultType": "vector", "result": rows},
	})
	if err != nil {
		t.Fatal(err)
	}
	settings := serviceLoadSettings(t, now, string(payload))
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (serviceLoadProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewServiceLoadSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return findings, alerts
}

// Rotate and reverse every row boundary, including opposite publisher orders.
func serviceLoadCapacityOrders(rows []serviceLoadCapacityRow) [][]serviceLoadCapacityRow {
	orders := make([][]serviceLoadCapacityRow, 0, 2*len(rows))
	for offset := range rows {
		for _, direction := range []int{1, -1} {
			ordered := make([]serviceLoadCapacityRow, len(rows))
			for i := range rows {
				ordered[i] = rows[(offset+direction*i+len(rows))%len(rows)]
			}
			orders = append(orders, ordered)
		}
	}
	return orders
}

// The complete process has enough RSS for a CPU-dependent page at one core,
// but its other measurements are below all independent pressure thresholds.
func serviceLoadCapacityProcess(now time.Time) serviceLoadFixture {
	return serviceLoadFixture{
		host: "compute-a.example.test", service: "relay", block: "g1", instance: "synthetic-process",
		rss: 20 * 1024 * 1024 * 1024, heap: 10 * 1024 * 1024 * 1024,
		objects: 1_000_000, goroutines: 100_000, start: float64(now.Add(-time.Hour).Unix()),
		cpu: 2, allocation: 1024 * 1024, gc: 0.01,
	}
}

// Host-capacity visibility has a host target; process completeness and pressure
// keep their existing host/service/block target and process generation frame.
func requireServiceLoadCapacityUnknown(t testing.TB, findings []finding, alerts Alerts, host string, status string, count int, positive bool) {
	t.Helper()
	coverage := 0
	runaway := 0
	for _, alert := range alerts {
		if alert.Class == "service-runtime-metrics-incomplete" && alert.Target == host {
			coverage++
			if !alert.unobservable || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
				!strings.Contains(alert.Observed, fmt.Sprintf("host_capacity_status=%s host_core_samples=%d", status, count)) {
				t.Fatalf("wrong capacity authority finding: %+v", alert)
			}
		}
		if alert.Class == "service-runtime-runaway" && strings.HasPrefix(alert.Target, host+"/") {
			runaway++
			if !strings.Contains(alert.Observed, "host_logical_cpus=unknown") ||
				!strings.Contains(alert.Observed, "cpu_threshold_upper_bound_cores=4.00") {
				t.Fatalf("unknown capacity was presented as measured: %s", alert.Observed)
			}
		}
	}
	if coverage != 1 || (positive && runaway != 1) || (!positive && runaway != 0) {
		t.Fatalf("wrong capacity coverage/positive counts: coverage=%d positives=%d want_positive=%t", coverage, runaway, positive)
	}
	for _, f := range findings {
		if f.class == "service-runtime-runaway" && f.healthy && strings.HasPrefix(f.target, host+"/") {
			t.Fatal("unknown node capacity published a healthy runtime clear")
		}
	}
}

// Opposite capacity publishers previously selected either a page or health by
// response order. Neither row has authority to choose the host denominator.
func TestServiceLoadCapacityConflictingPublishersStayUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)
	process := serviceLoadCapacityProcess(now)
	rows := []serviceLoadCapacityRow{
		serviceLoadCapacityNodeRow(now, process.host, "synthetic-large", "128"),
		serviceLoadCapacityNodeRow(now, process.host, "synthetic-small", "16"),
	}
	rows = append(rows, serviceLoadCapacityProcessRows(t, now, process)...)
	var firstFindings []finding
	var firstAlerts Alerts
	for index, ordered := range serviceLoadCapacityOrders(rows) {
		findings, alerts := serviceLoadCapacityResults(t, now, ordered)
		for _, alert := range alerts {
			if alert.Class == "service-runtime-runaway" {
				t.Fatalf("ambiguous node capacity selected a CPU-dependent PAGE in row order %d", index)
			}
		}
		requireServiceLoadCapacityUnknown(t, findings, alerts, process.host, "ambiguous", 2, false)
		if index == 0 {
			firstFindings, firstAlerts = findings, alerts
		} else if !reflect.DeepEqual(findings, firstFindings) || !reflect.DeepEqual(alerts, firstAlerts) {
			t.Fatalf("node capacity results changed with row order %d", index)
		}
	}
}

// Repeated identical rows, equal-count publishers and extra publisher dimensions
// all remain ambiguous; agreeing values do not prove one authoritative source.
func TestServiceLoadCapacityDuplicateRowsStayUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 1, 0, 0, time.UTC)
	process := serviceLoadCapacityProcess(now)
	for _, label := range []string{"", "instance", "publisher"} {
		first := serviceLoadCapacityNodeRow(now, process.host, "", "16")
		second := serviceLoadCapacityNodeRow(now, process.host, "", "16")
		if label != "" {
			first.Metric[label], second.Metric[label] = "synthetic-a", "synthetic-b"
		}
		rows := append([]serviceLoadCapacityRow{first, second}, serviceLoadCapacityProcessRows(t, now, process)...)
		for _, ordered := range serviceLoadCapacityOrders(rows) {
			findings, alerts := serviceLoadCapacityResults(t, now, ordered)
			requireServiceLoadCapacityUnknown(t, findings, alerts, process.host, "ambiguous", 2, false)
		}
	}
}

// Invalid source rows remain part of host coverage even beside a valid row;
// parse failures cannot erase independent runtime pressure by aborting the probe.
func TestServiceLoadCapacityInvalidRowsPreserveIndependentPositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 2, 0, 0, time.UTC)
	process := serviceLoadCapacityProcess(now)
	process.cpu = 4
	for _, mutate := range []func(*serviceLoadCapacityRow){
		func(row *serviceLoadCapacityRow) { row.Value[1] = "NaN" },
		func(row *serviceLoadCapacityRow) { row.Value[1] = "+Inf" },
		func(row *serviceLoadCapacityRow) { row.Value[1] = "-Inf" },
		func(row *serviceLoadCapacityRow) { row.Value[1] = "-1" },
		func(row *serviceLoadCapacityRow) { row.Value[1] = "0" },
		func(row *serviceLoadCapacityRow) { row.Value[1] = "0.5" },
		func(row *serviceLoadCapacityRow) { row.Value[1] = "9007199254740992" },
		func(row *serviceLoadCapacityRow) { row.Value[1] = "synthetic-private-value" },
		func(row *serviceLoadCapacityRow) { row.Value = nil },
		func(row *serviceLoadCapacityRow) { row.Value[0] = "synthetic-private-clock" },
		func(row *serviceLoadCapacityRow) { row.Value[0] = float64(now.Add(-91 * time.Second).Unix()) },
		func(row *serviceLoadCapacityRow) { row.Value[0] = float64(now.Add(31 * time.Second).Unix()) },
		func(row *serviceLoadCapacityRow) { row.Metric["env"] = "synthetic-other-env" },
		func(row *serviceLoadCapacityRow) { row.Metric["job"] = "synthetic-other-job" },
		func(row *serviceLoadCapacityRow) { delete(row.Metric, "env") },
		func(row *serviceLoadCapacityRow) { delete(row.Metric, "job") },
	} {
		for _, withValid := range []bool{false, true} {
			invalid := serviceLoadCapacityNodeRow(now, process.host, "synthetic-invalid-publisher", "16")
			mutate(&invalid)
			rows := []serviceLoadCapacityRow{invalid}
			status := "invalid"
			if withValid {
				rows = append(rows, serviceLoadCapacityNodeRow(now, process.host, "synthetic-valid-publisher", "16"))
				status = "ambiguous"
			}
			count := len(rows)
			rows = append(rows, serviceLoadCapacityProcessRows(t, now, process)...)
			var firstFindings []finding
			var firstAlerts Alerts
			for index, ordered := range serviceLoadCapacityOrders(rows) {
				findings, alerts := serviceLoadCapacityResults(t, now, ordered)
				requireServiceLoadCapacityUnknown(t, findings, alerts, process.host, status, count, true)
				for _, alert := range alerts {
					requireAlertOmits(t, alert, "synthetic-private-value", "synthetic-private-clock", "synthetic-invalid-publisher", "synthetic-valid-publisher", "synthetic-other-env", "synthetic-other-job", "host_logical_cpus=64")
				}
				if index == 0 {
					firstFindings, firstAlerts = findings, alerts
				} else if !reflect.DeepEqual(findings, firstFindings) || !reflect.DeepEqual(alerts, firstAlerts) {
					t.Fatalf("invalid capacity changed results with row order %d", index)
				}
			}
		}
	}
}

// Without capacity, the existing universal CPU ceiling and raw pressure
// predicates still prove positives; values below them stay unknown.
func TestServiceLoadCapacityMissingPreservesIndependentPredicates(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 3, 0, 0, time.UTC)
	for _, c := range []struct {
		name     string
		mutate   func(*serviceLoadFixture)
		positive bool
	}{
		{"below-upper-cpu", func(f *serviceLoadFixture) { f.cpu = 3.999 }, false},
		{"at-upper-cpu", func(f *serviceLoadFixture) { f.cpu = 4 }, true},
		{"cpu-missing", func(f *serviceLoadFixture) { f.omit = "cpu" }, false},
		{"rss-hard", func(f *serviceLoadFixture) {
			f.rss = 64 * 1024 * 1024 * 1024
			f.omit = "cpu"
		}, true},
		{"goroutine-floor", func(f *serviceLoadFixture) { f.goroutines = 250_000 }, true},
		{"goroutine-hard", func(f *serviceLoadFixture) {
			f.rss = 1024
			f.goroutines = 500_000
		}, true},
		{"allocation-floor", func(f *serviceLoadFixture) { f.allocation = 256 * 1024 * 1024 }, true},
		{"allocation-hard", func(f *serviceLoadFixture) {
			f.rss = 1024
			f.allocation = 1024 * 1024 * 1024
		}, true},
	} {
		process := serviceLoadCapacityProcess(now)
		c.mutate(&process)
		findings, alerts := serviceLoadCapacityResults(t, now, serviceLoadCapacityProcessRows(t, now, process))
		requireServiceLoadCapacityUnknown(t, findings, alerts, process.host, "missing", 0, c.positive)
		if c.positive {
			alert := requireAlertClass(t, alerts, "service-runtime-runaway")
			if alert.Frame != process.instance || alert.Severity != SeverityPage || alert.Sustain != 2 {
				t.Fatalf("%s: independent positive lost its existing identity", c.name)
			}
		}
	}
}

// Unknown capacity on one host cannot erase a sibling's uniquely qualified
// CPU-dependent positive or borrow the sibling's capacity for the unknown host.
func TestServiceLoadCapacityPartialHostsStayIndependent(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 4, 0, 0, time.UTC)
	first := serviceLoadCapacityProcess(now)
	second := first
	second.host, second.instance = "compute-b.example.test", "synthetic-peer-process"
	rows := []serviceLoadCapacityRow{serviceLoadCapacityNodeRow(now, second.host, "", "16")}
	rows = append(rows, serviceLoadCapacityProcessRows(t, now, first, second)...)
	for _, ordered := range serviceLoadCapacityOrders(rows) {
		findings, alerts := serviceLoadCapacityResults(t, now, ordered)
		requireServiceLoadCapacityUnknown(t, findings, alerts, first.host, "missing", 0, false)
		positive := requireAlertClass(t, alerts, "service-runtime-runaway")
		if positive.Target != second.host+"/relay/g1" || positive.Frame != second.instance ||
			!strings.Contains(positive.Observed, "host_logical_cpus=16 host_capacity_status=valid") {
			t.Fatalf("partial host coverage changed the independent positive: %+v", positive)
		}
	}
}

// Missing node capacity does not select only the newest process or suppress an
// independently oversized draining generation in the same block.
func TestServiceLoadCapacityUnknownRetainsDrainingGeneration(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 5, 0, 0, time.UTC)
	draining := serviceLoadCapacityProcess(now)
	draining.instance = "synthetic-draining"
	draining.rss = 70 * 1024 * 1024 * 1024
	current := draining
	current.instance = "synthetic-current"
	current.start = float64(now.Add(-time.Minute).Unix())
	current.rss, current.cpu = 2*1024*1024*1024, 0.2
	rows := serviceLoadCapacityProcessRows(t, now, draining, current)
	for _, ordered := range serviceLoadCapacityOrders(rows) {
		findings, alerts := serviceLoadCapacityResults(t, now, ordered)
		requireServiceLoadCapacityUnknown(t, findings, alerts, draining.host, "missing", 0, true)
		positive := requireAlertClass(t, alerts, "service-runtime-runaway")
		if positive.Frame != draining.instance || !strings.Contains(positive.Observed, "same_block_generations=2") {
			t.Fatalf("unknown host capacity lost the draining generation: %+v", positive)
		}
	}
}

// Complete capacity controls reproduce the real 1-to-4-core threshold range
// and accept both owning no-instance publishers and optional extra labels.
func TestServiceLoadCapacityUniquePublisherControlsAndRecovery(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 5, 0, 0, time.UTC)
	process := serviceLoadCapacityProcess(now)
	for _, c := range []struct {
		cores    string
		cpu      float64
		positive bool
	}{
		{"8", 0.999, false}, {"8", 1, true},
		{"32", 1.999, false}, {"32", 2, true},
		{"128", 3.999, false}, {"128", 4, true},
	} {
		for _, instance := range []string{"", "synthetic-exporter"} {
			for _, age := range []time.Duration{0, 90 * time.Second, -30 * time.Second} {
				candidate := process
				candidate.cpu = c.cpu
				node := serviceLoadCapacityNodeRow(now.Add(-age), candidate.host, instance, c.cores)
				node.Metric["publisher"] = "synthetic-private-publisher"
				rows := append([]serviceLoadCapacityRow{node}, serviceLoadCapacityProcessRows(t, now, candidate)...)
				findings, alerts := serviceLoadCapacityResults(t, now, rows)
				if c.positive {
					alert := requireAlertClass(t, alerts, "service-runtime-runaway")
					if len(alerts) != 1 || !strings.Contains(alert.Observed, "host_logical_cpus="+c.cores+" host_capacity_status=valid") {
						t.Fatalf("qualified capacity control failed: %+v", alerts)
					}
					requireAlertOmits(t, alert, "synthetic-private-publisher", "synthetic-exporter")
				} else if len(alerts) != 0 {
					t.Fatalf("qualified healthy capacity control alerted: %+v", alerts)
				}
				capacityHealthy := false
				runtimeHealthy := false
				for _, f := range findings {
					capacityHealthy = capacityHealthy || (f.healthy && f.class == "service-runtime-metrics-incomplete" && f.target == candidate.host)
					runtimeHealthy = runtimeHealthy || (f.healthy && f.class == "service-runtime-runaway" && f.target == candidate.host+"/relay/g1")
				}
				if !capacityHealthy || runtimeHealthy == c.positive {
					t.Fatal("qualified capacity did not restore the correct healthy findings")
				}
			}
		}
	}
}

// An unowned malformed node row cannot abort a complete enabled-host finding.
func TestServiceLoadCapacityUnownedRowCannotErasePositive(t *testing.T) {
	now := time.Date(2026, 10, 19, 16, 6, 0, 0, time.UTC)
	process := serviceLoadCapacityProcess(now)
	rows := []serviceLoadCapacityRow{
		serviceLoadCapacityNodeRow(now, process.host, "", "16"),
		serviceLoadCapacityNodeRow(now, "unowned.example.test", "", "synthetic-private-value"),
	}
	rows = append(rows, serviceLoadCapacityProcessRows(t, now, process)...)
	_, alerts := serviceLoadCapacityResults(t, now, rows)
	if len(alerts) != 1 || alerts[0].Class != "service-runtime-runaway" {
		t.Fatalf("unowned node row erased the complete positive: %+v", alerts)
	}
	requireAlertOmits(t, alerts[0], "unowned.example.test", "synthetic-private-value")
}
