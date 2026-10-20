// Synthetic vectors exercise numeric, duplicate, cohort and source-clock
// authority through the real worker probes and public signal adapters.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A test-owned wire shape keeps causal controls independent of new reducers.
type workerAuthoritySeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

// Warnings are deliberately retained without using the production response type.
type workerAuthorityVector struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings,omitempty"`
	Data     struct {
		ResultType string                  `json:"resultType"`
		Result     []workerAuthoritySeries `json:"result"`
	} `json:"data"`
}

// Changes a wire response, not an implementation-owned observation structure.
func workerAuthorityAlter(t *testing.T, payload string, alter func(*workerAuthorityVector)) string {
	t.Helper()
	var vector workerAuthorityVector
	if err := json.Unmarshal([]byte(payload), &vector); err != nil {
		t.Fatal(err)
	}
	alter(&vector)
	encoded, err := json.Marshal(vector)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Three quiet controls bound one hot worker even if the fifth worker is unknown.
func workerAuthorityFleet() []workerMetricFixture {
	return []workerMetricFixture{
		{host: "worker-a.example", block: "g1", instance: "runtime-a", heap: 128 * 1024 * 1024, cpuRate: 0.02, allocRate: 1024 * 1024},
		{host: "worker-b.example", block: "g1", instance: "runtime-b", heap: 256 * 1024 * 1024, cpuRate: 0.04, allocRate: 2 * 1024 * 1024},
		{host: "worker-c.example", block: "g1", instance: "runtime-c", heap: 512 * 1024 * 1024, cpuRate: 0.06, allocRate: 4 * 1024 * 1024},
		{host: "worker-hot.example", block: "g1", instance: "runtime-hot", heap: 32 * 1024 * 1024 * 1024, cpuRate: 4, allocRate: 650 * 1024 * 1024},
		{host: "worker-suspect.example", block: "g1", instance: "runtime-suspect", heap: 32 * 1024 * 1024 * 1024, cpuRate: 4, allocRate: 650 * 1024 * 1024},
	}
}

// Every transport is synthetic; no logs or Redis enrichment is needed here.
func workerAuthoritySettings(now time.Time, memory bool, primary, rates string) SignalSettings {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if memory && strings.Contains(command, "monitor_rate") {
				return rates, nil
			}
			return primary, nil
		},
		localFn: func(string, ...string) (string, error) { return "", nil },
	}
	return workerMemorySyntheticSettings(source, now)
}

// Both the reducer's explicit healthy controls and the public adapter are checked.
func workerAuthorityObserve(t *testing.T, memory bool, settings SignalSettings) ([]finding, Alerts) {
	t.Helper()
	signal := NewWorkerChurnSignal()
	var workerProbe probe = workerChurnProbe{}
	if memory {
		signal, workerProbe = NewWorkerMemorySignal(), workerMemoryProbe{}
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := workerProbe.check(context.Background(), env)
	if err != nil {
		t.Fatalf("synthetic worker reducer returned an error: %v", err)
	}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("synthetic worker adapter returned an error: %v", err)
	}
	return findings, alerts
}

// A rejected runtime must neither disappear nor become a measured diagnosis.
func workerAuthorityRequireUnknown(t *testing.T, findings []finding, alerts Alerts, memory bool, rejected string) {
	t.Helper()
	class := "worker-cpu-allocation-churn"
	if memory {
		class = "worker-memory-skew"
	}
	healthy, positive, visibility := false, false, false
	for _, observed := range findings {
		if observed.class == class && observed.target == rejected {
			t.Fatalf("rejected runtime became a measurement: %+v", observed)
		}
		if observed.class == class && observed.target == "worker-a.example/g1" && observed.healthy {
			healthy = true
		}
		if observed.class == class && observed.target == "worker-hot.example/g1" && !observed.healthy {
			positive = true
		}
		if observed.isUnobservable() && strings.Contains(observed.observed, "incomplete_runtimes=1") {
			visibility = true
		}
	}
	if !healthy || !positive || !visibility {
		t.Fatalf("independent controls or rejected coverage lost: healthy=%t positive=%t visibility=%t", healthy, positive, visibility)
	}
	if requireAlertClassCount(alerts, class) != 1 || requireAlertClassCount(alerts, "cannot-observe") != 1 {
		t.Fatalf("public worker classifications changed: %+v", alerts)
	}
	if !requireAlertClass(t, alerts, "cannot-observe").unobservable {
		t.Fatal("partial evidence can trigger absence-based recovery")
	}
}

