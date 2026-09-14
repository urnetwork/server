// Correlates fresh carrier-budget snapshots with sampled preemption pressure.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	proxyTransportFreshness              = 90 * time.Second
	proxyTransportMimirEndpoint          = "http://127.0.0.1:3100/prometheus/api/v1/query"
	proxyTransportSourceSuffix           = "__source_timestamp"
	proxyTransportPerDeviceCount         = 16
	proxyTransportMinimumBudgetPerDevice = float64(3 * 1024 * 1024)
	proxyTransportChurnRate              = 0.1
	proxyTransportChurnCpuCores          = 0.5
)

// Implements SIGNALS.md §14.6 for the newest proxy process's aggregate private
// carrier budgets; sampled preemptions indicate pressure without proving its cause.
func NewProxyTransportSignal() Signal {
	return &signalAdapter{
		number: "14.6", key: "proxy-transport", name: "Proxy carrier-budget admission",
		probe: proxyTransportProbe{},
	}
}

// Queries the metrics gateway without retaining hosted device identity.
type proxyTransportProbe struct{}

// Uses one stable identity for admission, visibility, and accounting findings.
func (proxyTransportProbe) id() string { return "runtime/proxy-transport-budget" }

// Reports suspected pressure and observability gaps at warning severity.
func (proxyTransportProbe) tier() string { return tierWarn }

// Samples once per minute; pressure findings require two observations.
func (proxyTransportProbe) cadence() time.Duration { return time.Minute }

var proxyTransportMetricNames = []string{
	"process_resident_memory_bytes",
	"process_start_time_seconds",
	"process_cpu_seconds_total",
	"urnetwork_proxy_devices_live",
	"urnetwork_proxy_device_memory_target_bytes",
	"urnetwork_proxy_platform_transport_budget_bytes",
	"urnetwork_proxy_platform_transport_used_bytes",
	"urnetwork_proxy_platform_transports_max",
	"urnetwork_proxy_platform_transports_used",
	"urnetwork_proxy_platform_transports_pending_h1",
	"urnetwork_proxy_platform_transports_pending_h1_bytes",
	"urnetwork_proxy_platform_transport_slot_full_pending_h1_devices",
	"urnetwork_proxy_platform_transport_h3_preemptions_total",
	"urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total",
}

const (
	proxyTransportCpuRateMetric        = "monitor_cpu_rate"
	proxyTransportPreemptionRateMetric = "monitor_h3_preemption_rate"
)

// Groups values and producer timestamps by one exact process identity.
type proxyTransportMetrics struct {
	host        string
	block       string
	instance    string
	values      map[string]float64
	sourceTimes map[string]float64
}

// Filters each required family by producer freshness and preserves its source
// timestamp; counter rates stay optional while their range warms.
func proxyTransportQuery(environment string) string {
	parts := make([]string, 0, 2*len(proxyTransportMetricNames)+2)
	for _, metricName := range proxyTransportMetricNames {
		rawSeries := fmt.Sprintf(
			`%s{env=%s,job="proxy"}`,
			metricName,
			strconv.Quote(environment),
		)
		series := fmt.Sprintf(
			`label_replace(%s,"monitor_metric",%s,"job",".*")`,
			rawSeries,
			strconv.Quote(metricName),
		)
		fresh := fmt.Sprintf(
			`timestamp(%s) >= time() - %d`,
			rawSeries,
			int64(proxyTransportFreshness/time.Second),
		)
		parts = append(parts, fmt.Sprintf(
			`(%s and on(env,host,block,instance) (%s))`,
			series,
			fresh,
		))
		sourceTimestamp := fmt.Sprintf(
			`label_replace(timestamp(%s),"monitor_metric",%s,"job",".*")`,
			rawSeries,
			strconv.Quote(metricName+proxyTransportSourceSuffix),
		)
		parts = append(parts, fmt.Sprintf(
			`(%s and on(env,host,block,instance) (%s))`,
			sourceTimestamp,
			fresh,
		))
	}
	parts = append(parts,
		fmt.Sprintf(
			`label_replace(rate(process_cpu_seconds_total{env=%s,job="proxy"}[2m]),"monitor_metric",%s,"job",".*")`,
			strconv.Quote(environment),
			strconv.Quote(proxyTransportCpuRateMetric),
		),
		fmt.Sprintf(
			`label_replace(rate(urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total{env=%s,job="proxy"}[2m]),"monitor_metric",%s,"job",".*")`,
			strconv.Quote(environment),
			strconv.Quote(proxyTransportPreemptionRateMetric),
		),
	)
	return strings.Join(parts, " or ")
}

