// Worker metric authority keeps rejected samples in the returned runtime census
// and proves fleet comparisons without treating missing scalars as measurements.
package monitor

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// A duplicate poisons this exact scalar even when both values agree. Seeing an
// invalid scalar before a valid duplicate must not make response order decisive.
type workerMetricSample struct {
	value float64
	seen  bool
	valid bool
}

// Only fixed, query-owned metric kinds enter this exact-runtime map.
type workerMetricSamples map[string]*workerMetricSample

// Records coverage before checking numeric domain and source/evaluation clocks.
// The shared Mimir parser deliberately permits other callers' numeric domains.
func (self workerMetricSamples) observe(kind string, raw []json.RawMessage, now time.Time) {
	sample := self[kind]
	if sample == nil {
		sample = &workerMetricSample{}
		self[kind] = sample
	}
	if sample.seen {
		sample.valid = false
		return
	}
	sample.seen = true
	if len(raw) != 2 || strings.TrimSpace(string(raw[1])) == "null" {
		return
	}
	var timestamp *float64
	if json.Unmarshal(raw[0], &timestamp) != nil || timestamp == nil ||
		math.IsNaN(*timestamp) || math.IsInf(*timestamp, 0) || *timestamp <= 0 ||
		*timestamp >= float64(math.MaxInt64) {
		return
	}
	observedAt, value, err := mimirInstantValue(raw)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}
	if age := now.Sub(observedAt); age > workerMemoryMetricFreshness || age < -30*time.Second {
		return
	}
	if strings.HasSuffix(kind, "_source_time") {
		if value <= 0 || value >= float64(math.MaxInt64) {
			return
		}
		if age := now.Sub(unixFloatTime(value)); age > workerMemoryMetricFreshness || age < -30*time.Second {
			return
		}
	}
	if kind == "process_start_time_seconds" && value > float64(now.Add(30*time.Second).Unix()) {
		return
	}
	sample.value, sample.valid = value, true
}

// Missing, invalid and duplicate scalars all lack measurement authority.
func (self workerMetricSamples) value(kind string) (float64, bool) {
	if sample := self[kind]; sample != nil && sample.valid {
		return sample.value, true
	}
	return 0, false
}

// A scalar needs its own fresh source, not the instant query's evaluation clock.
func (self workerMetricSamples) qualified(kind string) (float64, bool) {
	value, valid := self.value(kind)
	_, sourceValid := self.value(kind + "_source_time")
	return value, valid && sourceValid
}

// Optional scalar enrichment never renders a rejected value as measured zero.
func (self workerMetricSamples) summary(kind string) string {
	if value, valid := self.qualified(kind); valid {
		return fmt.Sprintf("%.0f", value)
	}
	return "unknown"
}

// The timestamp companion preserves returned runtime coverage when rate lacks
// two samples or source freshness filtering removes its value. This is a census
// of returned series, not an inventory of exporters absent beyond lookback.
func workerRatesQuery(environment, window string, includeGc bool) string {
	queries := []string{}
	for _, metric := range []struct {
		kind string
		name string
	}{
		{kind: "cpu", name: "process_cpu_seconds_total"},
		{kind: "alloc", name: "go_memstats_alloc_bytes_total"},
		{kind: "gc", name: "go_gc_duration_seconds_count"},
		{kind: "gc_pause", name: "go_gc_duration_seconds_sum"},
	} {
		if !includeGc && metric.kind != "cpu" && metric.kind != "alloc" {
			continue
		}
		selector := fmt.Sprintf(`%s{env=%s,job="taskworker"}`, metric.name, strconv.Quote(environment))
		queries = append(queries,
			fmt.Sprintf(`label_replace((rate(%s[%s]) and (timestamp(%s) >= time() - 90) and (timestamp(%s) <= time() + 30)),"monitor_rate",%s,"job",".*")`,
				selector, window, selector, selector, strconv.Quote(metric.kind)),
			fmt.Sprintf(`label_replace(timestamp(%s),"monitor_rate",%s,"job",".*")`,
				selector, strconv.Quote(metric.kind+"_source_time")),
		)
	}
	return strings.Join(queries, " or ")
}

// A malformed identity cannot be safely joined or exposed as a public target.
func workerMetricIdentity(metric map[string]string, environment string) bool {
	return metric["env"] == environment && metric["job"] == "taskworker" &&
		safeServiceLoadLabel(metric["host"]) && safeServiceLoadLabel(metric["block"]) &&
		safeServiceLoadLabel(metric["instance"])
}

// Missing nonnegative values range from zero to infinity. Bounding both possible
// medians lets independent positive/negative controls survive a partial sibling.
type workerMedian struct {
	lower    float64
	upper    float64
	complete bool
}

// A warning or unbound identity leaves the number of omitted runtimes unknown.
func workerMedianBounds(values []float64, total int, bounded bool) workerMedian {
	if !bounded || total == 0 {
		return workerMedian{lower: 0, upper: math.Inf(1)}
	}
	lowerValues := append([]float64(nil), values...)
	upperValues := append([]float64(nil), values...)
	for i := len(values); i < total; i++ {
		lowerValues = append(lowerValues, 0)
		upperValues = append(upperValues, math.Inf(1))
	}
	return workerMedian{
		lower: medianFloat64(lowerValues), upper: medianFloat64(upperValues),
		complete: len(values) == total,
	}
}

// Preserve existing measured fields for complete fleets and name a conservative
// upper bound explicitly when incomplete scalars can still prove the finding.
func (self workerMedian) summary(name, format string) string {
	if self.complete {
		return name + "=" + fmt.Sprintf(format, self.upper)
	}
	if math.IsInf(self.upper, 1) {
		return name + "=unknown"
	}
	return name + "_upper_bound=" + fmt.Sprintf(format, self.upper)
}

// Positive work over an idle baseline has unbounded skew; never print Inf/NaN
// or use a display fallback as the threshold comparison.
func workerMetricRatio(value, baseline float64) string {
	if math.IsInf(baseline, 1) {
		return "unknown"
	}
	if baseline == 0 && value == 0 {
		return "0.0"
	}
	if baseline == 0 {
		return "unbounded"
	}
	if math.IsInf(value/baseline, 1) {
		return "overflow"
	}
	return fmt.Sprintf("%.1f", value/baseline)
}

// Coverage gaps accompany established findings and block absence-based recovery.
// Only aggregate counts and fixed language enter public diagnostics.
func workerMetricCoverage(target string, runtimes, incomplete, indeterminate int, bounded bool) finding {
	observed := cannotObserveFinding(target, fmt.Errorf("worker metric coverage is incomplete"))
	observed.observed = fmt.Sprintf("returned_runtimes=%d incomplete_runtimes=%d indeterminate_runtimes=%d returned_census_bounded=%t",
		runtimes, incomplete, indeterminate, bounded)
	observed.mechanism = "Missing, stale, invalid, or duplicate worker scalars and partial source responses do not establish a fleet baseline. Independent measured findings remain valid; unobserved workers cannot be declared recovered."
	observed.evidence = "Only finite nonnegative, unique, source-fresh scalars with an exact host/block/runtime identity are measurements. Unknown scalars remain in the returned census; warnings or unbound identities leave its size unknown. Raw labels, values and source errors remain private."
	return observed
}