// Invalid numeric inputs used to become false churn or disappear as healthy.
func TestWorkerMetricAuthorityChurnRejectsInvalidScalars(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	base := workerChurnRatesFixtureJSON(t, now, workerAuthorityFleet()...)
	for _, kind := range []string{"cpu", "alloc", "cpu_source_time", "alloc_source_time"} {
		for _, invalid := range []any{"NaN", "+Inf", "-Inf", "-1", "1e999", nil, "synthetic-private-scalar"} {
			payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
				for i := range vector.Data.Result {
					series := &vector.Data.Result[i]
					if series.Metric["instance"] == "runtime-suspect" && series.Metric["monitor_rate"] == kind {
						series.Value[1] = invalid
					}
				}
			})
			findings, alerts := workerAuthorityObserve(t, false, workerAuthoritySettings(now, false, payload, ""))
			workerAuthorityRequireUnknown(t, findings, alerts, false, "worker-suspect.example/g1")
			for _, alert := range alerts {
				requireAlertOmits(t, alert, "NaN", "+Inf", "-Inf", "synthetic-private-scalar")
			}
		}
	}
}

// Heap NaN previously vanished from the cohort; infinity became a heap warning.
func TestWorkerMetricAuthorityMemoryRejectsInvalidHeap(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()
	base := workerMetricsFixtureJSON(t, now, workers...)
	rates := workerRatesFixtureJSON(t, now, workers...)
	for _, invalid := range []any{"NaN", "+Inf", "-Inf", "-1", "1e999", nil, "synthetic-private-heap"} {
		payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
			for i := range vector.Data.Result {
				series := &vector.Data.Result[i]
				if series.Metric["instance"] == "runtime-suspect" && series.Metric["__name__"] == "go_memstats_heap_alloc_bytes" {
					series.Value[1] = invalid
				}
			}
		})
		findings, alerts := workerAuthorityObserve(t, true, workerAuthoritySettings(now, true, payload, rates))
		workerAuthorityRequireUnknown(t, findings, alerts, true, "worker-suspect.example/g1")
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "NaN", "+Inf", "-Inf", "synthetic-private-heap")
		}
	}
}

// Identical, conflicting and invalid duplicates are ambiguous in both orders.
func TestWorkerMetricAuthorityDuplicateOrderCannotChooseMeasurement(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()
	rates := workerRatesFixtureJSON(t, now, workers...)
	for _, memory := range []bool{false, true} {
		base, label := workerChurnRatesFixtureJSON(t, now, workers...), "monitor_rate"
		kinds := []string{"cpu", "alloc", "cpu_source_time", "alloc_source_time"}
		if memory {
			base, label = workerMetricsFixtureJSON(t, now, workers...), "__name__"
			kinds = []string{"go_memstats_heap_alloc_bytes"}
		}
		for _, kind := range kinds {
			for _, duplicateValue := range []string{"same", "0", "NaN"} {
				for _, reverse := range []bool{false, true} {
					payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
						for _, series := range vector.Data.Result {
							if series.Metric["instance"] == "runtime-suspect" && series.Metric[label] == kind {
								duplicate := workerAuthoritySeries{Metric: series.Metric, Value: append([]any(nil), series.Value...)}
								if duplicateValue != "same" {
									duplicate.Value[1] = duplicateValue
								}
								vector.Data.Result = append(vector.Data.Result, duplicate)
								break
							}
						}
						if reverse {
							for i, j := 0, len(vector.Data.Result)-1; i < j; i, j = i+1, j-1 {
								vector.Data.Result[i], vector.Data.Result[j] = vector.Data.Result[j], vector.Data.Result[i]
							}
						}
					})
					findings, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, payload, rates))
					workerAuthorityRequireUnknown(t, findings, alerts, memory, "worker-suspect.example/g1")
				}
			}
		}
	}
}

// Ancillary malformed scalars remain unknown without suppressing valid heap.
func TestWorkerMetricAuthorityMemoryAncillaryScalarCannotEraseHeap(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:4]
	base, rates := workerMetricsFixtureJSON(t, now, workers...), workerRatesFixtureJSON(t, now, workers...)
	for _, metric := range []struct {
		kind  string
		field string
	}{
		{kind: "go_memstats_heap_objects", field: "heap_objects"},
		{kind: "process_resident_memory_bytes", field: "rss_bytes"},
		{kind: "go_memstats_alloc_bytes_total", field: "alloc_total_bytes"},
		{kind: "go_gc_duration_seconds_count", field: "gc_cycles"},
		{kind: "process_start_time_seconds", field: "process_age_s"},
	} {
		for _, mode := range []string{"invalid", "duplicate", "missing"} {
			payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
				for i, series := range vector.Data.Result {
					if series.Metric["instance"] != "runtime-hot" || series.Metric["__name__"] != metric.kind {
						continue
					}
					switch mode {
					case "invalid":
						vector.Data.Result[i].Value[1] = "NaN"
					case "duplicate":
						vector.Data.Result = append(vector.Data.Result, series)
					case "missing":
						vector.Data.Result = append(vector.Data.Result[:i], vector.Data.Result[i+1:]...)
					}
					break
				}
			})
			_, alerts := workerAuthorityObserve(t, true, workerAuthoritySettings(now, true, payload, rates))
			heap := requireAlertClass(t, alerts, "worker-memory-skew")
			if heap.Target != "worker-hot.example/g1" || !strings.Contains(heap.Observed, metric.field+"=unknown") {
				t.Fatalf("%s %s: heap or scalar authority changed: %+v", metric.kind, mode, heap)
			}
			requireAlertClass(t, alerts, "cannot-observe")
			requireAlertOmits(t, heap, "NaN", "+Inf")
		}
	}
}