// Selects the newest process before checking scrape coherence and private
// budget invariants; sampled preemptions require separate causal verification.
func (proxyTransportProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	expectedHosts := []string{}
	for _, target := range env.cfg.hosts {
		if target.proxy != nil {
			expectedHosts = append(expectedHosts, target.name)
		}
	}
	sort.Strings(expectedHosts)
	if env.cfg.proxyPathExpectedHosts > 0 && len(expectedHosts) != env.cfg.proxyPathExpectedHosts {
		return []finding{{
			probeId: "runtime/proxy-transport-budget", tier: tierWarn,
			class: "proxy-transport-unobservable", target: "proxy-fleet", frame: "active-inventory", sustain: 1,
			symptom:   "The monitor host inventory does not contain every active Proxy placement",
			mechanism: "The active services.yml placement count is authoritative, but fewer or additional monitor hosts are armed for Proxy telemetry. Building a denominator from that incomplete join could hide an entirely missing Proxy host behind healthy siblings.",
			baseline:  "The services.yml Proxy placement count exactly matches the monitor hosts carrying derived Proxy settings.",
			observed:  fmt.Sprintf("expected_proxy_hosts=%d armed_proxy_hosts=%d", env.cfg.proxyPathExpectedHosts, len(expectedHosts)),
			evidence:  "Expected count and armed host settings are derived independently from the same active services.yml version before any Mimir identity is considered.",
			context:   "This is an inventory visibility failure, not zero carrier pressure and not evidence that any missing Proxy process is healthy.",
			action:    "Restore every active Proxy placement to the monitor host inventory and reload settings. Do not infer fleet health from the observable subset.",
			verify:    "The authoritative and armed Proxy host counts match, then every expected host/block pair has one newest complete scrape for two cadences.",
			playbook:  "SIGNALS.md §14.6",
		}}, nil
	}
	if len(expectedHosts) == 0 {
		return []finding{healthyFinding("runtime/proxy-transport-budget", tierWarn, "proxy-transport-admission-pending", "proxy-fleet")}, nil
	}
	expectedBlocks := append([]string(nil), env.cfg.logServiceBlocks["proxy"]...)
	sort.Strings(expectedBlocks)
	if len(expectedBlocks) == 0 {
		return []finding{{
			probeId: "runtime/proxy-transport-budget", tier: tierWarn,
			class: "proxy-transport-unobservable", target: "proxy-fleet", frame: "active-inventory", sustain: 1,
			symptom:   "Active Proxy hosts have no authoritative service-block denominator for carrier telemetry",
			mechanism: "The carrier query can evaluate only observed series. Without the active services.yml Proxy block set, a missing process could be mistaken for an empty healthy fleet.",
			baseline:  "The active inventory supplies at least one Proxy block for every configured Proxy host.",
			observed:  fmt.Sprintf("expected_proxy_hosts=%d expected_proxy_blocks=0", len(expectedHosts)),
			evidence:  "Expected hosts come from active Proxy placements; expected blocks come from the same active services.yml version used by standing service coverage.",
			context:   "This is an inventory visibility failure, not zero carrier pressure and not evidence that a Proxy process is healthy.",
			action:    "Restore the active Proxy block inventory in services.yml and reload the monitor settings. Do not infer health from whatever Mimir series happen to remain.",
			verify:    "A fresh settings generation enumerates every expected Proxy host/block pair and each pair has one newest complete scrape for two cadences.",
			playbook:  "SIGNALS.md §14.6",
		}}, nil
	}
	expectedSlots := map[string]string{}
	for _, host := range expectedHosts {
		for _, block := range expectedBlocks {
			expectedSlots[host+"\x00"+block] = host + "/" + block
		}
	}

	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("proxy transport: no services host in inventory for the loopback Mimir query")
	}

	query := proxyTransportQuery(env.cfg.env)
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 --data-urlencode "+shellSingleQuote("query="+query)+" "+
			shellSingleQuote(proxyTransportMimirEndpoint),
	)
	if err != nil {
		return nil, fmt.Errorf("proxy transport: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("proxy transport: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"proxy transport: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	now := env.now().UTC()
	processes := map[string]*proxyTransportMetrics{}
	for _, series := range response.Data.Result {
		metricName := series.Metric["monitor_metric"]
		if metricName == "" {
			metricName = series.Metric["__name__"]
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("proxy transport: parse %s sample: %w", metricName, err)
		}
		age := now.Sub(observedAt)
		if age > proxyTransportFreshness || age < -30*time.Second {
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("proxy transport: invalid %s value %v", metricName, value)
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
			process = &proxyTransportMetrics{
				host: host, block: block, instance: instance,
				values: map[string]float64{}, sourceTimes: map[string]float64{},
			}
			processes[key] = process
		}
		if strings.HasSuffix(metricName, proxyTransportSourceSuffix) {
			process.sourceTimes[strings.TrimSuffix(metricName, proxyTransportSourceSuffix)] = value
		} else {
			process.values[metricName] = value
		}
	}

	expectedProcesses := map[string]*proxyTransportMetrics{}
	for key, process := range processes {
		if _, ok := expectedSlots[process.host+"\x00"+process.block]; ok {
			expectedProcesses[key] = process
		}
	}
	current, err := newestProxyTransportProcesses(expectedProcesses)
	if err != nil {
		return nil, err
	}
	observedSlots := map[string]bool{}
	for _, process := range current {
		observedSlots[process.host+"\x00"+process.block] = true
	}
	missingExpected := []string{}
	for slot, label := range expectedSlots {
		if !observedSlots[slot] {
			missingExpected = append(missingExpected, label)
		}
	}
	sort.Strings(missingExpected)
	sort.Slice(current, func(i, j int) bool {
		return proxyTransportLabel(current[i]) < proxyTransportLabel(current[j])
	})

	missing := []string{}
	incoherent := []string{}
	invalid := []string{}
	isolation := []string{}
	pending := []string{}
	churn := []string{}
	for _, process := range current {
		label := proxyTransportLabel(process)
		missingNames := proxyTransportMissingMetrics(process)
		if len(missingNames) > 0 {
			missing = append(missing, fmt.Sprintf("%s[%s]", label, strings.Join(missingNames, ",")))
			continue
		}
		if reason := proxyTransportSnapshotReason(process); reason != "" {
			incoherent = append(incoherent, fmt.Sprintf("%s[%s]", label, reason))
			continue
		}
		if reason := proxyTransportInvalidReason(process); reason != "" {
			invalid = append(invalid, fmt.Sprintf("%s[%s;%s]", label, reason, proxyTransportObservation(process)))
			continue
		}
		if reason := proxyTransportIsolationReason(process); reason != "" {
			isolation = append(isolation, fmt.Sprintf("%s[%s;%s]", label, reason, proxyTransportObservation(process)))
		}

		pendingH1 := process.values["urnetwork_proxy_platform_transports_pending_h1"]
		if pendingH1 <= 0 {
			continue
		}
		cpuRate, hasCpuRate := process.values[proxyTransportCpuRateMetric]
		preemptionRate, hasPreemptionRate := process.values[proxyTransportPreemptionRateMetric]
		observation := proxyTransportObservation(process)
		if hasCpuRate && hasPreemptionRate &&
			process.values["urnetwork_proxy_platform_transport_slot_full_pending_h1_devices"] > 0 &&
			preemptionRate >= proxyTransportChurnRate &&
			cpuRate >= proxyTransportChurnCpuCores {
			churn = append(churn, observation)
		} else {
			if !hasCpuRate || !hasPreemptionRate {
				observation += ",churn_rates=warming-or-unobservable"
			}
			pending = append(pending, observation)
		}
	}

	findings := []finding{}
	if len(missingExpected) > 0 || len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-transport-budget", tier: tierWarn,
			class: "proxy-transport-unobservable", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom:   fmt.Sprintf("%d of %d expected Proxy host/block identities are absent or lack the complete carrier-budget metric set", len(missingExpected)+len(missing), len(expectedSlots)),
			mechanism: "An expected active host/block has no fresh process series, or its newest process lacks one or more identity-free carrier gauges or counters. Missing process, pending, slot-full, or same-device preemption telemetry is unknown admission state; it must not be interpreted as zero pressure or as proof that the connect#211 loop is absent.",
			baseline:  "Every active services.yml Proxy host/block pair has one newest actual-scrape-fresh process exporting CPU, device count, target bytes, carrier byte/count budget and use, pending H1 count/bytes, slot-full pending-device count, the process-monotonic H3-preemption counter, and its same-device slot-full subset.",
			observed:  fmt.Sprintf("expected_proxy_identities=%d current_proxy_identities=%d absent_identities=%d absent=%s incomplete_identities=%d incomplete=%s metrics_gateway=%s", len(expectedSlots), len(current), len(missingExpected), strings.Join(missingExpected, ","), len(missing), strings.Join(missing, ";"), metricHost.name),
			evidence:  fmt.Sprintf("The denominator is the active Proxy host/block cross-product. Each observed family is source-timestamp filtered to at most %.0f seconds old before exact host/block/instance selection; process start chooses the newest generation.", proxyTransportFreshness.Seconds()),
			context:   "This is an observability/deployment gap, not carrier starvation and not host-memory exhaustion. §14.7 remains the hardware and host reserve owner.",
			action:    "For an absent expected identity, restore its intended deployment and scrape path; for an incomplete identity, deploy the identity-free Proxy carrier telemetry through the ordinary host-serialized rollout. Preserve current processes and bounded logs while metrics are unavailable; do not restart or enlarge a budget to manufacture a green sample.",
			verify:    "Every expected Proxy host/block pair has one newest process exporting the complete metric set on two consecutive coherent scrapes, including a rateable H3-preemption counter after the two-minute range warms.",
			playbook:  "SIGNALS.md §14.6",
		})
	}
	if len(incoherent) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-transport-budget", tier: tierWarn,
			class: "proxy-transport-snapshot-unobservable", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom:   fmt.Sprintf("%d of %d newest fresh proxy identities lack a same-scrape carrier-budget snapshot", len(incoherent), len(current)),
			mechanism: "Mimir can expose only part of a remote-write scrape while ingestion is in flight or rejected. Instant-query evaluation timestamps cannot prove that independently selected gauges belonged to one SDK sample, so cross-field budget invariants are unsafe until their source timestamps agree.",
			baseline:  "Every required carrier value for one host/block/instance has one identical producer scrape timestamp.",
			observed:  fmt.Sprintf("current_proxy_identities=%d incoherent_identities=%d incoherent=%s metrics_gateway=%s", len(current), len(incoherent), strings.Join(incoherent, ";"), metricHost.name),
			evidence:  "The query returns timestamp(metric) as a companion value for every required family rather than trusting the common instant-query evaluation time.",
			context:   "A mixed snapshot is backend visibility skew, not impossible carrier accounting and not recovery.",
			action:    "Restore complete scrape ingestion and wait for a coherent fresh sample. Preserve the raw series privately; inspect carrier code only if a same-scrape set still violates its invariants.",
			verify:    "All required values share one source timestamp for two consecutive scrapes and Mimir admission controls remain healthy.",
			playbook:  "SIGNALS.md §14.6",
		})
	}
	if len(invalid) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-transport-budget", tier: tierWarn,
			class: "proxy-transport-metrics-invalid", target: "proxy-fleet", frame: metricHost.name, sustain: 2,
			symptom:   fmt.Sprintf("%d newest fresh proxy identities export internally inconsistent carrier accounting", len(invalid)),
			mechanism: "One same-scrape SDK aggregate violates a count, byte, or pending-device invariant. That can be exporter/label drift or an accounting bug; it cannot safely support a capacity or preemption diagnosis.",
			baseline:  "Counts are integral and nonnegative; used carriers do not exceed the count cap; pending count and bytes agree on zero; slot-full pending devices do not exceed live devices; and an empty device set has no live carrier allocation.",
			observed:  fmt.Sprintf("invalid_identities=%d invalid=%s metrics_gateway=%s", len(invalid), strings.Join(invalid, ";"), metricHost.name),
			evidence:  "Every compared value belongs to the same exact process identity and producer scrape.",
			context:   "Do not call this a shared budget, a provider failure, or host capacity pressure until the metric contract is valid.",
			action:    "Inspect the deployed SDK and Proxy exporter at this exact generation and correct the first accounting or label boundary. Do not raise budgets or restart away the evidence.",
			verify:    "The invariants hold on every newest identity for two consecutive scrapes, then the isolation and admission checks can run.",
			playbook:  "SIGNALS.md §14.6",
		})
	}
	if len(isolation) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-transport-budget", tier: tierWarn,
			class: "proxy-transport-budget-isolation", target: "proxy-fleet", frame: metricHost.name, sustain: 2,
			symptom:   fmt.Sprintf("%d newest fresh proxy identities do not scale the carrier budget independently with their hosted DeviceLocals", len(isolation)),
			mechanism: "Every hosted DeviceLocal must own one target-derived carrier budget with sixteen carrier slots. A zero target, a flat sixteen-slot process cap across multiple devices, or an aggregate byte budget inconsistent with the per-device target recreates the shared admission domain that previously parked later H1 carriers behind unrelated devices.",
			baseline:  "For D live devices, max carriers equals D*16 and aggregate carrier bytes equal D*min(per-device target, max(3 MiB, per-device target/4)); the aggregate target is positive and uniform.",
			observed:  fmt.Sprintf("isolation_failures=%d failures=%s metrics_gateway=%s", len(isolation), strings.Join(isolation, ";"), metricHost.name),
			evidence:  "The relationship is evaluated on a same-scrape aggregate and does not retain DeviceLocal or customer identity.",
			context:   "This is a software correctness/deployment failure, not proof that the proxy fleet needs more hardware. Host memory and the total active-client ceiling remain §14.7.",
			action:    "Deploy a Proxy/SDK build that keeps each positive DeviceLocal memory target and constructs a private PlatformTransportBudget from it. Do not compensate by raising the process-global budget or restarting the same artifact.",
			verify:    "Every newest Proxy generation satisfies both scaling equations for two consecutive scrapes and ordinary HTTP/SOCKS/WireGuard acceptance remains healthy.",
			playbook:  "SIGNALS.md §14.6",
		})
	}
	if len(churn) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-transport-budget", tier: tierWarn,
			class: "proxy-transport-preemption-churn", target: "proxy-fleet", frame: metricHost.name, sustain: 2,
			symptom:   fmt.Sprintf("%d newest fresh proxy identities combine recent H3 preemptions, sampled slot-full H1 admission, and active CPU", len(churn)),
			mechanism: "Before Connect f10a173, an H1 claim with both byte and slot deficits could preempt a slotless Auto-H3 lease merely because it freed bytes. H3 then reacquired because the H1 still could not fit, producing an endless yield/reacquire loop. Slot-full pending-device count, an advancing H3-preemption counter, and process CPU identify a similar pressure pattern without transport or customer labels. Sampling cannot establish that every preemption occurred while the carrier slots were full.",
			baseline:  fmt.Sprintf("No two-minute window combines a slot-full pending DeviceLocal, at least %.1f H3 preemptions/second, and at least %.1f CPU cores on the same newest Proxy identity.", proxyTransportChurnRate, proxyTransportChurnCpuCores),
			observed:  fmt.Sprintf("churning_identities=%d churn=%s metrics_gateway=%s", len(churn), strings.Join(churn, ";"), metricHost.name),
			evidence:  "The CPU rate uses the process counter over two minutes. The preemption rate is the process-monotonic sampled subset whose increment and slot-full pending state came from the same DeviceLocal observation; a new/reset device's first value is excluded because its history cannot be joined safely. A device removed between samples can make this detector undercount. The same device can also preempt normally before becoming slot-full within a sampling interval, so a positive rate does not prove event-time saturation.",
			context:   "This is a suspected software loop resembling urnetwork/connect#211. Confirm the event ordering before distinguishing it from legitimate fixed-code transitions; §14.7 owns hardware-backed fleet capacity.",
			action:    "Prove the running Connect input. If it lacks f10a173, build and deploy the affected Proxy-bearing artifact with f10a173/ab74d62 or a descendant through the ordinary serialized rollout. If it contains the fix, preserve the generation and correlate preemption-time slot state with the blocked transition before attributing a software defect or changing policy.",
			verify:    "For ten minutes under comparable multi-device load, the preemption rate returns to zero, CPU returns to its traffic baseline, pending H1 drains, and concurrent protocol acceptance remains healthy.",
			playbook:  "SIGNALS.md §14.6 and urnetwork/connect#211",
		})
	}
	if len(pending) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-transport-budget", tier: tierWarn,
			class: "proxy-transport-admission-pending", target: "proxy-fleet", frame: metricHost.name, sustain: 2,
			symptom:   fmt.Sprintf("%d newest fresh proxy identities retain H1 carrier claims waiting for private DeviceLocal admission", len(pending)),
			mechanism: "A positive pending-H1 count means at least one private DeviceLocal budget cannot currently admit the requested H1 carrier by bytes or slots. A brief overlap can be normal; persistence across two one-minute probes requires correlation with that window's readiness and transport-policy transition.",
			baseline:  "Pending H1 count and bytes return to zero within one probe cadence; no hosted window remains unsatisfied behind carrier admission.",
			observed:  fmt.Sprintf("pending_identities=%d pending=%s metrics_gateway=%s", len(pending), strings.Join(pending, ";"), metricHost.name),
			evidence:  "The aggregate preserves pending count/bytes and slot-full DeviceLocal count without retaining device identity. CPU and preemption rates are included when the two-minute range is available.",
			context:   "This is capacity pressure, not by itself the connect#211 loop, a provider outage, or a hardware requirement. Per-device carrier admission and host-wide proxy capacity have different owners.",
			action:    "Correlate the exact generation with unsatisfied-window and retry evidence. On old/shared-budget code deploy private budgets; on current code diagnose the blocked mode transition or genuinely excessive per-device carrier demand. Do not blindly raise the cap, restart, or add hardware from this aggregate alone.",
			verify:    "Pending count and bytes remain zero for ten minutes under comparable load, windows recover their minimum, and no preemption-churn alert appears.",
			playbook:  "SIGNALS.md §14.6",
		})
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("runtime/proxy-transport-budget", tierWarn, "proxy-transport-admission-pending", "proxy-fleet")}, nil
	}
	return findings, nil
}

