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

const (
	metricCardinalityMarker            = "monitor-signal-11.20d-cardinality"
	metricCardinalityRedisSourceMarker = "monitor-signal-11.20d-redis-latencystats-source"
)

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

// The host-local reducer retains only fixed counts. Redis replies and node
// addresses never enter an Alert.
type redisLatencyTrackingObservation struct {
	expected    int
	enabled     int
	disabled    int
	invalid     int
	unreachable int
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
	redisSource, redisSourceErr := observeRedisLatencyTracking(ctx, env)
	return evaluateMetricCardinality(host.name, sample, redisSource, redisSourceErr), nil
}

func observeRedisLatencyTracking(ctx context.Context, env *probeEnv) (redisLatencyTrackingObservation, error) {
	host := env.cfg.hostByRole("redis-cluster")
	if host == nil {
		return redisLatencyTrackingObservation{}, fmt.Errorf("redis latencystats source: no redis-cluster host")
	}
	ports := host.redisNodePorts()
	if len(ports) == 0 {
		return redisLatencyTrackingObservation{}, fmt.Errorf("redis latencystats source: no node ports")
	}
	portFields := make([]string, 0, len(ports))
	for _, port := range ports {
		portFields = append(portFields, strconv.Itoa(port))
	}
	command := `# ` + metricCardinalityRedisSourceMarker + `
set -eu
command -v timeout >/dev/null
command -v redis-cli >/dev/null
expected=0
enabled=0
disabled=0
invalid=0
unreachable=0
for port in ` + strings.Join(portFields, " ") + `; do
  expected=$((expected + 1))
  if ! response=$(timeout 4 redis-cli --raw -h 127.0.0.1 -p "$port" CONFIG GET latency-tracking 2>/dev/null); then
    unreachable=$((unreachable + 1))
    continue
  fi
  key=$(printf '%s\n' "$response" | sed -n '1p')
  value=$(printf '%s\n' "$response" | sed -n '2p')
  extra=$(printf '%s\n' "$response" | sed -n '3p')
  if [ "$key" != latency-tracking ] || [ -n "$extra" ]; then
    invalid=$((invalid + 1))
    continue
  fi
  case "$value" in
    yes) enabled=$((enabled + 1)) ;;
    no) disabled=$((disabled + 1)) ;;
    *) invalid=$((invalid + 1)) ;;
  esac
done
printf 'expected=%d enabled=%d disabled=%d invalid=%d unreachable=%d\n' \
  "$expected" "$enabled" "$disabled" "$invalid" "$unreachable"
`
	output, err := env.runner.shell(ctx, host, command)
	if err != nil {
		return redisLatencyTrackingObservation{expected: len(ports)}, fmt.Errorf("redis latencystats source: host reduction failed")
	}
	return parseRedisLatencyTrackingObservation(output, len(ports))
}