// Unknown runtime heaps/rates can move a median enough to reverse apparent skew.
func TestWorkerMetricAuthorityPartialCohortCannotManufactureSkew(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	baseWorkers := workerAuthorityFleet()
	workers := append([]workerMetricFixture(nil), baseWorkers[:2]...)
	workers = append(workers, baseWorkers[3])
	for i := 0; i < 3; i++ {
		workers = append(workers, workerMetricFixture{host: fmt.Sprintf("missing-%d.example", i), block: "g1", instance: fmt.Sprintf("missing-%d", i)})
	}
	rates := workerRatesFixtureJSON(t, now, workers...)
	for _, memory := range []bool{false, true} {
		base := workerChurnRatesFixtureJSON(t, now, workers...)
		if memory {
			base = workerMetricsFixtureJSON(t, now, workers...)
		}
		payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
			retained := []workerAuthoritySeries{}
			for _, series := range vector.Data.Result {
				missing := strings.HasPrefix(series.Metric["instance"], "missing-")
				keep := !missing || (memory && series.Metric["__name__"] != "go_memstats_heap_alloc_bytes") ||
					(!memory && strings.HasSuffix(series.Metric["monitor_rate"], "_source_time"))
				if keep {
					retained = append(retained, series)
				}
			}
			vector.Data.Result = retained
		})
		findings, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, payload, rates))
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !strings.Contains(alerts[0].Observed, "indeterminate_runtimes=1") {
			t.Fatalf("partial cohort manufactured a measured fleet verdict: %+v", alerts)
		}
		for _, observed := range findings {
			if observed.target == "worker-hot.example/g1" {
				t.Fatalf("undetermined hot worker was measured healthy or broken: %+v", observed)
			}
		}
	}
}

// A missing allocation rate must not discard that worker's valid high CPU rate.
func TestWorkerMetricAuthorityChurnKeepsUnpairedCpuInBaseline(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	fleet := workerAuthorityFleet()
	workers := []workerMetricFixture{fleet[0], fleet[1], fleet[3], fleet[4]}
	payload := workerAuthorityAlter(t, workerChurnRatesFixtureJSON(t, now, workers...), func(vector *workerAuthorityVector) {
		for i, series := range vector.Data.Result {
			if series.Metric["instance"] == "runtime-suspect" && series.Metric["monitor_rate"] == "alloc" {
				vector.Data.Result = append(vector.Data.Result[:i], vector.Data.Result[i+1:]...)
				break
			}
		}
	})
	findings, alerts := workerAuthorityObserve(t, false, workerAuthoritySettings(now, false, payload, ""))
	if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
		t.Fatalf("dropping unpaired CPU manufactured churn: %+v", alerts)
	}
	healthy := false
	for _, observed := range findings {
		if observed.target == "worker-hot.example/g1" && observed.healthy {
			healthy = true
		}
	}
	if !healthy {
		t.Fatal("complete CPU comparison did not preserve independently healthy worker")
	}
}

// A warning may hide an unknown number of runtimes, beyond finite median bounds.
func TestWorkerMetricAuthorityWarningsAndUnsafeLabelsRemainPrivateUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:4]
	rates := workerRatesFixtureJSON(t, now, workers...)
	for _, memory := range []bool{false, true} {
		base := workerChurnRatesFixtureJSON(t, now, workers...)
		if memory {
			base = workerMetricsFixtureJSON(t, now, workers...)
		}
		for _, kind := range []string{"warning", "identity"} {
			payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
				if kind == "warning" {
					vector.Warnings = []string{"synthetic-private-warning"}
				} else {
					for i := range vector.Data.Result {
						series := &vector.Data.Result[i]
						if (memory && series.Metric["__name__"] == "go_memstats_heap_alloc_bytes") ||
							(!memory && series.Metric["monitor_rate"] == "cpu") {
							series.Metric["host"] = "synthetic-private-label\nunsafe"
							break
						}
					}
				}
			})
			_, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, payload, rates))
			if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable {
				t.Fatalf("unbounded omitted runtime count became a fleet measurement: %+v", alerts)
			}
			encoded, err := json.Marshal(alerts)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"synthetic-private-warning", "synthetic-private-label"} {
				if strings.Contains(string(encoded), private) || strings.Contains(alerts[0].Markdown(), private) {
					t.Fatal("raw warning or rejected label reached public output")
				}
			}
		}
	}
}

