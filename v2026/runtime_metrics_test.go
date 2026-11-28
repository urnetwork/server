package server

// The default registry's Go collector (runtime_metrics.go) exports the chosen
// runtime/metrics once each, keeps every metric of client_golang's default
// collector, adds nothing else, and pushes under the stats push grouping.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	dto "github.com/prometheus/client_model/go"
)

// The gathered families by name.
func testGatherFamilies(t *testing.T, gatherer prometheus.Gatherer) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	familyNames := map[string]*dto.MetricFamily{}
	for _, family := range families {
		familyNames[family.GetName()] = family
	}
	return familyNames
}

// The series a family pushes: one a metric, and a histogram's buckets, +Inf,
// sum and count.
func testFamilySeriesCount(family *dto.MetricFamily) int {
	seriesCount := 0
	for _, metric := range family.Metric {
		if histogram := metric.GetHistogram(); histogram != nil {
			seriesCount += len(histogram.Bucket) + 3
		} else if summary := metric.GetSummary(); summary != nil {
			seriesCount += len(summary.Quantile) + 2
		} else {
			seriesCount += 1
		}
	}
	return seriesCount
}

// The chosen runtime/metrics, under client_golang's names, and the default
// collector's metrics that dashboards and the monitor read, once each.
func TestDefaultRegistryExportsGoRuntimeMetrics(t *testing.T) {
	families := testGatherFamilies(t, prometheus.DefaultGatherer)
	for name, metricType := range map[string]dto.MetricType{
		"go_cpu_classes_gc_total_cpu_seconds_total":            dto.MetricType_COUNTER,
		"go_cpu_classes_gc_mark_assist_cpu_seconds_total":      dto.MetricType_COUNTER,
		"go_cpu_classes_gc_mark_dedicated_cpu_seconds_total":   dto.MetricType_COUNTER,
		"go_cpu_classes_gc_mark_idle_cpu_seconds_total":        dto.MetricType_COUNTER,
		"go_cpu_classes_gc_pause_cpu_seconds_total":            dto.MetricType_COUNTER,
		"go_cpu_classes_idle_cpu_seconds_total":                dto.MetricType_COUNTER,
		"go_cpu_classes_scavenge_total_cpu_seconds_total":      dto.MetricType_COUNTER,
		"go_cpu_classes_scavenge_assist_cpu_seconds_total":     dto.MetricType_COUNTER,
		"go_cpu_classes_scavenge_background_cpu_seconds_total": dto.MetricType_COUNTER,
		"go_cpu_classes_total_cpu_seconds_total":               dto.MetricType_COUNTER,
		"go_cpu_classes_user_cpu_seconds_total":                dto.MetricType_COUNTER,
		"go_sched_latencies_seconds":                           dto.MetricType_HISTOGRAM,
		"go_gc_gogc_percent":                                   dto.MetricType_GAUGE,
		"go_sched_gomaxprocs_threads":                          dto.MetricType_GAUGE,
		"go_gc_gomemlimit_bytes":                               dto.MetricType_GAUGE,
		"go_memstats_alloc_bytes_total":                        dto.MetricType_COUNTER,
		"go_memstats_mallocs_total":                            dto.MetricType_COUNTER,
		"go_memstats_heap_alloc_bytes":                         dto.MetricType_GAUGE,
		"go_memstats_heap_inuse_bytes":                         dto.MetricType_GAUGE,
		"go_memstats_heap_objects":                             dto.MetricType_GAUGE,
		"go_gc_duration_seconds":                               dto.MetricType_SUMMARY,
		"go_goroutines":                                        dto.MetricType_GAUGE,
		"go_threads":                                           dto.MetricType_GAUGE,
		"go_info":                                              dto.MetricType_GAUGE,
	} {
		family := families[name]
		if family == nil {
			t.Errorf("the default registry exports no %s", name)
			continue
		}
		if family.GetType() != metricType {
			t.Errorf("%s is a %s, want a %s", name, family.GetType(), metricType)
		}
		if len(family.Metric) != 1 {
			t.Errorf("%s has %d series, want 1", name, len(family.Metric))
		}
	}
}