// Selects the latest start within each host/block and keeps unplaced instances
// separate when no block label establishes their shared slot.
func newestProxyTransportProcesses(processes map[string]*proxyTransportMetrics) ([]*proxyTransportMetrics, error) {
	newest := map[string]*proxyTransportMetrics{}
	for _, process := range processes {
		rss, hasRss := process.values["process_resident_memory_bytes"]
		if !hasRss || rss <= 0 {
			continue
		}
		start, hasStart := process.values["process_start_time_seconds"]
		if !hasStart {
			return nil, fmt.Errorf("proxy transport: fresh RSS identity %s omitted process_start_time_seconds", proxyTransportLabel(process))
		}
		slot := process.host + "\x00" + process.block
		if process.block == "" {
			slot += "\x00" + process.instance
		}
		previous := newest[slot]
		if previous == nil || start > previous.values["process_start_time_seconds"] ||
			(start == previous.values["process_start_time_seconds"] && process.instance > previous.instance) {
			newest[slot] = process
		}
	}
	current := make([]*proxyTransportMetrics, 0, len(newest))
	for _, process := range newest {
		current = append(current, process)
	}
	return current, nil
}

// Formats only the process identity already supplied by the metrics gateway.
func proxyTransportLabel(process *proxyTransportMetrics) string {
	label := process.host
	if process.block != "" {
		label += "/" + process.block
	}
	if process.instance != "" {
		label += "#" + process.instance
	}
	return label
}

