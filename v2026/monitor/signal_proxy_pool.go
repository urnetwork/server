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
	proxyPoolByteLimit     = float64(8 << 30)
	proxyPoolByteTolerance = float64(16 << 10)
	proxyPoolFreshness     = 90 * time.Second
)

// Signal proxy-pool implements SIGNALS.md §14.7a. It joins the newest
// actual-scrape-fresh proxy identity for each host/block to the message-pool
// gauges, so draining generations cannot inflate the deployment denominator,
// a missing collector cannot be mistaken for a zero-byte pool, and an old
// one-argument 8 GiB configuration is distinguishable from the intended
// process-wide ceiling.
func NewProxyPoolSignal() Signal {
	return &signalAdapter{
		number: "14.7a", key: "proxy-pool", name: "Proxy message-pool capacity",
		probe: proxyPoolProbe{},
	}
}

type proxyPoolProbe struct{}

func (proxyPoolProbe) id() string             { return "runtime/proxy-message-pool-capacity" }
func (proxyPoolProbe) tier() string           { return tierWarn }
func (proxyPoolProbe) cadence() time.Duration { return time.Minute }

const (
	proxyPoolMetricRSS uint8 = 1 << iota
	proxyPoolMetricStart
	proxyPoolMetricCapacity
	proxyPoolMetricRetained
	proxyPoolMetricPacketRetained
	proxyPoolMetricLargeRetained
	proxyPoolMetricOutstanding
	proxyPoolMetricAll = proxyPoolMetricCapacity |
		proxyPoolMetricRetained |
		proxyPoolMetricPacketRetained |
		proxyPoolMetricLargeRetained |
		proxyPoolMetricOutstanding
)

type proxyPoolMetrics struct {
	host          string
	block         string
	instance      string
	rss           float64
	start         float64
	capacity      float64
	retained      float64
	packet        float64
	large         float64
	outstanding   float64
	availableMask uint8
}

func proxyPoolQuery(metricNames []string, environment string) string {
	parts := make([]string, 0, len(metricNames))
	for _, metricName := range metricNames {
		// monitor_metric keeps the seven independently filtered metric families
		// distinct across PromQL's set union. Binary set matching otherwise
		// ignores __name__ and collapses equal host/block/instance label sets.
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
			int64(proxyPoolFreshness/time.Second),
		))
	}
	return strings.Join(parts, " or ")
}