func parseRedisLatencyTrackingObservation(output string, configuredNodes int) (redisLatencyTrackingObservation, error) {
	observation := redisLatencyTrackingObservation{}
	trimmed := strings.TrimSpace(output)
	parsed, err := fmt.Sscanf(
		trimmed,
		"expected=%d enabled=%d disabled=%d invalid=%d unreachable=%d",
		&observation.expected,
		&observation.enabled,
		&observation.disabled,
		&observation.invalid,
		&observation.unreachable,
	)
	if err != nil || parsed != 5 || trimmed != fmt.Sprintf(
		"expected=%d enabled=%d disabled=%d invalid=%d unreachable=%d",
		observation.expected,
		observation.enabled,
		observation.disabled,
		observation.invalid,
		observation.unreachable,
	) {
		return redisLatencyTrackingObservation{}, fmt.Errorf("redis latencystats source: invalid reduction")
	}
	if configuredNodes <= 0 || observation.expected != configuredNodes ||
		observation.enabled < 0 || observation.disabled < 0 || observation.invalid < 0 || observation.unreachable < 0 ||
		observation.enabled+observation.disabled+observation.invalid+observation.unreachable != observation.expected {
		return redisLatencyTrackingObservation{}, fmt.Errorf("redis latencystats source: inconsistent reduction")
	}
	return observation, nil
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

func evaluateMetricCardinality(
	host string,
	sample metricCardinalitySample,
	redisSource redisLatencyTrackingObservation,
	redisSourceErr error,
) []finding {
	observed := fmt.Sprintf(
		"total_current_series=%d egress_health_bucket_series=%d egress_health_sum_series=%d egress_health_count_series=%d redis_latency_series=%d",
		sample.totalSeries, sample.egressHealthBucketSeries, sample.egressHealthSumSeries,
		sample.egressHealthCountSeries, sample.redisLatencySeries,
	)
	findings := []finding{}
	redisSourceObserved := "source_observation=unavailable"
	if redisSourceErr == nil {
		redisSourceObserved = fmt.Sprintf(
			"source_expected_nodes=%d source_enabled_nodes=%d source_disabled_nodes=%d source_invalid_nodes=%d source_unreachable_nodes=%d",
			redisSource.expected,
			redisSource.enabled,
			redisSource.disabled,
			redisSource.invalid,
			redisSource.unreachable,
		)
	}
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
		action := "Restore the bounded live Redis source observation, then apply the reviewed latency-tracking policy only if any node is enabled. Retain commandstats, the thresholded latency monitor, and slowlog; do not restart Mimir or disable the exporter."
		context := "This is a measured avoidable source, not proof it initiated a specific admission event. Thresholded LATENCY events and the slowlog remain independent incident evidence."
		if redisSourceErr == nil && redisSource.enabled == 0 && redisSource.invalid == 0 && redisSource.unreachable == 0 {
			action = "The live source is already disabled on every node. Do not rerun or restart Redis to clear query-visible samples; wait for Prometheus staleness and ordinary Mimir head compaction, while requiring commandstats to remain fresh."
			context += " The live source has converged; a still-positive instant count is retained pre-convergence data within query lookback, not proof that Redis continues producing it."
		}
		findings = append(findings, finding{
			probeId: "observability/metric-cardinality", tier: tierWarn,
			class: "redis-latencystats-cardinality", target: "redis-cluster", frame: "info-latencystats", sustain: 1,
			symptom:   "Redis exports unused per-command INFO latencystats summaries",
			mechanism: "Each observed Redis command creates three percentile samples plus sum/count on every node. The dashboard uses commandstats calls and cumulative duration instead, so the latencystats families consume shared recent-head capacity without an operational consumer.",
			baseline:  "Redis latency-tracking is disabled on every node and the three redis_latency_percentiles_usec families have zero current series while commandstats remains fresh.",
			observed:  observed + " " + redisSourceObserved,
			evidence:  "A bounded loopback Mimir query through " + host + " returned only the combined fixed-family count; command labels, nodes, and values were discarded.",
			context:   context,
			action:    action,
			verify:    "All Redis nodes report latency-tracking=no, the three latencystats families reach zero current series, command-rate/duration panels remain fresh, and §11.20a proves removed series and its complete quiet/headroom window.",
			playbook:  "SIGNALS.md §11.20d, §11.14, and §11.20a",
		})
	} else {
		findings = append(findings, healthyFinding(
			"observability/metric-cardinality", tierWarn, "redis-latencystats-cardinality", "redis-cluster",
		))
	}

	if redisSourceErr != nil || redisSource.invalid > 0 || redisSource.unreachable > 0 {
		findings = append(findings, finding{
			probeId: "observability/metric-cardinality", tier: tierWarn,
			class: "redis-latencystats-source-unobservable", target: "redis-cluster", frame: "latency-tracking", sustain: 1,
			symptom:   "The monitor cannot prove the live Redis latency-tracking policy on every configured node",
			mechanism: "A missing, unreachable, or malformed CONFIG GET result leaves the avoidable latencystats producer state unknown. Query-visible absence alone can be temporary and must not be treated as durable source disablement.",
			baseline:  "Every configured Redis node returns exactly latency-tracking=no through the bounded host-local reducer.",
			observed:  redisSourceObserved,
			evidence:  "Only fixed node counts leave the Redis host; ports, replies, addresses, and error text are discarded.",
			action:    "Restore local Redis command reachability and the exact CONFIG GET reduction. Do not infer the missing node's setting, restart Redis, or use a zero Mimir count as a substitute.",
			verify:    "Two fresh runs observe every configured node with latency-tracking disabled and no invalid or unreachable result.",
			playbook:  "SIGNALS.md §11.20d",
		})
	} else {
		findings = append(findings, healthyFinding(
			"observability/metric-cardinality", tierWarn, "redis-latencystats-source-unobservable", "redis-cluster",
		))
	}
	if redisSourceErr == nil && redisSource.enabled > 0 {
		findings = append(findings, finding{
			probeId: "observability/metric-cardinality", tier: tierWarn,
			class: "redis-latencystats-source-drift", target: "redis-cluster", frame: "latency-tracking", sustain: 1,
			symptom:   "One or more Redis nodes still generate the unused INFO latencystats family",
			mechanism: "The persistent template and live process setting diverged, or the cardinality reduction was staged without its live CONFIG SET. Future command observations can recreate the removed series.",
			baseline:  "latency-tracking=no on every configured Redis node.",
			observed:  redisSourceObserved,
			evidence:  "The Redis host reduced exact CONFIG GET results to fixed counts; no port, address, command value beyond the yes/no enum, or raw reply enters the alert.",
			action:    "Run the reviewed Redis convergence playbook, which updates both redis.conf and the live process setting without restarting Redis. Preserve commandstats, the thresholded latency monitor, and slowlog.",
			verify:    "Two fresh runs observe every configured node with latency-tracking disabled; current latencystats series then expire and ordinary head compaction restores Mimir headroom.",
			playbook:  "SIGNALS.md §11.20d and §11.20a",
		})
	} else if redisSourceErr == nil {
		findings = append(findings, healthyFinding(
			"observability/metric-cardinality", tierWarn, "redis-latencystats-source-drift", "redis-cluster",
		))
	}
	return findings
}