// Treats absent required telemetry as unknown admission state.
func proxyTransportMissingMetrics(process *proxyTransportMetrics) []string {
	missing := []string{}
	for _, metricName := range proxyTransportMetricNames {
		if _, ok := process.values[metricName]; !ok {
			missing = append(missing, metricName)
		}
	}
	return missing
}

// Rejects joins across producer scrapes before comparing aggregate invariants.
func proxyTransportSnapshotReason(process *proxyTransportMetrics) string {
	var sourceTime float64
	for i, metricName := range proxyTransportMetricNames {
		at, ok := process.sourceTimes[metricName]
		if !ok {
			return "missing-source-time=" + metricName
		}
		if i == 0 {
			sourceTime = at
		} else if at != sourceTime {
			return "source-time-skew"
		}
	}
	return ""
}

// Validates aggregate count and pending-state invariants before diagnosis.
func proxyTransportInvalidReason(process *proxyTransportMetrics) string {
	counts := []string{
		"urnetwork_proxy_devices_live",
		"urnetwork_proxy_platform_transports_max",
		"urnetwork_proxy_platform_transports_used",
		"urnetwork_proxy_platform_transports_pending_h1",
		"urnetwork_proxy_platform_transport_slot_full_pending_h1_devices",
	}
	for _, metricName := range counts {
		value := process.values[metricName]
		if value != math.Trunc(value) || value > float64(1<<53) {
			return "non-integral-or-unrepresentable=" + metricName
		}
	}
	devices := process.values["urnetwork_proxy_devices_live"]
	used := process.values["urnetwork_proxy_platform_transports_used"]
	maximum := process.values["urnetwork_proxy_platform_transports_max"]
	pending := process.values["urnetwork_proxy_platform_transports_pending_h1"]
	pendingBytes := process.values["urnetwork_proxy_platform_transports_pending_h1_bytes"]
	slotFull := process.values["urnetwork_proxy_platform_transport_slot_full_pending_h1_devices"]
	if used > maximum {
		return "used-carriers-exceed-maximum"
	}
	if slotFull > devices {
		return "slot-full-pending-devices-exceed-live-devices"
	}
	if (pending == 0) != (pendingBytes == 0) {
		return "pending-count-byte-zero-mismatch"
	}
	if slotFull > 0 && pending == 0 {
		return "slot-full-device-without-pending-h1"
	}
	if devices == 0 {
		for _, metricName := range []string{
			"urnetwork_proxy_device_memory_target_bytes",
			"urnetwork_proxy_platform_transport_budget_bytes",
			"urnetwork_proxy_platform_transport_used_bytes",
			"urnetwork_proxy_platform_transports_max",
			"urnetwork_proxy_platform_transports_used",
			"urnetwork_proxy_platform_transports_pending_h1",
			"urnetwork_proxy_platform_transports_pending_h1_bytes",
			"urnetwork_proxy_platform_transport_slot_full_pending_h1_devices",
		} {
			if process.values[metricName] != 0 {
				return "empty-device-set-has-live-carrier-state=" + metricName
			}
		}
	}
	return ""
}