// Fresh evaluation timestamps do not override stale or future source timestamps.
func TestWorkerMetricAuthoritySourceClockAndEvaluationBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:4]
	for _, memory := range []bool{false, true} {
		for _, age := range []time.Duration{90 * time.Second, 91 * time.Second, -30 * time.Second, -31 * time.Second} {
			base := workerChurnRatesFixtureJSON(t, now, workers...)
			if memory {
				base = workerMetricsFixtureJSON(t, now, workers...)
			}
			payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
				for i := range vector.Data.Result {
					series := &vector.Data.Result[i]
					if series.Metric["instance"] != "runtime-hot" {
						continue
					}
					if memory && series.Metric["monitor_memory_source"] == "go_memstats_heap_alloc_bytes" {
						series.Value[1] = fmt.Sprintf("%d", now.Add(-age).Unix())
					} else if !memory && strings.HasSuffix(series.Metric["monitor_rate"], "_source_time") {
						series.Value[1] = fmt.Sprintf("%d", now.Add(-age).Unix())
					}
				}
			})
			_, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, payload, workerRatesFixtureJSON(t, now, workers...)))
			class := "worker-cpu-allocation-churn"
			if memory {
				class = "worker-memory-skew"
			}
			accepted := age == 90*time.Second || age == -30*time.Second
			if accepted && (len(alerts) != 1 || alerts[0].Class != class) {
				t.Fatalf("inclusive freshness boundary rejected: memory=%t age=%s alerts=%+v", memory, age, alerts)
			}
			if !accepted && (len(alerts) != 1 || alerts[0].Class != "cannot-observe") {
				t.Fatalf("invalid source clock became measured: memory=%t age=%s alerts=%+v", memory, age, alerts)
			}
		}
	}
}

// Null and overflowing evaluation timestamps must not be coerced into evidence.
func TestWorkerMetricAuthorityMalformedTimestampPreservesSibling(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()
	rates := workerRatesFixtureJSON(t, now, workers...)
	for _, memory := range []bool{false, true} {
		base := workerChurnRatesFixtureJSON(t, now, workers...)
		if memory {
			base = workerMetricsFixtureJSON(t, now, workers...)
		}
		for _, timestamp := range []any{nil, -1, 0, math.MaxFloat64, "synthetic-private-clock"} {
			payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
				for i := range vector.Data.Result {
					series := &vector.Data.Result[i]
					if series.Metric["instance"] == "runtime-suspect" {
						series.Value[0] = timestamp
					}
				}
			})
			findings, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, payload, rates))
			workerAuthorityRequireUnknown(t, findings, alerts, memory, "worker-suspect.example/g1")
		}
	}
}

// Malformed tuples are per-scalar defects, not a reason to discard a valid peer.
func TestWorkerMetricAuthorityMalformedTuplePreservesSibling(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()
	rates := workerRatesFixtureJSON(t, now, workers...)
	for _, memory := range []bool{false, true} {
		base := workerChurnRatesFixtureJSON(t, now, workers...)
		if memory {
			base = workerMetricsFixtureJSON(t, now, workers...)
		}
		for _, value := range [][]any{nil, {}, {float64(now.Unix())}, {float64(now.Unix()), "1", "extra"}, {float64(now.Unix()), map[string]string{"private": "synthetic-private-shape"}}} {
			payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
				for i := range vector.Data.Result {
					series := &vector.Data.Result[i]
					if series.Metric["instance"] == "runtime-suspect" {
						series.Value = value
					}
				}
			})
			findings, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, payload, rates))
			workerAuthorityRequireUnknown(t, findings, alerts, memory, "worker-suspect.example/g1")
		}
	}
}

// No usable runtime evidence is a coverage failure, never an empty healthy run.
func TestWorkerMetricAuthorityEmptyAndRejectedOnlyAreUnknown(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:1]
	rates := workerRatesFixtureJSON(t, now, workers...)
	for _, memory := range []bool{false, true} {
		base := workerChurnRatesFixtureJSON(t, now, workers...)
		if memory {
			base = workerMetricsFixtureJSON(t, now, workers...)
		}
		for _, empty := range []bool{false, true} {
			payload := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
				if empty {
					vector.Data.Result = []workerAuthoritySeries{}
					return
				}
				for i := range vector.Data.Result {
					vector.Data.Result[i].Value[1] = "NaN"
				}
			})
			findings, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, payload, rates))
			if len(findings) != 1 || findings[0].healthy || len(alerts) != 1 || !alerts[0].unobservable {
				t.Fatalf("absent authority became healthy: memory=%t empty=%t", memory, empty)
			}
		}
	}
}