func (proxyPoolProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("proxy pool: no services host in inventory for the loopback Mimir query")
	}

	metricNames := []string{
		"process_resident_memory_bytes",
		"process_start_time_seconds",
		"urnetwork_message_pool_capacity_bytes",
		"urnetwork_message_pool_retained_bytes",
		"urnetwork_message_pool_packet_retained_bytes",
		"urnetwork_message_pool_large_object_retained_bytes",
		"urnetwork_message_pool_outstanding",
	}
	query := proxyPoolQuery(metricNames, env.cfg.env)
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(query)
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("proxy pool: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("proxy pool: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"proxy pool: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	now := env.now().UTC()
	processes := map[string]*proxyPoolMetrics{}
	for _, series := range response.Data.Result {
		metricName := series.Metric["monitor_metric"]
		if metricName == "" {
			metricName = series.Metric["__name__"]
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("proxy pool: parse %s sample: %w", metricName, err)
		}
		age := now.Sub(observedAt)
		if age > proxyPoolFreshness || age < -30*time.Second {
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("proxy pool: invalid %s value %v", metricName, value)
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
			process = &proxyPoolMetrics{host: host, block: block, instance: instance}
			processes[key] = process
		}
		switch metricName {
		case "process_resident_memory_bytes":
			process.rss = value
			process.availableMask |= proxyPoolMetricRSS
		case "process_start_time_seconds":
			process.start = value
			process.availableMask |= proxyPoolMetricStart
		case "urnetwork_message_pool_capacity_bytes":
			process.capacity = value
			process.availableMask |= proxyPoolMetricCapacity
		case "urnetwork_message_pool_retained_bytes":
			process.retained = value
			process.availableMask |= proxyPoolMetricRetained
		case "urnetwork_message_pool_packet_retained_bytes":
			process.packet = value
			process.availableMask |= proxyPoolMetricPacketRetained
		case "urnetwork_message_pool_large_object_retained_bytes":
			process.large = value
			process.availableMask |= proxyPoolMetricLargeRetained
		case "urnetwork_message_pool_outstanding":
			process.outstanding = value
			process.availableMask |= proxyPoolMetricOutstanding
		}
	}

	current, err := newestProxyPoolProcesses(processes)
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("proxy pool: Mimir returned no actual-scrape-fresh proxy process samples")
	}
	sort.Slice(current, func(i, j int) bool { return proxyPoolLabel(current[i]) < proxyPoolLabel(current[j]) })

	missing := []string{}
	oversized := []string{}
	invalid := []string{}
	for _, process := range current {
		if process.availableMask&proxyPoolMetricAll != proxyPoolMetricAll {
			missing = append(missing, fmt.Sprintf(
				"%s[%s]",
				proxyPoolLabel(process),
				strings.Join(proxyPoolMissingMetrics(process.availableMask), ","),
			))
			continue
		}
		row := proxyPoolObservation(process)
		if process.capacity <= 0 ||
			process.retained > process.capacity ||
			process.packet+process.large != process.retained {
			invalid = append(invalid, row)
			continue
		}
		if process.capacity > proxyPoolByteLimit+proxyPoolByteTolerance {
			oversized = append(oversized, row)
		}
	}

	findings := []finding{}
	if len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-message-pool-capacity", tier: tierWarn,
			class: "proxy-message-pool-unobservable", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh proxy identities do not export the complete message-pool capacity/retention gauge set",
				len(missing), len(current),
			),
			mechanism: "The query filters on each process RSS series' actual scrape timestamp, then process_start_time_seconds selects the newest generation per host/block. That identity is being scraped, so its missing pool gauges are not zero-byte pools or a blind metrics backend. The known source boundary registered the collector in controller, which proxy does not import; consequently the service with the material memory ceiling could not prove its configured or retained pool bytes.",
			baseline:  "The newest actual-scrape-fresh proxy identity for every host/block exports capacity, retained, packet-retained, large-object-retained, and outstanding gauges on the same labels.",
			observed: fmt.Sprintf(
				"current_proxy_identities=%d complete_pool_metric_identities=%d missing_identities=%d missing=%s metrics_gateway=%s",
				len(current), len(current)-len(missing), len(missing), strings.Join(missing, ";"), metricHost.name,
			),
			evidence: fmt.Sprintf("PromQL timestamp(process_resident_memory_bytes) enforces an actual-scrape age of at most %.0f seconds before the exact host/block/instance join; the newest start time suppresses draining generations. A selected process metric without the five pool gauges is affirmative instrumentation drift.", proxyPoolFreshness.Seconds()),
			context:  "This identity count is not a live process-overlap measurement: Prometheus lookback can retain a stopped generation, and §14.7 reads each host's process table for rollout/OOM evidence. Do not infer a pool leak from missing counters. The allocation-versus-retention split remains unknown until this collector is deployed, and lower software retention does not raise the fleet's hardware-backed active-client ceiling.",
			action:   "Deploy a proxy build that registers message-pool metrics from the root server package and applies one 8 GiB total budget with the two-argument ResizeMessagePools form. Do not restart solely to erase RSS evidence; use the ordinary host-serialized rollout guard.",
			verify:   "Every newest fresh proxy identity exports the five gauges; each capacity is at most 8 GiB plus 16 KiB rounding, packet plus large-object retained bytes equals total retained bytes, and direct host RSS is followed across a controlled rollout.",
			playbook: "SIGNALS.md 14.7a",
		})
	}
	if len(oversized) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-message-pool-capacity", tier: tierWarn,
			class: "proxy-message-pool-capacity", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh proxy identities permit more than the intended 8 GiB total message-pool retention",
				len(oversized), len(current),
			),
			mechanism: "ResizeMessagePools' historical one-argument form gives its byte budget to the packet classes and independently to each large-object class. With two large classes, a call that appears to request 8 GiB therefore permits roughly 24 GiB process-wide; the capacity gauge distinguishes that logical ceiling from current RSS or an in-flight ownership leak.",
			baseline:  "Each proxy process has a process-wide message-pool capacity no greater than 8 GiB plus 16 KiB of class-size rounding.",
			observed: fmt.Sprintf(
				"current_proxy_identities=%d oversized_identities=%d limit_bytes=%.0f tolerance_bytes=%.0f oversized=%s metrics_gateway=%s",
				len(current), len(oversized), proxyPoolByteLimit, proxyPoolByteTolerance, strings.Join(oversized, ";"), metricHost.name,
			),
			evidence: "Capacity is the configured free-list retention ceiling exported from one aggregate library snapshot. Retained bytes are included separately; neither value includes every non-pool heap object or host-kernel allocation.",
			context:  "This is a software efficiency defect and a deployment gate, but it does not by itself prove the earlier OOM was a steady-state pool leak. The incident's 19-process/ten-unit overlap remains direct evidence, and additional hardware is still required when steady client demand reaches the aggregate proxy ceiling.",
			action:   "Deploy the proxy helper that assigns one third of 8 GiB to packet classes and the remaining two thirds across large-object classes using ResizeMessagePools(packetBytes, largeBytes). Roll one host-safe candidate at a time and retain §14.7 OOM/UDP evidence.",
			verify:   "Every fresh proxy capacity gauge is at most 8 GiB plus 16 KiB; retained bytes never exceed capacity; host RSS, MemAvailable, OOM, and UDP receive-drop deltas remain healthy through the serialized rollout.",
			playbook: "SIGNALS.md 14.7a",
		})
	}
	if len(invalid) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-message-pool-capacity", tier: tierWarn,
			class: "proxy-message-pool-metrics-invalid", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom:   fmt.Sprintf("%d newest fresh proxy identities export internally inconsistent message-pool gauges", len(invalid)),
			mechanism: "A single library snapshot defines capacity and retention. Total retained bytes must not exceed capacity, and packet plus large-object retained bytes must exactly reconstruct total retained bytes; violating either invariant makes deployment validation unsafe.",
			baseline:  "capacity_bytes is positive, retained_bytes is no greater than capacity_bytes, and packet_retained_bytes + large_object_retained_bytes = retained_bytes.",
			observed:  fmt.Sprintf("invalid_processes=%d invalid=%s metrics_gateway=%s", len(invalid), strings.Join(invalid, ";"), metricHost.name),
			evidence:  "All compared values were fresh and carried the same host, block, and runtime-instance labels.",
			context:   "Treat this as collector or label drift, not as proof that the pool physically owns impossible memory.",
			action:    "Inspect the deployed root collector and its connect-library revision, preserve the raw series, and correct snapshot or label drift before using these gauges to approve a proxy rollout.",
			verify:    "The invariant holds for every fresh proxy identity on two consecutive scrapes and the capacity limit remains at or below 8 GiB plus rounding.",
			playbook:  "SIGNALS.md 14.7a",
		})
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("runtime/proxy-message-pool-capacity", tierWarn, "proxy-message-pool-capacity", "proxy-fleet")}, nil
	}
	return findings, nil
}

