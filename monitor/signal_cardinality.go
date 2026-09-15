package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const metricCardinalityMarker = "monitor-signal-11.20d-cardinality"

// Prometheus samples are decoded as float64. Reject values above the largest
// exact integer before converting them to int so a malformed response cannot
// wrap or round into a plausible cardinality.
const metricCardinalityMaxExactCount = float64(1<<53 - 1)

// Signal cardinality implements SIGNALS.md §11.20d. It measures two reviewed,
// unnecessary classic metric multipliers before they exhaust Mimir's shared
// recent-head series budget.
func NewCardinalitySignal() Signal {
	return &signalAdapter{
		number: "11.20d", key: "cardinality", name: "Mimir avoidable metric cardinality",
		probe: metricCardinalityProbe{},
	}
}

type metricCardinalityProbe struct{}

func (metricCardinalityProbe) id() string             { return "observability/metric-cardinality" }
func (metricCardinalityProbe) tier() string           { return tierWarn }
func (metricCardinalityProbe) cadence() time.Duration { return 5 * time.Minute }

type metricCardinalitySample struct {
	totalSeries              int
	egressHealthBucketSeries int
	egressHealthSumSeries    int
	egressHealthCountSeries  int
	redisLatencySeries       int
}

func (metricCardinalityProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("metric cardinality: no services host is configured")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(metricCardinalityQuery(env.cfg.env))
	output, host, err := shellFirstServiceGateway(
		ctx, env.runner, hosts, nil,
		"# "+metricCardinalityMarker+"\ncurl -fsS --max-time 20 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("query Mimir through service gateways: %w", err)
	}
	sample, err := parseMetricCardinalitySample(output)
	if err != nil {
		return nil, err
	}
	return evaluateMetricCardinality(host.name, sample), nil
}

func metricCardinalityQuery(environment string) string {
	environmentMatcher := `env=` + strconv.Quote(environment)
	taskBuckets := `urnetwork_egress_probe_health_check_seconds_bucket{` + environmentMatcher + `}`
	taskSums := `urnetwork_egress_probe_health_check_seconds_sum{` + environmentMatcher + `}`
	taskCounts := `urnetwork_egress_probe_health_check_seconds_count{` + environmentMatcher + `}`
	redisPercentiles := `redis_latency_percentiles_usec{` + environmentMatcher + `}`
	redisSum := `redis_latency_percentiles_usec_sum{` + environmentMatcher + `}`
	redisCount := `redis_latency_percentiles_usec_count{` + environmentMatcher + `}`
	checks := []struct {
		name       string
		expression string
	}{
		{name: "total", expression: `count({__name__!="",` + environmentMatcher + `})`},
		{name: "egress_buckets", expression: `count(` + taskBuckets + `)`},
		{name: "egress_sums", expression: `count(` + taskSums + `)`},
		{name: "egress_counts", expression: `count(` + taskCounts + `)`},
		{name: "redis_latency", expression: `((count(` + redisPercentiles + `) or vector(0)) + (count(` + redisSum + `) or vector(0)) + (count(` + redisCount + `) or vector(0)))`},
	}
	parts := make([]string, 0, len(checks))
	for _, check := range checks {
		parts = append(parts, `label_replace((`+check.expression+` or vector(0)),"monitor_check",`+
			strconv.Quote(check.name)+`,"__name__",".*")`)
	}
	return strings.Join(parts, " or ")
}