// Missing heaps must not turn three returned runtimes into a one-worker fleet.
func TestWorkerMetricAuthoritySparseGuardKeepsRejectedRuntimeCount(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:3]
	workers[0].heap = 10 * 1024 * 1024 * 1024
	payload := workerAuthorityAlter(t, workerMetricsFixtureJSON(t, now, workers...), func(vector *workerAuthorityVector) {
		retained := []workerAuthoritySeries{}
		for _, series := range vector.Data.Result {
			if series.Metric["instance"] == "runtime-a" || series.Metric["__name__"] != "go_memstats_heap_alloc_bytes" {
				retained = append(retained, series)
			}
		}
		vector.Data.Result = retained
	})
	findings, alerts := workerAuthorityObserve(t, true, workerAuthoritySettings(now, true, payload, workerRatesFixtureJSON(t, now, workers...)))
	if len(findings) != 1 || findings[0].healthy || len(alerts) != 1 || alerts[0].Class != "cannot-observe" ||
		!strings.Contains(alerts[0].Observed, "returned_runtimes=3 incomplete_runtimes=2 indeterminate_runtimes=1") {
		t.Fatalf("missing heaps changed sparse policy into false recovery: %+v", findings)
	}
}

// Optional rate defects never erase independently measured heap pressure.
func TestWorkerMetricAuthorityOptionalRatesStayUnknownAndHeapSurvives(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:4]
	primary, base := workerMetricsFixtureJSON(t, now, workers...), workerRatesFixtureJSON(t, now, workers...)
	for _, kind := range []string{"cpu", "alloc", "gc", "gc_pause"} {
		for _, mode := range []string{"invalid", "duplicate", "missing", "stale-source"} {
			rates := workerAuthorityAlter(t, base, func(vector *workerAuthorityVector) {
				for i, series := range vector.Data.Result {
					wanted := kind
					if mode == "stale-source" {
						wanted += "_source_time"
					}
					if series.Metric["instance"] != "runtime-hot" || series.Metric["monitor_rate"] != wanted {
						continue
					}
					switch mode {
					case "invalid":
						vector.Data.Result[i].Value[1] = "NaN"
					case "duplicate":
						vector.Data.Result = append(vector.Data.Result, series)
					case "missing":
						vector.Data.Result = append(vector.Data.Result[:i], vector.Data.Result[i+1:]...)
					case "stale-source":
						vector.Data.Result[i].Value[1] = fmt.Sprintf("%d", now.Add(-91*time.Second).Unix())
					}
					break
				}
			})
			_, alerts := workerAuthorityObserve(t, true, workerAuthoritySettings(now, true, primary, rates))
			heap := requireAlertClass(t, alerts, "worker-memory-skew")
			if len(alerts) != 1 || !strings.Contains(heap.Evidence, "did not return every five-minute rate") {
				t.Fatalf("optional %s %s changed heap authority: %+v", kind, mode, alerts)
			}
			requireAlertOmits(t, heap, "cpu_cores_5m=", "NaN", "+Inf")
		}
	}
}

// Absolute guards and fleet skew remain independent, including a zero baseline.
func TestWorkerMetricAuthorityIdleMedianAndHealthyHotFleet(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	for _, memory := range []bool{false, true} {
		for _, idle := range []bool{false, true} {
			workers := workerAuthorityFleet()[:4]
			for i := 0; i < 3; i++ {
				workers[i].heap, workers[i].cpuRate, workers[i].allocRate = workers[3].heap, workers[3].cpuRate, workers[3].allocRate
				if idle {
					workers[i].heap, workers[i].cpuRate, workers[i].allocRate = 0, 0, 0
				}
			}
			rates := workerRatesFixtureJSON(t, now, workers...)
			primary := workerChurnRatesFixtureJSON(t, now, workers...)
			if memory {
				primary = workerMetricsFixtureJSON(t, now, workers...)
			}
			_, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, primary, rates))
			if !idle && len(alerts) != 0 {
				t.Fatalf("uniform useful work became skew: memory=%t alerts=%+v", memory, alerts)
			}
			if idle {
				if len(alerts) != 1 || alerts[0].Target != "worker-hot.example/g1" || !strings.Contains(alerts[0].Observed, "=unbounded") {
					t.Fatalf("zero median suppressed hot worker: memory=%t alerts=%+v", memory, alerts)
				}
				requireAlertOmits(t, alerts[0], "NaN", "+Inf")
			}
		}
	}
}

// Adding two individually finite counters must not overflow the even median.
func TestWorkerMetricAuthorityMedianAvoidsFiniteOverflow(t *testing.T) {
	for _, value := range []float64{math.MaxFloat64, math.SmallestNonzeroFloat64} {
		if got := medianFloat64([]float64{value, value}); got != value {
			t.Fatalf("finite median changed its equal inputs: input=%g median=%g", value, got)
		}
	}
}

