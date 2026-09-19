package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	serviceLoadFreshness                   = 90 * time.Second
	serviceLoadRSSFloorBytes               = float64(16 << 30)
	serviceLoadRSSHardBytes                = float64(64 << 30)
	serviceLoadGoroutineFloor              = float64(250_000)
	serviceLoadGoroutineHard               = float64(500_000)
	serviceLoadAllocationFloor             = float64(256 << 20)
	serviceLoadAllocationHard              = float64(1 << 30)
	serviceLoadMinimumCPUCoreRate          = 1.0
	serviceLoadMaximumCPUCoreRate          = 4.0
	serviceLoadHostCPUFraction             = 0.0625
	serviceLoadLazyForwardCommit           = "2425b71e71bff58448b6e26c84a0188871364409"
	serviceLoadLazyForwardCapabilityCommit = "7ed9a3065bb2e62653d0681b26527f56fb4001fe"
	// A service-load vector has a fixed metric set and bounded identities.  Do
	// not permit one unhealthy Mimir response to retain an unbounded body in
	// the serial monitor runner before JSON validation.
	serviceLoadResponseMaxBytes = 4 << 20
)

// Signal service-load implements SIGNALS.md §8.15. It attributes a saturated
// host to fresh process identities without suppressing an old draining
// generation merely because a replacement for the same block is also live.
func NewServiceLoadSignal() Signal {
	return &signalAdapter{
		number: "8.15", key: "service-load", name: "Per-service runtime runaway",
		probe: serviceLoadProbe{},
	}
}

type serviceLoadProbe struct{}

func (serviceLoadProbe) id() string             { return "runtime/service-runaway" }
func (serviceLoadProbe) tier() string           { return tierPage }
func (serviceLoadProbe) cadence() time.Duration { return time.Minute }

const (
	serviceLoadMetricRSS uint16 = 1 << iota
	serviceLoadMetricHeap
	serviceLoadMetricObjects
	serviceLoadMetricGoroutines
	serviceLoadMetricStart
	serviceLoadMetricCPU
	serviceLoadMetricAllocation
	serviceLoadMetricGC
	serviceLoadMetricLazyForwardIngress
	serviceLoadMetricRawAll = serviceLoadMetricRSS | serviceLoadMetricHeap |
		serviceLoadMetricObjects | serviceLoadMetricGoroutines | serviceLoadMetricStart
)

type serviceLoadMetrics struct {
	host               string
	service            string
	block              string
	instance           string
	rss                float64
	heap               float64
	objects            float64
	goroutines         float64
	start              float64
	cpu                float64
	allocation         float64
	gc                 float64
	lazyForwardIngress float64
	residents          *serviceLoadResidentSample
	mask               uint16
}

type serviceLoadResidentSample struct {
	value   float64
	count   int
	invalid bool
}

func serviceLoadServicePattern(services []string) string {
	unique := map[string]bool{}
	for _, service := range services {
		service = strings.TrimSpace(service)
		if service != "" {
			unique[service] = true
		}
	}
	ordered := make([]string, 0, len(unique))
	for service := range unique {
		ordered = append(ordered, regexp.QuoteMeta(service))
	}
	sort.Strings(ordered)
	if len(ordered) == 0 {
		return ".+"
	}
	return "(?:" + strings.Join(ordered, "|") + ")"
}

