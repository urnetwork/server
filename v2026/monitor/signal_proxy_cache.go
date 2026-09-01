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
	proxyCacheFreshness        = 90 * time.Second
	proxyCacheMaximumEntries   = float64(16_384)
	proxyCachePressureFraction = 0.90
)

// Signal proxy-cache implements SIGNALS.md §14.7c. It proves that every
// current Proxy generation exposes and obeys the hard caller-lock cache bound
// that replaced the historical lifetime-retained map.
func NewProxyCacheSignal() Signal {
	return &signalAdapter{
		number: "14.7c", key: "proxy-cache", name: "Proxy caller-lock cache boundedness",
		probe: proxyCacheProbe{},
	}
}

type proxyCacheProbe struct{}

func (proxyCacheProbe) id() string             { return "runtime/proxy-lock-cache" }
func (proxyCacheProbe) tier() string           { return tierWarn }
func (proxyCacheProbe) cadence() time.Duration { return time.Minute }

const (
	proxyCacheMetricRSS uint16 = 1 << iota
	proxyCacheMetricStart
	proxyCacheMetricEntries
	proxyCacheMetricCapacity
	proxyCacheMetricHits
	proxyCacheMetricMisses
	proxyCacheMetricExpirations
	proxyCacheMetricEvictions
	proxyCacheMetricAll = proxyCacheMetricRSS |
		proxyCacheMetricStart |
		proxyCacheMetricEntries |
		proxyCacheMetricCapacity |
		proxyCacheMetricHits |
		proxyCacheMetricMisses |
		proxyCacheMetricExpirations |
		proxyCacheMetricEvictions
)

var proxyCacheMetricNames = []string{
	"process_resident_memory_bytes",
	"process_start_time_seconds",
	"urnetwork_proxy_lock_cache_entries",
	"urnetwork_proxy_lock_cache_capacity",
	"urnetwork_proxy_lock_cache_hits_total",
	"urnetwork_proxy_lock_cache_misses_total",
	"urnetwork_proxy_lock_cache_expirations_total",
	"urnetwork_proxy_lock_cache_evictions_total",
}

type proxyCacheMetrics struct {
	host          string
	block         string
	instance      string
	rss           float64
	start         float64
	entries       float64
	capacity      float64
	hits          float64
	misses        float64
	expirations   float64
	evictions     float64
	availableMask uint16
}

func proxyCacheQuery(environment string) string {
	parts := make([]string, 0, len(proxyCacheMetricNames))
	for _, metricName := range proxyCacheMetricNames {
		series := fmt.Sprintf(
			`label_replace(%s{env=%s,job="proxy"},"monitor_metric",%s,"job",".*")`,
			metricName,
			strconv.Quote(environment),
			strconv.Quote(metricName),
		)
		parts = append(parts, fmt.Sprintf(
			`(%s and on(monitor_metric,env,host,block,instance) (timestamp(%s) >= time() - %d))`,
			series,
			series,
			int64(proxyCacheFreshness/time.Second),
		))
	}
	return strings.Join(parts, " or ")
}