// Inspect the actual issued PromQL, including both clock gates and the census
// companion; fake result timestamps alone cannot validate rate freshness.
func TestWorkerMetricAuthorityIssuedQueriesCarryCounterSourceClocks(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:3]
	for _, memory := range []bool{false, true} {
		queries := []string{}
		memoryQueries := []string{}
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			start := strings.Index(command, "?query=")
			if start < 0 {
				t.Fatal("worker did not issue an instant query")
			}
			encoded := strings.TrimSuffix(command[start+len("?query="):], "'")
			query, err := url.QueryUnescape(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(query, "monitor_rate") {
				queries = append(queries, query)
				if !memory {
					return workerChurnRatesFixtureJSON(t, now, workers...), nil
				}
				return workerRatesFixtureJSON(t, now, workers...), nil
			}
			memoryQueries = append(memoryQueries, query)
			return workerMetricsFixtureJSON(t, now, workers...), nil
		}}
		signal, window := NewWorkerChurnSignal(), "1m"
		if memory {
			signal, window = NewWorkerMemorySignal(), "5m"
		}
		if _, err := signal.Run(context.Background(), workerMemorySyntheticSettings(source, now)); err != nil {
			t.Fatal(err)
		}
		if len(queries) != 1 {
			t.Fatalf("unexpected number of rate queries: %d", len(queries))
		}
		if memory {
			if len(memoryQueries) != 1 {
				t.Fatalf("unexpected primary memory query count: %d", len(memoryQueries))
			}
			for _, metric := range []string{"go_memstats_heap_alloc_bytes", "go_memstats_heap_objects", "go_memstats_alloc_bytes_total",
				"go_gc_duration_seconds_count", "process_resident_memory_bytes", "process_start_time_seconds"} {
				selector := metric + `{env="synthetic",job="taskworker"}`
				for _, required := range []string{
					`label_replace((` + selector + " and (timestamp(" + selector + ") >= time() - 90) and (timestamp(" + selector + `) <= time() + 30)),"monitor_memory_metric","` + metric + `","job",".*")`,
					`label_replace(timestamp(` + selector + `),"monitor_memory_source","` + metric + `","job",".*")`,
				} {
					if !strings.Contains(memoryQueries[0], required) {
						t.Fatalf("primary memory family lacks its own source clock: %s", required)
					}
				}
			}
			if strings.Count(memoryQueries[0], "label_replace(") != 12 || strings.Count(memoryQueries[0], "timestamp(") != 18 {
				t.Fatal("primary memory source contract changed its fixed six-family shape")
			}
		}
		families := map[string]string{"cpu": "process_cpu_seconds_total", "alloc": "go_memstats_alloc_bytes_total"}
		if memory {
			families["gc"], families["gc_pause"] = "go_gc_duration_seconds_count", "go_gc_duration_seconds_sum"
		}
		for kind, metric := range families {
			selector := metric + `{env="synthetic",job="taskworker"}`
			for _, required := range []string{
				"rate(" + selector + "[" + window + "])",
				"timestamp(" + selector + ") >= time() - 90",
				"timestamp(" + selector + ") <= time() + 30",
				`label_replace(timestamp(` + selector + `),"monitor_rate","` + kind + `_source_time","job",".*")`,
			} {
				if !strings.Contains(queries[0], required) {
					t.Fatalf("worker rate query lacks source authority clause: %s", required)
				}
			}
		}
	}
}

// Drive only the notifier's deterministic state reducer; no sender or network
// is constructed. Held and mature coverage alerts must both prevent clears.
func workerAuthorityRecoveryControl(t *testing.T, memory bool) {
	t.Helper()
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:4]
	hotRates := workerRatesFixtureJSON(t, now, workers...)
	hot := workerChurnRatesFixtureJSON(t, now, workers...)
	if memory {
		hot = workerMetricsFixtureJSON(t, now, workers...)
	}
	unknown := workerAuthorityAlter(t, hot, func(vector *workerAuthorityVector) {
		for i := range vector.Data.Result {
			series := &vector.Data.Result[i]
			if series.Metric["instance"] == "runtime-hot" {
				series.Value[1] = "NaN"
			}
		}
	})
	workers[3].heap, workers[3].cpuRate, workers[3].allocRate = 0, 0, 0
	corrected := workerChurnRatesFixtureJSON(t, now, workers...)
	if memory {
		corrected = workerMetricsFixtureJSON(t, now, workers...)
	}
	signal := NewWorkerChurnSignal()
	class := "worker-cpu-allocation-churn"
	if memory {
		signal, class = NewWorkerMemorySignal(), "worker-memory-skew"
	}
	notifier := &SlackAlertNotifier{
		settings:     SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{},
	}
	gate := newCadenceAlertGate()
	raised, cleared := 0, 0
	for run, payload := range []string{hot, hot, unknown, unknown, hot, hot, corrected, corrected} {
		alerts, err := signal.Run(context.Background(), workerAuthoritySettings(now, memory, payload, hotRates))
		if err != nil {
			t.Fatal(err)
		}
		ready, held := gate.split(signal, alerts)
		transitions := notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
		for _, transition := range transitions {
			if transition.alert.Class != class {
				continue
			}
			switch transition.kind {
			case slackRaised:
				raised++
			case slackCleared:
				cleared++
				if run != 7 {
					t.Fatalf("worker recovered without two complete healthy runs: run=%d", run)
				}
			}
		}
	}
	if raised != 1 || cleared != 1 {
		t.Fatalf("worker incident transition counts: raised=%d cleared=%d", raised, cleared)
	}
}