// The replacement keeps every metric of client_golang's default collector and
// adds only the CPU classes, whose number is the toolchain's, and the
// scheduler latency, none labeled, within a bound on series a process.
func TestGoRuntimeCollectorAddsOnlyTheChosenMetrics(t *testing.T) {
	defaultRegistry := prometheus.NewRegistry()
	defaultRegistry.MustRegister(collectors.NewGoCollector())
	runtimeRegistry := prometheus.NewRegistry()
	runtimeRegistry.MustRegister(newGoRuntimeCollector())
	defaultFamilies := testGatherFamilies(t, defaultRegistry)
	runtimeFamilies := testGatherFamilies(t, runtimeRegistry)

	for name := range defaultFamilies {
		if runtimeFamilies[name] == nil {
			t.Errorf("the runtime collector drops %s", name)
		}
	}
	addedNamePattern := regexp.MustCompile(`^(go_cpu_classes_[a-z_]+_cpu_seconds_total|go_sched_latencies_seconds)$`)
	addedNames := []string{}
	addedSeriesCount := 0
	for name, family := range runtimeFamilies {
		if defaultFamilies[name] != nil {
			continue
		}
		addedNames = append(addedNames, name)
		if !addedNamePattern.MatchString(name) {
			t.Errorf("the runtime collector adds %s", name)
		}
		for _, metric := range family.Metric {
			if 0 < len(metric.Label) {
				t.Errorf("%s is labeled %v", name, metric.Label)
			}
		}
		if histogram := family.Metric[0].GetHistogram(); histogram != nil && 10 < len(histogram.Bucket) {
			t.Errorf("%s has %d buckets, want at most 10", name, len(histogram.Bucket))
		}
		addedSeriesCount += testFamilySeriesCount(family)
	}
	slices.Sort(addedNames)
	// 21 with Go 1.27: 11 CPU classes, and the histogram's 7 buckets, +Inf,
	// sum and count
	if addedSeriesCount < 12 || 32 < addedSeriesCount {
		t.Fatalf("the runtime collector adds %d series a process (%v), want 12 to 32", addedSeriesCount, addedNames)
	}
	t.Logf("%d series a process: %v", addedSeriesCount, addedNames)
}

// The default registry's Go collector is the replacement, and the default
// collector, whose metrics are all the replacement's, is not registered
// beside it. Neither registration below changes the registry.
func TestDefaultRegistryReplacesTheDefaultGoCollector(t *testing.T) {
	var alreadyRegistered prometheus.AlreadyRegisteredError
	err := prometheus.DefaultRegisterer.Register(newGoRuntimeCollector())
	if !errors.As(err, &alreadyRegistered) {
		t.Fatalf("registering the runtime collector again returned %v; it is not the default registry's", err)
	}
	err = prometheus.DefaultRegisterer.Register(collectors.NewGoCollector())
	if err == nil {
		prometheus.DefaultRegisterer.Unregister(collectors.NewGoCollector())
		t.Fatal("the default Go collector registered beside the runtime collector")
	}
	if errors.As(err, &alreadyRegistered) {
		t.Fatal("the default Go collector is still registered")
	}
}

// The process's whole default registry, the runtime metrics included, pushes
// under the stats push grouping: no Go collector metric has a label of a
// reserved name, which would make the push client refuse every push.
func TestDefaultRegistryPushesUnderStatsPushGrouping(t *testing.T) {
	families := testGatherFamilies(t, prometheus.DefaultGatherer)
	reservedLabelNames := StatsPushReservedLabelNames()
	for name, family := range families {
		if !strings.HasPrefix(name, "go_") {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if slices.Contains(reservedLabelNames, label.GetName()) {
					t.Errorf("%s is labeled %s, a stats push grouping label", name, label.GetName())
				}
			}
		}
	}

	pushBodies := make(chan []byte, 1)
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case pushBodies <- body:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer pushServer.Close()

	err := newGroupedStatsPusher(
		pushServer.URL,
		"env-test",
		"service-test",
		"block-test",
		"host-test",
		"instance-test",
		"writer",
		"secret",
	).Push()
	if err != nil {
		t.Fatal(err)
	}
	pushBody := <-pushBodies
	for _, name := range []string{
		"go_cpu_classes_gc_total_cpu_seconds_total",
		"go_cpu_classes_total_cpu_seconds_total",
		"go_cpu_classes_idle_cpu_seconds_total",
		"go_sched_latencies_seconds",
		"go_gc_gogc_percent",
		"go_sched_gomaxprocs_threads",
	} {
		if !bytes.Contains(pushBody, []byte(name)) {
			t.Errorf("the push carries no %s", name)
		}
	}
}