func serviceLoadQuery(environment string, services []string) string {
	env := strconv.Quote(environment)
	jobs := strconv.Quote(serviceLoadServicePattern(services))
	freshness := int64(serviceLoadFreshness / time.Second)
	parts := []string{}
	for _, metric := range []string{
		"process_resident_memory_bytes",
		"go_memstats_heap_alloc_bytes",
		"go_memstats_heap_objects",
		"go_goroutines",
		"process_start_time_seconds",
	} {
		selector := fmt.Sprintf(`%s{env=%s,job=~%s}`, metric, env, jobs)
		fresh := fmt.Sprintf(
			`(%s and (timestamp(%s) >= time() - %d))`,
			selector,
			selector,
			freshness,
		)
		parts = append(parts, fmt.Sprintf(
			`label_replace(%s,"monitor_metric",%s,"job",".*")`,
			fresh,
			strconv.Quote(metric),
		))
	}
	for _, rate := range []struct {
		metric string
		name   string
	}{
		{metric: "process_cpu_seconds_total", name: "cpu"},
		{metric: "go_memstats_alloc_bytes_total", name: "allocation"},
		{metric: "go_gc_duration_seconds_count", name: "gc"},
	} {
		selector := fmt.Sprintf(`%s{env=%s,job=~%s}`, rate.metric, env, jobs)
		freshRate := fmt.Sprintf(
			`(rate(%s[5m]) and (timestamp(%s) >= time() - %d))`,
			selector,
			selector,
			freshness,
		)
		parts = append(parts, fmt.Sprintf(
			`label_replace(%s,"monitor_metric",%s,"job",".*")`,
			freshRate,
			strconv.Quote(rate.name),
		))
	}
	if len(services) == 0 || slices.Contains(services, "connect") {
		for _, metric := range []struct {
			metric string
			name   string
		}{
			{metric: "urnetwork_connect_resident_lazy_forward_ingress_enabled", name: "lazy_forward_ingress"},
			{metric: "urnetwork_connect_resident_clients", name: "resident_clients"},
		} {
			selector := fmt.Sprintf(`%s{env=%s,job="connect"}`, metric.metric, env)
			fresh := fmt.Sprintf(
				`(%s and (timestamp(%s) >= time() - %d))`,
				selector,
				selector,
				freshness,
			)
			parts = append(parts, fmt.Sprintf(
				`label_replace(%s,"monitor_metric",%s,"job",".*")`,
				fresh,
				strconv.Quote(metric.name),
			))
		}
	}
	nodeSelector := fmt.Sprintf(`node_cpu_seconds_total{env=%s,job="node"}`, env)
	freshNodeCPU := fmt.Sprintf(
		`(%s and (timestamp(%s) >= time() - %d))`,
		nodeSelector,
		nodeSelector,
		freshness,
	)
	parts = append(parts, fmt.Sprintf(
		`label_replace(count without(cpu) (count without(mode) (%s)),"monitor_metric","host_cores","job",".*")`,
		freshNodeCPU,
	))
	return strings.Join(parts, " or ")
}

func safeServiceLoadLabel(value string) bool {
	return value != "" && len(value) <= 128 && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n/\\")
}

func serviceLoadCPUThreshold(hostCores float64) float64 {
	threshold := hostCores * serviceLoadHostCPUFraction
	if threshold < serviceLoadMinimumCPUCoreRate {
		return serviceLoadMinimumCPUCoreRate
	}
	if threshold > serviceLoadMaximumCPUCoreRate {
		return serviceLoadMaximumCPUCoreRate
	}
	return threshold
}

func serviceLoadRunaway(process *serviceLoadMetrics, hostCores float64) bool {
	cpuHot := process.mask&serviceLoadMetricCPU != 0 && process.cpu >= serviceLoadCPUThreshold(hostCores)
	allocationHot := process.mask&serviceLoadMetricAllocation != 0 && process.allocation >= serviceLoadAllocationFloor
	largeShape := process.rss >= serviceLoadRSSFloorBytes &&
		(cpuHot || process.goroutines >= serviceLoadGoroutineFloor || allocationHot)
	return largeShape || process.rss >= serviceLoadRSSHardBytes ||
		process.goroutines >= serviceLoadGoroutineHard ||
		(process.mask&serviceLoadMetricAllocation != 0 && process.allocation >= serviceLoadAllocationHard)
}