// Churn's unavailable interval preserves its established incident identity.
func TestWorkerMetricAuthorityChurnUnknownCannotRecoverIncident(t *testing.T) {
	workerAuthorityRecoveryControl(t, false)
}

// Heap's unavailable interval preserves its established incident identity.
func TestWorkerMetricAuthorityMemoryUnknownCannotRecoverIncident(t *testing.T) {
	workerAuthorityRecoveryControl(t, true)
}

// Primary family labels are query-owned, so every malformed variant is unknown.
func workerAuthorityMarkerModes(memory bool) []string {
	modes := []string{"missing", "empty", "unknown"}
	if memory {
		return append(modes, "both-equal", "both-conflicting", "extra-empty")
	}
	return append(modes, "unrequested-gc")
}

// Mutate returned evidence while retaining valid identities, scalars and quiet peers.
func workerAuthorityMalformedMarkers(t *testing.T, payload string, memory bool, runtime, mode string, single, reverse bool) string {
	t.Helper()
	return workerAuthorityAlter(t, payload, func(vector *workerAuthorityVector) {
		changed := 0
		for i := range vector.Data.Result {
			series := &vector.Data.Result[i]
			if series.Metric["instance"] != runtime {
				continue
			}
			marker := "monitor_rate"
			if memory {
				marker = "monitor_memory_metric"
				if _, source := series.Metric["monitor_memory_source"]; source {
					marker = "monitor_memory_source"
				}
			}
			kind := series.Metric[marker]
			if single && ((!memory && kind != "cpu") || (memory && (marker != "monitor_memory_metric" || kind != "go_memstats_heap_alloc_bytes"))) {
				continue
			}
			switch mode {
			case "missing":
				delete(series.Metric, marker)
			case "empty":
				series.Metric[marker] = ""
			case "unknown":
				series.Metric[marker] = "synthetic-private-family-marker"
			case "unrequested-gc":
				series.Metric[marker] = "gc"
				if strings.HasSuffix(kind, "_source_time") {
					series.Metric[marker] = "gc_source_time"
				}
			case "both-equal":
				series.Metric["monitor_memory_metric"], series.Metric["monitor_memory_source"] = kind, kind
			case "both-conflicting":
				other := "go_memstats_heap_objects"
				if kind == other {
					other = "go_memstats_heap_alloc_bytes"
				}
				series.Metric["monitor_memory_metric"], series.Metric["monitor_memory_source"] = kind, other
			case "extra-empty":
				other := "monitor_memory_source"
				if marker == other {
					other = "monitor_memory_metric"
				}
				series.Metric[other] = ""
			default:
				t.Fatalf("unknown malformed-family control: %s", mode)
			}
			changed++
		}
		want := 4
		if memory {
			want = 12
		}
		if single {
			want = 1
		}
		if changed != want {
			t.Fatalf("malformed-family control changed %d series, want %d", changed, want)
		}
		if reverse {
			for i, j := 0, len(vector.Data.Result)-1; i < j; i, j = i+1, j-1 {
				vector.Data.Result[i], vector.Data.Result[j] = vector.Data.Result[j], vector.Data.Result[i]
			}
		}
	})
}