// Checks private target-derived budgets for the uniform hosted-device policy.
func proxyTransportIsolationReason(process *proxyTransportMetrics) string {
	devices := process.values["urnetwork_proxy_devices_live"]
	if devices == 0 {
		return ""
	}
	target := process.values["urnetwork_proxy_device_memory_target_bytes"]
	if target <= 0 || target != math.Trunc(target) || math.Mod(target, devices) != 0 {
		return "device-target-not-positive-uniform-integer"
	}
	perDeviceTarget := target / devices
	expectedBudgetPerDevice := math.Min(
		perDeviceTarget,
		math.Max(proxyTransportMinimumBudgetPerDevice, math.Floor(perDeviceTarget/4)),
	)
	expectedBudget := devices * expectedBudgetPerDevice
	if process.values["urnetwork_proxy_platform_transport_budget_bytes"] != expectedBudget {
		return fmt.Sprintf("budget-bytes=%g-want=%g", process.values["urnetwork_proxy_platform_transport_budget_bytes"], expectedBudget)
	}
	expectedMaximum := devices * proxyTransportPerDeviceCount
	if process.values["urnetwork_proxy_platform_transports_max"] != expectedMaximum {
		return fmt.Sprintf("max-carriers=%g-want=%g", process.values["urnetwork_proxy_platform_transports_max"], expectedMaximum)
	}
	return ""
}