func (proxyCacheProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("proxy cache: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(proxyCacheQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("proxy cache: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("proxy cache: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"proxy cache: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	now := env.now().UTC()
	processes := map[string]*proxyCacheMetrics{}
	for _, series := range response.Data.Result {
		metricName := series.Metric["monitor_metric"]
		if metricName == "" {
			metricName = series.Metric["__name__"]
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("proxy cache: parse %s sample: %w", metricName, err)
		}
		age := now.Sub(observedAt)
		if age > proxyCacheFreshness || age < -30*time.Second {
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("proxy cache: invalid %s value %v", metricName, value)
		}
		host := series.Metric["host"]
		if host == "" {
			continue
		}
		block := series.Metric["block"]
		instance := series.Metric["instance"]
		key := host + "\x00" + block + "\x00" + instance
		process := processes[key]
		if process == nil {
			process = &proxyCacheMetrics{host: host, block: block, instance: instance}
			processes[key] = process
		}
		switch metricName {
		case "process_resident_memory_bytes":
			process.rss = value
			process.availableMask |= proxyCacheMetricRSS
		case "process_start_time_seconds":
			process.start = value
			process.availableMask |= proxyCacheMetricStart
		case "urnetwork_proxy_lock_cache_entries":
			process.entries = value
			process.availableMask |= proxyCacheMetricEntries
		case "urnetwork_proxy_lock_cache_capacity":
			process.capacity = value
			process.availableMask |= proxyCacheMetricCapacity
		case "urnetwork_proxy_lock_cache_hits_total":
			process.hits = value
			process.availableMask |= proxyCacheMetricHits
		case "urnetwork_proxy_lock_cache_misses_total":
			process.misses = value
			process.availableMask |= proxyCacheMetricMisses
		case "urnetwork_proxy_lock_cache_expirations_total":
			process.expirations = value
			process.availableMask |= proxyCacheMetricExpirations
		case "urnetwork_proxy_lock_cache_evictions_total":
			process.evictions = value
			process.availableMask |= proxyCacheMetricEvictions
		}
	}

	current, err := newestProxyCacheProcesses(processes)
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("proxy cache: Mimir returned no actual-scrape-fresh proxy process samples")
	}
	sort.Slice(current, func(i, j int) bool {
		return proxyCacheLabel(current[i]) < proxyCacheLabel(current[j])
	})

	missing := []string{}
	invalid := []string{}
	pressure := []string{}
	for _, process := range current {
		if process.availableMask&proxyCacheMetricAll != proxyCacheMetricAll {
			missing = append(missing, fmt.Sprintf(
				"%s[%s]",
				proxyCacheLabel(process),
				strings.Join(proxyCacheMissingMetrics(process.availableMask), ","),
			))
			continue
		}
		observation := proxyCacheObservation(process)
		if process.capacity <= 0 ||
			process.capacity > proxyCacheMaximumEntries ||
			process.entries > process.capacity {
			invalid = append(invalid, observation)
			continue
		}
		if process.entries >= process.capacity*proxyCachePressureFraction {
			pressure = append(pressure, observation)
		}
	}

	findings := []finding{}
	if len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-lock-cache", tier: tierWarn,
			class: "proxy-lock-cache-unobservable", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh proxy identities do not expose the complete caller-lock cache bound and activity metrics",
				len(missing), len(current),
			),
			mechanism: "The selected Proxy process is live, but it does not expose the cache entry count, hard capacity, or lookup/expiry/eviction counters on that exact identity. The historical map used a 30-second value TTL without deleting expired keys, so every distinct authenticated or rejected proxy ID could remain reachable until process exit. Missing metrics therefore cannot prove the lifetime-retention path is closed.",
			baseline:  "Every newest actual-scrape-fresh Proxy identity exports a cache entry gauge, a positive capacity no greater than 16,384, and hit, miss, expiration, and eviction counters on the same labels.",
			observed: fmt.Sprintf(
				"current_proxy_identities=%d observable_identities=%d missing_identities=%d missing=%s metrics_gateway=%s",
				len(current), len(current)-len(missing), len(missing), strings.Join(missing, ";"), metricHost.name,
			),
			evidence: fmt.Sprintf("Every family is source-timestamp filtered to no more than %.0f seconds old before the exact host/block/instance join; newest process start suppresses a draining generation.", proxyCacheFreshness.Seconds()),
			context:  "This is software-owned memory observability. It does not establish that the old cache owns the whole multi-gigabyte live set, and reducing it cannot create host RAM or additional active-client slots required by §14.7.",
			action:   "Provenance-check and deploy the Proxy artifact containing the bounded TTL/LRU cache and identity-free cache metrics through the ordinary host-serialized rollout. Do not restart solely to hide retained entries or infer zero entries from an absent gauge.",
			verify:   "Every newest identity exports all eight joined metrics for two consecutive scrapes; capacity is positive and no greater than 16,384, and entries never exceed capacity.",
			playbook: "SIGNALS.md §14.7c",
		})
	}
	if len(invalid) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-lock-cache", tier: tierWarn,
			class: "proxy-lock-cache-bound", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh proxy identities violate the caller-lock cache hard-bound contract",
				len(invalid), len(current),
			),
			mechanism: "A zero or relaxed capacity restores an unbounded retention path; an entry count above the published capacity means the bound is not enforced or the gauges are not sampled atomically enough to prove it. The former implementation retained stale map keys for process lifetime because TTL was checked only when the same key returned.",
			baseline:  "Caller-lock cache capacity is in (0, 16,384] and its current entry count is never greater than that capacity on every newest Proxy process.",
			observed: fmt.Sprintf(
				"current_proxy_identities=%d invalid_bound_identities=%d invalid=%s maximum_capacity=%.0f metrics_gateway=%s",
				len(current), len(invalid), strings.Join(invalid, ";"), proxyCacheMaximumEntries, metricHost.name,
			),
			evidence: "The cache implementation has deterministic unique-ID churn, exact-expiry, and LRU tests. Metrics are joined to the newest actual-scrape-fresh process generation, so a bounded replacement cannot borrow an old process's zero or capacity.",
			context:  "This bound limits cache-owned memory and rejection-token churn; §14.7b separately determines whether other process owners still retain a multi-gigabyte heap. Hardware capacity remains a separate closure if the optimized process pair cannot fit with reserve.",
			action:   "Stop promotion of the offending Proxy artifact. Restore the fixed bounded TTL/LRU implementation and its deterministic churn tests; do not raise the capacity to silence the alert. If entries exceed a correct gauge only because collection is non-atomic, fix the collector snapshot rather than weakening the contract.",
			verify:   "Under more than 16,384 distinct synthetic IDs and production acceptance traffic, entries stay at or below the published capacity, the least-recently-used entry is evicted, expired entries miss, and caller-IP locks still accept and reject correctly.",
			playbook: "SIGNALS.md §14.7c",
		})
	}
	if len(pressure) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-lock-cache", tier: tierWarn,
			class: "proxy-lock-cache-pressure", target: "proxy-fleet", frame: metricHost.name, sustain: 5,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh proxy identities retain at least %.0f%% of the caller-lock cache capacity",
				len(pressure), len(current), 100*proxyCachePressureFraction,
			),
			mechanism: "The hard bound still protects heap size, but sustained near-capacity occupancy means distinct proxy IDs are arriving faster than the amortized TTL sweep removes them. Evictions keep memory bounded while misses continue configuration loads; high rejected-ID churn or an undersized legitimate hot set are the discriminators.",
			baseline:  "Caller-lock cache occupancy remains below 90% of capacity on every newest process; hit, miss, expiry, and eviction counters explain any transient churn.",
			observed: fmt.Sprintf(
				"current_proxy_identities=%d pressured_identities=%d pressure=%s pressure_fraction=%.2f metrics_gateway=%s",
				len(current), len(pressure), strings.Join(pressure, ";"), proxyCachePressureFraction, metricHost.name,
			),
			evidence: "Entry count and cumulative activity counters come from the exact newest actual-scrape-fresh identity. Five sustained one-minute probes avoid paging on a short rollout or credential burst.",
			context:  "This is bounded software pressure, not a host-memory-capacity diagnosis. If legitimate active-client demand is near a hard service ceiling, additional capable Proxy hardware or an operational load reduction is still required.",
			action:   "Compare hit/miss/eviction deltas with authenticated acceptance and rejected credential logs without exposing proxy IDs. Rate-limit or block abusive invalid-token sources operationally; if the legitimate 30-second hot set is genuinely larger, measure the per-entry heap cost before changing capacity and keep the new bound explicit.",
			verify:   "Occupancy remains below 90% for 15 minutes, eviction and miss rates return to baseline, configuration storage latency stays healthy, and valid caller-IP locks continue to authorize correctly.",
			playbook: "SIGNALS.md §14.7c",
		})
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("runtime/proxy-lock-cache", tierWarn, "proxy-lock-cache-bound", "proxy-fleet")}, nil
	}
	return findings, nil
}