// Complete quiet peers do not establish completeness for malformed returned workers.
func workerAuthorityMarkerUnknownControl(t *testing.T, memory bool) {
	t.Helper()
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	class := "worker-cpu-allocation-churn"
	if memory {
		class = "worker-memory-skew"
	}
	for _, independentHot := range []bool{false, true} {
		workers := workerAuthorityFleet()[:4]
		runtime := "runtime-hot"
		if independentHot {
			workers = workerAuthorityFleet()
			runtime = "runtime-suspect"
		}
		rates := workerRatesFixtureJSON(t, now, workers...)
		primary := workerChurnRatesFixtureJSON(t, now, workers...)
		if memory {
			primary = workerMetricsFixtureJSON(t, now, workers...)
		}
		for _, mode := range workerAuthorityMarkerModes(memory) {
			for _, single := range []bool{false, true} {
				for _, reverse := range []bool{false, true} {
					payload := workerAuthorityMalformedMarkers(t, primary, memory, runtime, mode, single, reverse)
					findings, alerts := workerAuthorityObserve(t, memory, workerAuthoritySettings(now, memory, payload, rates))
					healthy := 0
					for _, observed := range findings {
						if observed.class != class {
							continue
						}
						if !observed.healthy || observed.target == "worker-hot.example/g1" || observed.target == "worker-suspect.example/g1" {
							t.Fatalf("malformed family established a relative verdict: memory=%t mode=%s single=%t reverse=%t", memory, mode, single, reverse)
						}
						healthy++
					}
					if healthy != 3 || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable ||
						!strings.Contains(alerts[0].Observed, "returned_census_bounded=false") {
						t.Fatalf("malformed family lost quiet controls or unknown coverage: memory=%t mode=%s single=%t reverse=%t", memory, mode, single, reverse)
					}
					for _, alert := range alerts {
						requireAlertOmits(t, alert, "synthetic-private-family-marker")
						encoded, err := json.Marshal(alert)
						if err != nil || strings.Contains(string(encoded), "synthetic-private-family-marker") ||
							strings.Contains(alert.Markdown(), "synthetic-private-family-marker") {
							t.Fatal("public worker output disclosed a raw family marker")
						}
					}
				}
			}
		}
	}
}

// Churn admits exactly its two rate families and their source companions.
func TestWorkerMetricAuthorityChurnMalformedFamilyRemainsUnknown(t *testing.T) {
	workerAuthorityMarkerUnknownControl(t, false)
}

// Memory rejects absent, unknown and simultaneous value/source family markers.
func TestWorkerMetricAuthorityMemoryMalformedFamilyRemainsUnknown(t *testing.T) {
	workerAuthorityMarkerUnknownControl(t, true)
}

// Held and mature unknown coverage must preserve the established hot incident.
func workerAuthorityMarkerRecoveryControl(t *testing.T, memory bool) {
	t.Helper()
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	workers := workerAuthorityFleet()[:4]
	rates := workerRatesFixtureJSON(t, now, workers...)
	hot := workerChurnRatesFixtureJSON(t, now, workers...)
	signal, class := NewWorkerChurnSignal(), "worker-cpu-allocation-churn"
	if memory {
		hot = workerMetricsFixtureJSON(t, now, workers...)
		signal, class = NewWorkerMemorySignal(), "worker-memory-skew"
	}
	workers[3].heap, workers[3].cpuRate, workers[3].allocRate = 0, 0, 0
	corrected := workerChurnRatesFixtureJSON(t, now, workers...)
	if memory {
		corrected = workerMetricsFixtureJSON(t, now, workers...)
	}
	for _, mode := range workerAuthorityMarkerModes(memory) {
		for _, reverse := range []bool{false, true} {
			unknown := workerAuthorityMalformedMarkers(t, hot, memory, "runtime-hot", mode, false, reverse)
			notifier := &SlackAlertNotifier{
				settings:     SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
				keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{},
			}
			gate := newCadenceAlertGate()
			raised, cleared := 0, 0
			for run, payload := range []string{hot, hot, unknown, unknown, hot, hot, corrected, corrected} {
				alerts, err := signal.Run(context.Background(), workerAuthoritySettings(now, memory, payload, rates))
				if err != nil {
					t.Fatal(err)
				}
				if run == 2 || run == 3 {
					if len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !alerts[0].unobservable {
						t.Fatalf("quiet peers concealed malformed returned runtime: memory=%t mode=%s reverse=%t run=%d", memory, mode, reverse, run)
					}
				}
				ready, held := gate.split(signal, alerts)
				transitions := notifier.observeWithLock(now, signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
				for _, transition := range transitions {
					if transition.alert.Class != class {
						continue
					}
					switch transition.kind {
					case slackRaised:
						raised++
					case slackCleared:
						cleared++
						if run != 7 {
							t.Fatalf("malformed family permitted early recovery: memory=%t mode=%s reverse=%t run=%d", memory, mode, reverse, run)
						}
					}
				}
			}
			if raised != 1 || cleared != 1 {
				t.Fatalf("malformed-family incident transitions: memory=%t mode=%s reverse=%t raised=%d cleared=%d", memory, mode, reverse, raised, cleared)
			}
		}
	}
}

// Source schema loss must not clear churn through healthy peer-only replies.
func TestWorkerMetricAuthorityChurnMalformedFamilyCannotRecoverIncident(t *testing.T) {
	workerAuthorityMarkerRecoveryControl(t, false)
}

// Heap incidents likewise require complete corrected evidence for recovery.
func TestWorkerMetricAuthorityMemoryMalformedFamilyCannotRecoverIncident(t *testing.T) {
	workerAuthorityMarkerRecoveryControl(t, true)
}