// Formats bounded aggregate values and marks missing rates explicitly.
func proxyTransportObservation(process *proxyTransportMetrics) string {
	value := func(metricName string) string {
		if observed, ok := process.values[metricName]; ok {
			return strconv.FormatFloat(observed, 'g', -1, 64)
		}
		return "unknown"
	}
	return fmt.Sprintf(
		"%s:devices=%s,target_bytes=%s,budget_bytes=%s,used_bytes=%s,max=%s,used=%s,pending_h1=%s,pending_h1_bytes=%s,slot_full_pending_devices=%s,h3_preemptions_total=%s,slot_full_h3_preemptions_total=%s,slot_full_h3_preemptions_per_second=%s,cpu_cores=%s",
		proxyTransportLabel(process),
		value("urnetwork_proxy_devices_live"),
		value("urnetwork_proxy_device_memory_target_bytes"),
		value("urnetwork_proxy_platform_transport_budget_bytes"),
		value("urnetwork_proxy_platform_transport_used_bytes"),
		value("urnetwork_proxy_platform_transports_max"),
		value("urnetwork_proxy_platform_transports_used"),
		value("urnetwork_proxy_platform_transports_pending_h1"),
		value("urnetwork_proxy_platform_transports_pending_h1_bytes"),
		value("urnetwork_proxy_platform_transport_slot_full_pending_h1_devices"),
		value("urnetwork_proxy_platform_transport_h3_preemptions_total"),
		value("urnetwork_proxy_platform_transport_slot_full_pending_h1_h3_preemptions_total"),
		value(proxyTransportPreemptionRateMetric),
		value(proxyTransportCpuRateMetric),
	)
}