func newestProxyCacheProcesses(processes map[string]*proxyCacheMetrics) ([]*proxyCacheMetrics, error) {
	newest := map[string]*proxyCacheMetrics{}
	for _, process := range processes {
		if process.availableMask&proxyCacheMetricRSS == 0 || process.rss <= 0 {
			continue
		}
		if process.availableMask&proxyCacheMetricStart == 0 {
			return nil, fmt.Errorf(
				"proxy cache: fresh RSS identity %s omitted process_start_time_seconds",
				proxyCacheLabel(process),
			)
		}
		slot := process.host + "\x00" + process.block
		if process.block == "" {
			slot += "\x00" + process.instance
		}
		previous := newest[slot]
		if previous == nil || process.start > previous.start ||
			(process.start == previous.start && process.instance > previous.instance) {
			newest[slot] = process
		}
	}
	current := make([]*proxyCacheMetrics, 0, len(newest))
	for _, process := range newest {
		current = append(current, process)
	}
	return current, nil
}

func proxyCacheLabel(process *proxyCacheMetrics) string {
	label := process.host
	if process.block != "" {
		label += "/" + process.block
	}
	if process.instance != "" {
		label += "#" + process.instance
	}
	return label
}

func proxyCacheMissingMetrics(mask uint16) []string {
	metrics := []struct {
		bit  uint16
		name string
	}{
		{proxyCacheMetricEntries, "entries"},
		{proxyCacheMetricCapacity, "capacity"},
		{proxyCacheMetricHits, "hits"},
		{proxyCacheMetricMisses, "misses"},
		{proxyCacheMetricExpirations, "expirations"},
		{proxyCacheMetricEvictions, "evictions"},
	}
	missing := []string{}
	for _, metric := range metrics {
		if mask&metric.bit == 0 {
			missing = append(missing, metric.name)
		}
	}
	return missing
}

func proxyCacheObservation(process *proxyCacheMetrics) string {
	totalLookups := process.hits + process.misses
	hitRatio := float64(0)
	if totalLookups > 0 {
		hitRatio = process.hits / totalLookups
	}
	return fmt.Sprintf(
		"%s(entries=%.0f capacity=%.0f occupancy=%.3f hits=%.0f misses=%.0f hit_ratio=%.3f expirations=%.0f evictions=%.0f rss_bytes=%.0f start_time=%.0f)",
		proxyCacheLabel(process),
		process.entries,
		process.capacity,
		proxyCacheOccupancy(process),
		process.hits,
		process.misses,
		hitRatio,
		process.expirations,
		process.evictions,
		process.rss,
		process.start,
	)
}

func proxyCacheOccupancy(process *proxyCacheMetrics) float64 {
	if process.capacity <= 0 {
		return 0
	}
	return process.entries / process.capacity
}
