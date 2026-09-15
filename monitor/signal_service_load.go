package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	serviceLoadFreshness          = 90 * time.Second
	serviceLoadRSSFloorBytes      = float64(16 << 30)
	serviceLoadRSSHardBytes       = float64(64 << 30)
	serviceLoadGoroutineFloor     = float64(250_000)
	serviceLoadGoroutineHard      = float64(500_000)
	serviceLoadAllocationFloor    = float64(256 << 20)
	serviceLoadAllocationHard     = float64(1 << 30)
	serviceLoadMinimumCPUCoreRate = 1.0
	serviceLoadMaximumCPUCoreRate = 4.0
	serviceLoadHostCPUFraction    = 0.0625
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
	serviceLoadMetricRawAll = serviceLoadMetricRSS | serviceLoadMetricHeap |
		serviceLoadMetricObjects | serviceLoadMetricGoroutines | serviceLoadMetricStart
)

type serviceLoadMetrics struct {
	host       string
	service    string
	block      string
	instance   string
	rss        float64
	heap       float64
	objects    float64
	goroutines float64
	start      float64
	cpu        float64
	allocation float64
	gc         float64
	mask       uint16
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
		"curl -fsS --max-time 15 '"+queryURL+"'",
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
	hostCores := map[string]float64{}
	invalidSeries := 0
	for _, series := range response.Data.Result {
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("service load: parse fixed metric sample: %w", err)
		}
		age := now.Sub(observedAt)
		if age > serviceLoadFreshness || age < -30*time.Second {
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("service load: fixed metric value is invalid")
		}
		metric := series.Metric["monitor_metric"]
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
		}
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
			),
			evidence: "Fresh runtime gauges and five-minute counter rates were joined on the exact host/service/block/instance identity through gateway " + gateway.name + ". Draining generations remain visible by design.",
			context:  "This is affirmative process ownership, not proof of the allocation call site. If bounded per-client state is legitimately at its designed ceiling, operations must reduce traffic or add capable hardware; unbounded goroutine/queue fan-out or retained retired state requires a software fix.",
			action:   "Inspect the service-specific resident/connection/queue/drain metrics and the source defaults that scale per client. Preserve the process generation and bounded profile evidence. Hard-terminate only a proven-stuck old generation after its replacement is ready, one block at a time; do not reboot the host or kill the current owner blindly.",
			verify:   "Two consecutive runs place every process below the runtime band and §8.14 returns healthy while service traffic and rollout convergence remain healthy.",
			playbook: "SIGNALS.md §8.15",
		})
	}
	return findings, nil
}