func parseMetricCardinalitySample(output string) (metricCardinalitySample, error) {
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return metricCardinalitySample{}, fmt.Errorf("decode Mimir cardinality response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return metricCardinalitySample{}, fmt.Errorf(
			"Mimir cardinality status=%q result_type=%q", response.Status, response.Data.ResultType,
		)
	}
	wanted := map[string]bool{
		"total": true, "egress_buckets": true, "egress_sums": true,
		"egress_counts": true, "redis_latency": true,
	}
	values := map[string]int{}
	for _, result := range response.Data.Result {
		name := result.Metric["monitor_check"]
		if !wanted[name] {
			return metricCardinalitySample{}, fmt.Errorf("unexpected Mimir cardinality partition %q", name)
		}
		if _, duplicate := values[name]; duplicate {
			return metricCardinalitySample{}, fmt.Errorf("duplicate Mimir cardinality partition %q", name)
		}
		_, value, err := mimirInstantValue(result.Value)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 ||
			value > metricCardinalityMaxExactCount || value != math.Trunc(value) {
			return metricCardinalitySample{}, fmt.Errorf("invalid Mimir cardinality partition %q", name)
		}
		values[name] = int(value)
	}
	if len(values) != len(wanted) {
		missing := []string{}
		for name := range wanted {
			if _, ok := values[name]; !ok {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		return metricCardinalitySample{}, fmt.Errorf("Mimir cardinality response omitted %s", strings.Join(missing, ","))
	}
	return metricCardinalitySample{
		totalSeries: values["total"], egressHealthBucketSeries: values["egress_buckets"],
		egressHealthSumSeries: values["egress_sums"], egressHealthCountSeries: values["egress_counts"],
		redisLatencySeries: values["redis_latency"],
	}, nil
}

func evaluateMetricCardinality(host string, sample metricCardinalitySample) []finding {
	observed := fmt.Sprintf(
		"total_current_series=%d egress_health_bucket_series=%d egress_health_sum_series=%d egress_health_count_series=%d redis_latency_series=%d",
		sample.totalSeries, sample.egressHealthBucketSeries, sample.egressHealthSumSeries,
		sample.egressHealthCountSeries, sample.redisLatencySeries,
	)
	findings := []finding{}
	if sample.egressHealthBucketSeries > 0 {
		findings = append(findings, finding{
			probeId: "observability/metric-cardinality", tier: tierWarn,
			class: "taskworker-histogram-cardinality", target: "taskworker", frame: "egress-health-latency", sustain: 1,
			symptom:   "Taskworker egress health latency still multiplies every destination/class cohort by classic histogram buckets",
			mechanism: "The finite destination/class labels are crossed with every classic bucket and every random process instance. Routine Taskworker replacement retains the retiring cohort in Mimir's recent head while the new cohort creates another complete bucket set.",
			baseline:  "urnetwork_egress_probe_health_check_seconds has only cumulative sum/count series plus a bounded paired interval maximum; its _bucket family is absent.",
			observed:  observed,
			evidence:  "A bounded loopback Mimir query through " + host + " returned only fixed counts for exact reviewed metric names; labels and values were discarded.",
			context:   "The count proves accepted avoidable series, not that this family alone caused a particular rejected batch. Direct child admission counters under §11.20a remain the loss authority.",
			action:    "Deploy the Taskworker summary/max implementation and matching egress dashboard only after applying the §11.20a rollout headroom gate. Do not remove process identity, raise Mimir limits, restart Mimir, or hide the panel.",
			verify:    "Every Taskworker block has the corrected artifact, the bucket count is zero, sum/count and freshness-gated maximum queries are current, and §11.20a observes removal plus its complete quiet/headroom window.",
			playbook:  "SIGNALS.md §11.20d and §11.20a",
		})
	} else {
		findings = append(findings, healthyFinding(
			"observability/metric-cardinality", tierWarn, "taskworker-histogram-cardinality", "taskworker",
		))
	}
	if sample.egressHealthSumSeries != sample.egressHealthCountSeries {
		findings = append(findings, finding{
			probeId: "observability/metric-cardinality", tier: tierWarn,
			class: "taskworker-latency-pair-mismatch", target: "taskworker", frame: "egress-health-latency", sustain: 1,
			symptom:   "Taskworker egress health latency sum and count series are not paired",
			mechanism: "The corrected producer and its dashboard require one cumulative sum and one cumulative count for every identical environment/service/block/host/instance/destination/class label set. Unequal fleet counts prove an incomplete accepted family, a mixed producer contract, or query-visible staleness.",
			baseline:  "The exact urnetwork_egress_probe_health_check_seconds_sum and _count families have equal current series counts.",
			observed:  observed,
			evidence:  "A bounded loopback Mimir query through " + host + " returned only separate fixed-family counts; labels and values were discarded.",
			context:   "Aggregate count equality is necessary but not sufficient to prove label-by-label pairing. This visibility finding does not attribute an admission rejection or suppress either independently proven cardinality finding.",
			action:    "Compare the exact deployed Taskworker artifact and a privacy-safe label-set difference reduced at the Mimir boundary. Repair the producer or dashboard contract; do not infer the missing side, restart Mimir, or suppress one family.",
			verify:    "Every Taskworker block has one reviewed metric contract, exact sum/count counts are equal on two fresh reads, the dashboard mean is current, and §11.20a remains independently observable.",
			playbook:  "SIGNALS.md §11.20d and §11.20a",
		})
	} else {
		findings = append(findings, healthyFinding(
			"observability/metric-cardinality", tierWarn, "taskworker-latency-pair-mismatch", "taskworker",
		))
	}
	if sample.redisLatencySeries > 0 {
		findings = append(findings, finding{
			probeId: "observability/metric-cardinality", tier: tierWarn,
			class: "redis-latencystats-cardinality", target: "redis-cluster", frame: "info-latencystats", sustain: 1,
			symptom:   "Redis exports unused per-command INFO latencystats summaries",
			mechanism: "Each observed Redis command creates three percentile samples plus sum/count on every node. The dashboard uses commandstats calls and cumulative duration instead, so the latencystats families consume shared recent-head capacity without an operational consumer.",
			baseline:  "Redis latency-tracking is disabled on every node and the three redis_latency_percentiles_usec families have zero current series while commandstats remains fresh.",
			observed:  observed,
			evidence:  "A bounded loopback Mimir query through " + host + " returned only the combined fixed-family count; command labels, nodes, and values were discarded.",
			context:   "This is a measured avoidable source, not proof it initiated a specific admission event. Thresholded LATENCY events and the slowlog remain independent incident evidence.",
			action:    "Apply the reviewed Redis latency-tracking policy through run-redis-clusters.sh after authorization. Retain commandstats, the thresholded latency monitor, and slowlog; do not restart Mimir or disable the exporter.",
			verify:    "All Redis nodes report latency-tracking=no, the three latencystats families reach zero current series, command-rate/duration panels remain fresh, and §11.20a proves removed series and its complete quiet/headroom window.",
			playbook:  "SIGNALS.md §11.20d, §11.14, and §11.20a",
		})
	} else {
		findings = append(findings, healthyFinding(
			"observability/metric-cardinality", tierWarn, "redis-latencystats-cardinality", "redis-cluster",
		))
	}
	return findings
}