func (serviceLoadProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("service load: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(serviceLoadQuery(env.cfg.env, env.cfg.logServices))
	out, gateway, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 --max-filesize "+strconv.Itoa(serviceLoadResponseMaxBytes)+" '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("service load: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("service load: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"service load: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	expectedHosts := make(map[string]bool, len(env.cfg.hosts))
	for _, configured := range env.cfg.hosts {
		expectedHosts[configured.name] = true
	}
	expectedServices := map[string]bool{}
	for _, service := range env.cfg.logServices {
		expectedServices[service] = true
	}
	now := env.now().UTC()
	processes := map[string]*serviceLoadMetrics{}
	residentSamples := map[string]*serviceLoadResidentSample{}
	hostCores := map[string]float64{}
	invalidSeries := 0
	for _, series := range response.Data.Result {
		metric := series.Metric["monitor_metric"]
		residentMetric := metric == "resident_clients"
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil && !residentMetric {
			return nil, fmt.Errorf("service load: parse fixed metric sample: %w", err)
		}
		age := now.Sub(observedAt)
		if err == nil && (age > serviceLoadFreshness || age < -30*time.Second) {
			continue
		}
		invalidValue := err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0
		if invalidValue && !residentMetric {
			return nil, fmt.Errorf("service load: fixed metric value is invalid")
		}
		host := series.Metric["host"]
		if metric == "host_cores" {
			if expectedHosts[host] && value >= 1 {
				hostCores[host] = value
			}
			continue
		}
		service := series.Metric["service"]
		if service == "" {
			service = series.Metric["job"]
		}
		block := series.Metric["block"]
		instance := series.Metric["instance"]
		if !expectedHosts[host] || (len(expectedServices) != 0 && !expectedServices[service]) ||
			!safeServiceLoadLabel(service) || !safeServiceLoadLabel(block) || !safeServiceLoadLabel(instance) {
			invalidSeries++
			continue
		}
		key := host + "\x00" + service + "\x00" + block + "\x00" + instance
		if residentMetric {
			if service != "connect" {
				invalidSeries++
				continue
			}
			sample := residentSamples[key]
			if sample == nil {
				sample = &serviceLoadResidentSample{}
				residentSamples[key] = sample
			}
			sample.count++
			sample.value = value
			sample.invalid = sample.invalid || invalidValue || math.Trunc(value) != value ||
				value > 1<<53-1 || series.Metric["job"] != "connect" || series.Metric["env"] != env.cfg.env
			continue
		}
		process := processes[key]
		if process == nil {
			process = &serviceLoadMetrics{host: host, service: service, block: block, instance: instance}
			processes[key] = process
		}
		switch metric {
		case "process_resident_memory_bytes":
			process.rss = value
			process.mask |= serviceLoadMetricRSS
		case "go_memstats_heap_alloc_bytes":
			process.heap = value
			process.mask |= serviceLoadMetricHeap
		case "go_memstats_heap_objects":
			process.objects = value
			process.mask |= serviceLoadMetricObjects
		case "go_goroutines":
			process.goroutines = value
			process.mask |= serviceLoadMetricGoroutines
		case "process_start_time_seconds":
			process.start = value
			process.mask |= serviceLoadMetricStart
		case "cpu":
			process.cpu = value
			process.mask |= serviceLoadMetricCPU
		case "allocation":
			process.allocation = value
			process.mask |= serviceLoadMetricAllocation
		case "gc":
			process.gc = value
			process.mask |= serviceLoadMetricGC
		case "lazy_forward_ingress":
			process.lazyForwardIngress = value
			process.mask |= serviceLoadMetricLazyForwardIngress
		}
	}
	// Optional population telemetry must never create a phantom process or
	// suppress a resource PAGE when its denominator is absent or malformed.
	for key, process := range processes {
		process.residents = residentSamples[key]
	}

	if len(processes) == 0 {
		return nil, fmt.Errorf("service load: Mimir returned no fresh managed-service process samples")
	}
	findings := []finding{}
	if invalidSeries != 0 {
		findings = append(findings, finding{
			probeId: "runtime/service-runaway", tier: tierWarn,
			class: "service-runtime-metrics-invalid", target: "managed-services", sustain: 2,
			symptom:   "Fresh process telemetry contained series outside the bounded inventory and label contract.",
			mechanism: "Malformed or unowned process identity prevents safe per-generation attribution; raw labels are discarded rather than copied into an alert.",
			baseline:  "Every selected process series has an enabled inventory host and bounded configured service, block, and instance labels.",
			observed:  fmt.Sprintf("invalid_series=%d", invalidSeries),
			evidence:  "Only a count is retained from rejected series. Query gateway=" + gateway.name + ".",
			action:    "Inspect the producer relabel configuration and active services inventory, then restore bounded host/service/block/instance identity before using the affected samples for diagnosis.",
			verify:    "Two consecutive runs reject zero fresh process series.",
			playbook:  "SIGNALS.md §8.15",
		})
	} else {
		findings = append(findings, healthyFinding("runtime/service-runaway", tierWarn, "service-runtime-metrics-invalid", "managed-services"))
	}
	findings = append(findings, serviceLoadConnectCapabilityFinding(processes, expectedServices["connect"]))
	findings = append(findings, serviceLoadConnectResidentCostFinding(processes, expectedServices["connect"]))

	overlaps := map[string]int{}
	for _, process := range processes {
		if process.mask&serviceLoadMetricRSS != 0 {
			overlaps[process.host+"\x00"+process.service+"\x00"+process.block]++
		}
	}
	ordered := make([]string, 0, len(processes))
	for key := range processes {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		process := processes[key]
		target := process.host + "/" + process.service + "/" + process.block
		if process.mask&serviceLoadMetricRawAll != serviceLoadMetricRawAll || process.start <= 0 {
			findings = append(findings, finding{
				probeId: "runtime/service-runaway", tier: tierWarn,
				class: "service-runtime-metrics-incomplete", target: target, frame: process.instance, sustain: 2,
				symptom:   "A fresh managed-service process lacks the runtime family required to attribute host load.",
				mechanism: "Partial process telemetry can hide a runaway generation or combine incompatible samples; an RSS-only process is not treated as healthy.",
				baseline:  fmt.Sprintf("raw_metric_mask=%02x", serviceLoadMetricRawAll),
				observed:  fmt.Sprintf("available_metric_mask=%02x", process.mask),
				evidence:  "The target is an exact bounded process identity from configured service telemetry; no command line or application payload is retained.",
				action:    "Verify the exact artifact exports process RSS/start plus Go heap/object/goroutine metrics and that relabeling preserves one coherent host/service/block/instance tuple.",
				verify:    "Two consecutive runs observe the complete raw runtime family for this process identity.",
				playbook:  "SIGNALS.md §8.15",
			})
			continue
		}
		findings = append(findings, healthyFinding("runtime/service-runaway", tierWarn, "service-runtime-metrics-incomplete", target))

		cores := hostCores[process.host]
		if cores < 1 {
			cores = serviceLoadMaximumCPUCoreRate / serviceLoadHostCPUFraction
		}
		if !serviceLoadRunaway(process, cores) {
			findings = append(findings, healthyFinding("runtime/service-runaway", tierPage, "service-runtime-runaway", target))
			continue
		}
		processAge := math.Max(0, float64(now.Unix())-process.start)
		blockKey := process.host + "\x00" + process.service + "\x00" + process.block
		observedCapability := ""
		contextText := "This is affirmative process ownership, not proof of the allocation call site. If bounded per-client state is legitimately at its designed ceiling, operations must reduce traffic or add capable hardware; unbounded goroutine/queue fan-out or retained retired state requires a software fix."
		actionText := "Inspect the service-specific resident/connection/queue/drain metrics and the source defaults that scale per client. Preserve the process generation and bounded profile evidence. Hard-terminate only a proven-stuck old generation after its replacement is ready, one block at a time; do not reboot the host or kill the current owner blindly."
		verifyText := "Two consecutive runs place every process below the runtime band and §8.14 returns healthy while service traffic and rollout convergence remain healthy."
		if process.service == "connect" {
			capability := "missing"
			if process.mask&serviceLoadMetricLazyForwardIngress != 0 {
				if process.lazyForwardIngress == 1 {
					capability = "enabled"
				} else {
					capability = "invalid"
				}
			}
			observedCapability = " resident_lazy_forward_ingress_capability=" + capability + " " + serviceLoadResidentCostEvidence(process)
			contextText += " Server commit " + serviceLoadLazyForwardCommit + " replaces the eager sixteen-shard worker/queue multiplier with first-destination construction, and commit " + serviceLoadLazyForwardCapabilityCommit + " exports its executable-owned capability gauge. A capability value of one proves that behavior in the exact executable; an absent value is unknown under a modified build and does not by itself prove legacy code."
			contextText += " Resident-normalized RSS, heap, and goroutines describe this exact generation's total process cost divided by its current resident population; they do not isolate resident allocations, establish a leak or causal call site, or change the PAGE thresholds. Compare matched controls and traffic before distinguishing population growth from inflated per-resident cost; zero residents leaves ratios undefined while process state remains real."
			actionText = "Use the fleet capability finding and §8.12 provenance first. Deploy Connect from an intentional checkout containing behavior commit " + serviceLoadLazyForwardCommit + " and capability commit " + serviceLoadLazyForwardCapabilityCommit + " only to newest artifacts that do not prove the capability. If the exact runaway generation reports capability=enabled, retain it long enough for a bounded aggregate profile and locate the remaining resident, transport, forward, or retained-generation owner before changing limits. Do not infer source ancestry from an unavailable modified base, reboot the host, or kill the current owner blindly."
			verifyText = "Every newest Connect identity reports resident_lazy_forward_ingress_capability=enabled, then two consecutive runs place every process below the runtime band and §8.14 returns healthy while resident count, service traffic, and rollout convergence remain healthy."
		}
		findings = append(findings, finding{
			probeId: "runtime/service-runaway", tier: tierPage,
			class: "service-runtime-runaway", target: target, frame: process.instance, sustain: 2,
			symptom:   "One managed-service process has a runaway runtime shape capable of saturating its host.",
			mechanism: "A large live process combined with multi-core CPU, extreme allocation, or hundreds of thousands of goroutines creates scheduler and garbage-collector pressure. During a rollout, an old generation can amplify the same load while its replacement accepts work.",
			baseline:  fmt.Sprintf("rss_bytes < %.0f or cpu_cores_5m < %.2f, goroutines < %.0f, and alloc_bytes_per_s_5m < %.0f; hard ceilings rss_bytes < %.0f, goroutines < %.0f, alloc_bytes_per_s_5m < %.0f", serviceLoadRSSFloorBytes, serviceLoadCPUThreshold(cores), serviceLoadGoroutineFloor, serviceLoadAllocationFloor, serviceLoadRSSHardBytes, serviceLoadGoroutineHard, serviceLoadAllocationHard),
			observed: fmt.Sprintf(
				"rss_bytes=%.0f heap_bytes=%.0f heap_objects=%.0f goroutines=%.0f cpu_cores_5m=%.3f alloc_bytes_per_s_5m=%.0f gc_cycles_per_s_5m=%.4f process_age_s=%.0f host_logical_cpus=%.0f same_block_generations=%d",
				process.rss,
				process.heap,
				process.objects,
				process.goroutines,
				process.cpu,
				process.allocation,
				process.gc,
				processAge,
				cores,
				overlaps[blockKey],
			) + observedCapability,
			evidence: "Fresh runtime gauges and five-minute counter rates were joined on the exact host/service/block/instance identity through gateway " + gateway.name + ". Draining generations remain visible by design.",
			context:  contextText,
			action:   actionText,
			verify:   verifyText,
			playbook: "SIGNALS.md §8.15",
		})
	}
	return findings, nil
}

func serviceLoadNewestConnectProcesses(processes map[string]*serviceLoadMetrics) (map[string]*serviceLoadMetrics, int) {
	newest := map[string]*serviceLoadMetrics{}
	ambiguous := map[string]bool{}
	unselectable := 0
	for _, process := range processes {
		if process.service != "connect" || process.mask&serviceLoadMetricRSS == 0 {
			continue
		}
		if process.mask&serviceLoadMetricStart == 0 || process.start <= 0 {
			unselectable++
			continue
		}
		slot := process.host + "\x00" + process.block
		previous := newest[slot]
		if previous == nil || process.start > previous.start {
			newest[slot] = process
			ambiguous[slot] = false
		} else if process.start == previous.start && process.instance != previous.instance {
			ambiguous[slot] = true
		}
	}
	for slot, mixed := range ambiguous {
		if mixed {
			delete(newest, slot)
			unselectable++
		}
	}
	return newest, unselectable
}

func serviceLoadConnectCapabilityFinding(processes map[string]*serviceLoadMetrics, expected bool) finding {
	newest, unselectable := serviceLoadNewestConnectProcesses(processes)
	enabled, missing, invalid := 0, 0, 0
	for _, process := range newest {
		switch {
		case process.mask&serviceLoadMetricLazyForwardIngress == 0:
			missing++
		case process.lazyForwardIngress != 1:
			invalid++
		default:
			enabled++
		}
	}
	if (!expected || len(newest) != 0) && missing == 0 && invalid == 0 && unselectable == 0 {
		return healthyFinding("runtime/service-runaway", tierWarn, "connect-resident-ingress-capability-unobservable", "connect-fleet")
	}
	return finding{
		probeId: "runtime/service-runaway", tier: tierWarn,
		class: "connect-resident-ingress-capability-unobservable", target: "connect-fleet", sustain: 1,
		symptom:   "The newest Connect fleet cannot prove the lazy resident forward-ingress capability.",
		mechanism: "Server commit " + serviceLoadLazyForwardCommit + " replaces sixteen eager per-resident forward shard queues and consumers with first-destination construction; commit " + serviceLoadLazyForwardCapabilityCommit + " adds the executable-owned gauge. That gauge proves the behavior even for an intentional modified build whose base revision is unavailable; missing or non-one evidence leaves the principal runtime correction unknown.",
		baseline:  "Every newest fresh Connect process reports urnetwork_connect_resident_lazy_forward_ingress_enabled=1 on its exact process identity.",
		observed:  fmt.Sprintf("newest_connect_processes=%d capability_enabled=%d capability_missing=%d capability_invalid=%d generation_unselectable=%d", len(newest), enabled, missing, invalid, unselectable),
		evidence:  "Only fixed fleet counts leave the Mimir join. Source revision, modified state, and image digest remain independently governed by §8.12; no client, resident, transport, command line, or raw label is retained.",
		context:   "An absent gauge can mean a legacy artifact or metric-delivery loss and is not proof that eager construction executed. Conversely, a value of one proves the lazy-shard code path exists but does not prove every remaining per-resident allocation is bounded or that runtime pressure recovered.",
		action:    "Use §8.12 to prove the newest artifact and metric delivery. Deploy Connect from an intentional checkout containing behavior commit " + serviceLoadLazyForwardCommit + " and capability commit " + serviceLoadLazyForwardCapabilityCommit + " only where the capability is absent because the artifact predates them; otherwise repair telemetry. If capability-proven processes still run away, preserve a bounded aggregate profile and attribute the remaining resident/transport/forward owner before changing limits.",
		verify:    "For two consecutive scrapes every newest Connect identity reports capability=1 with valid provenance; after convergence, two service-load cadences stay below the runtime band while §8.14 and Connect traffic remain healthy.",
		playbook:  "SIGNALS.md §8.15",
	}
}

func serviceLoadResidentCostStatus(process *serviceLoadMetrics) string {
	sample := process.residents
	switch {
	case sample == nil:
		return "missing"
	case sample.count != 1:
		return "mixed"
	case sample.invalid:
		return "invalid"
	case sample.value == 0:
		return "zero"
	default:
		return "available"
	}
}

func serviceLoadResidentCostEvidence(process *serviceLoadMetrics) string {
	status := serviceLoadResidentCostStatus(process)
	switch status {
	case "available":
		count := process.residents.value
		return fmt.Sprintf("resident_count_status=available resident_count=%.0f rss_bytes_per_resident=%.2f heap_bytes_per_resident=%.2f goroutines_per_resident=%.3f", count, process.rss/count, process.heap/count, process.goroutines/count)
	case "zero":
		return "resident_count_status=zero resident_count=0 resident_cost_ratios=undefined"
	default:
		return "resident_count_status=" + status + " resident_cost_ratios=unobservable"
	}
}

func serviceLoadConnectResidentCostFinding(processes map[string]*serviceLoadMetrics, expected bool) finding {
	newest, unselectable := serviceLoadNewestConnectProcesses(processes)
	available, zero, missing, mixed, invalid := 0, 0, 0, 0, 0
	for _, process := range newest {
		switch serviceLoadResidentCostStatus(process) {
		case "available":
			available++
		case "zero":
			zero++
		case "missing":
			missing++
		case "mixed":
			mixed++
		case "invalid":
			invalid++
		}
	}
	if (!expected || len(newest) != 0) && missing == 0 && mixed == 0 && invalid == 0 && unselectable == 0 {
		return healthyFinding("runtime/service-runaway", tierWarn, "connect-resident-cost-unobservable", "connect-fleet")
	}
	return finding{
		probeId: "runtime/service-runaway", tier: tierWarn,
		class: "connect-resident-cost-unobservable", target: "connect-fleet", sustain: 1,
		symptom:   "The newest Connect fleet lacks an unambiguous fresh resident-count join for runtime diagnosis.",
		mechanism: "Absent, stale, mixed, or invalid population telemetry prevents a trustworthy process-cost denominator; it does not establish a product failure and never suppresses the independent raw runtime PAGE.",
		baseline:  "Every newest fresh Connect process has exactly one fresh finite nonnegative integer urnetwork_connect_resident_clients sample on its exact host/service/block/instance identity. A zero count is valid but per-resident ratios are undefined.",
		observed:  fmt.Sprintf("newest_connect_processes=%d resident_count_available=%d resident_count_zero=%d resident_count_missing=%d resident_count_mixed=%d resident_count_invalid=%d generation_unselectable=%d", len(newest), available, zero, missing, mixed, invalid, unselectable),
		evidence:  "Only fixed fleet counts are retained; resident telemetry from another generation or service is never borrowed, and no device or customer labels are exposed.",
		context:   "RSS, heap, and goroutines per resident are descriptive total-process ratios, not proof of a per-resident allocation owner or leak. Resource findings still include every draining generation; only this capability check selects the newest generation.",
		action:    "Restore the identity-free resident gauge and coherent process relabeling, resolve duplicate or ambiguous generation samples, then compare matched traffic and population controls before attributing high total process cost. Do not change limits or redeploy solely because this diagnostic denominator is unavailable.",
		verify:    "Two consecutive scrapes provide one valid resident count for every unambiguously newest Connect identity; independently verify raw runtime, host saturation, service traffic, and rollout convergence.",
		playbook:  "SIGNALS.md §8.15",
	}
}