func newestProxyPoolProcesses(processes map[string]*proxyPoolMetrics) ([]*proxyPoolMetrics, error) {
	newest := map[string]*proxyPoolMetrics{}
	for _, process := range processes {
		if process.availableMask&proxyPoolMetricRSS == 0 || process.rss <= 0 {
			continue
		}
		if process.availableMask&proxyPoolMetricStart == 0 {
			return nil, fmt.Errorf(
				"proxy pool: fresh RSS identity %s omitted process_start_time_seconds",
				proxyPoolLabel(process),
			)
		}
		slot := process.host + "\x00" + process.block
		if process.block == "" {
			// A missing block label cannot safely collapse unrelated processes.
			slot += "\x00" + process.instance
		}
		previous := newest[slot]
		if previous == nil || process.start > previous.start ||
			(process.start == previous.start && process.instance > previous.instance) {
			newest[slot] = process
		}
	}
	current := make([]*proxyPoolMetrics, 0, len(newest))
	for _, process := range newest {
		current = append(current, process)
	}
	return current, nil
}

func proxyPoolLabel(process *proxyPoolMetrics) string {
	label := process.host
	if process.block != "" {
		label += "/" + process.block
	}
	if process.instance != "" {
		label += "#" + process.instance
	}
	return label
}

func proxyPoolMissingMetrics(mask uint8) []string {
	metrics := []struct {
		bit  uint8
		name string
	}{
		{proxyPoolMetricCapacity, "capacity"},
		{proxyPoolMetricRetained, "retained"},
		{proxyPoolMetricPacketRetained, "packet-retained"},
		{proxyPoolMetricLargeRetained, "large-retained"},
		{proxyPoolMetricOutstanding, "outstanding"},
	}
	missing := []string{}
	for _, metric := range metrics {
		if mask&metric.bit == 0 {
			missing = append(missing, metric.name)
		}
	}
	return missing
}

func proxyPoolObservation(process *proxyPoolMetrics) string {
	return fmt.Sprintf(
		"%s(capacity_bytes=%.0f retained_bytes=%.0f packet_retained_bytes=%.0f large_object_retained_bytes=%.0f outstanding=%.0f rss_bytes=%.0f start_time=%.0f)",
		proxyPoolLabel(process),
		process.capacity,
		process.retained,
		process.packet,
		process.large,
		process.outstanding,
		process.rss,
		process.start,
	)
}
