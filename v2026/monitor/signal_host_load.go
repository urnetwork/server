package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	hostLoadFreshness                = 90 * time.Second
	hostLoadCPUExecutionRatio        = 0.90
	hostLoadNormalizedRunnableRatio  = 1.25
	hostLoadIOWaitRatio              = 0.20
	hostLoadMemoryAvailableWarnRatio = 0.10
)

// Signal host-load implements SIGNALS.md §8.14. It joins fresh node-exporter
// load, CPU-mode, core-count, and memory gauges for every enabled inventory
// host. Missing telemetry remains an outage signal instead of silently
// removing an unreachable host from the denominator.
func NewHostLoadSignal() Signal {
	return &signalAdapter{
		number: "8.14", key: "host-load", name: "Host saturation and telemetry coverage",
		probe: hostLoadProbe{},
	}
}

type hostLoadProbe struct{}

func (hostLoadProbe) id() string             { return "host/runtime-saturation" }
func (hostLoadProbe) tier() string           { return tierPage }
func (hostLoadProbe) cadence() time.Duration { return time.Minute }

const (
	hostLoadMetricLoad uint8 = 1 << iota
	hostLoadMetricCores
	hostLoadMetricCPUExecution
	hostLoadMetricIOWait
	hostLoadMetricMemoryAvailable
	hostLoadMetricAll = hostLoadMetricLoad | hostLoadMetricCores |
		hostLoadMetricCPUExecution | hostLoadMetricIOWait | hostLoadMetricMemoryAvailable
)

type hostLoadMetrics struct {
	load1                float64
	cores                float64
	cpuExecution         float64
	ioWait               float64
	memoryAvailableRatio float64
	mask                 uint8
}

func hostLoadQuery(environment string) string {
	env := strconv.Quote(environment)
	freshness := int64(hostLoadFreshness / time.Second)
	freshInstant := func(metric string) string {
		selector := fmt.Sprintf(`%s{env=%s,job="node"}`, metric, env)
		return fmt.Sprintf(`(%s and (timestamp(%s) >= time() - %d))`, selector, selector, freshness)
	}
	freshRate := func(mode string) string {
		selector := fmt.Sprintf(`node_cpu_seconds_total{env=%s,job="node",mode=%s}`, env, strconv.Quote(mode))
		return fmt.Sprintf(`(rate(%s[5m]) and (timestamp(%s) >= time() - %d))`, selector, selector, freshness)
	}
	allCPUSelector := fmt.Sprintf(`node_cpu_seconds_total{env=%s,job="node"}`, env)
	freshCPU := fmt.Sprintf(
		`(%s and (timestamp(%s) >= time() - %d))`,
		allCPUSelector,
		allCPUSelector,
		freshness,
	)
	return strings.Join([]string{
		fmt.Sprintf(`label_replace(%s,"monitor_metric","load1","job",".*")`, freshInstant("node_load1")),
		fmt.Sprintf(`label_replace(count without(cpu) (count without(mode) (%s)),"monitor_metric","cores","job",".*")`, freshCPU),
		fmt.Sprintf(`label_replace(clamp_min(1 - avg without(cpu,mode) (%s) - avg without(cpu,mode) (%s),0),"monitor_metric","cpu_execution","job",".*")`, freshRate("idle"), freshRate("iowait")),
		fmt.Sprintf(`label_replace(avg without(cpu) (%s),"monitor_metric","io_wait","job",".*")`, freshRate("iowait")),
		fmt.Sprintf(`label_replace(%s / %s,"monitor_metric","memory_available_ratio","job",".*")`, freshInstant("node_memory_MemAvailable_bytes"), freshInstant("node_memory_MemTotal_bytes")),
	}, " or ")
}

func (hostLoadProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("host load: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(hostLoadQuery(env.cfg.env))
	out, gateway, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("host load: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("host load: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"host load: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	expected := make(map[string]bool, len(env.cfg.hosts))
	for _, configured := range env.cfg.hosts {
		expected[configured.name] = true
	}
	now := env.now().UTC()
	byHost := map[string]*hostLoadMetrics{}
	for _, series := range response.Data.Result {
		host := series.Metric["host"]
		if !expected[host] {
			continue
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("host load: parse fixed metric sample: %w", err)
		}
		age := now.Sub(observedAt)
		if age > hostLoadFreshness || age < -30*time.Second {
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("host load: fixed metric value is invalid")
		}
		metrics := byHost[host]
		if metrics == nil {
			metrics = &hostLoadMetrics{}
			byHost[host] = metrics
		}
		switch series.Metric["monitor_metric"] {
		case "load1":
			metrics.load1 = value
			metrics.mask |= hostLoadMetricLoad
		case "cores":
			metrics.cores = value
			metrics.mask |= hostLoadMetricCores
		case "cpu_execution":
			if value > 1.01 {
				return nil, fmt.Errorf("host load: CPU execution ratio is invalid")
			}
			metrics.cpuExecution = value
			metrics.mask |= hostLoadMetricCPUExecution
		case "io_wait":
			if value > 1.01 {
				return nil, fmt.Errorf("host load: I/O wait ratio is invalid")
			}
			metrics.ioWait = value
			metrics.mask |= hostLoadMetricIOWait
		case "memory_available_ratio":
			if value > 1.01 {
				return nil, fmt.Errorf("host load: memory available ratio is invalid")
			}
			metrics.memoryAvailableRatio = value
			metrics.mask |= hostLoadMetricMemoryAvailable
		}
	}

	findings := make([]finding, 0, len(expected)*4)
	for _, configured := range env.cfg.hosts {
		target := configured.name
		metrics := byHost[target]
		if metrics == nil || metrics.mask != hostLoadMetricAll || metrics.cores < 1 {
			available := uint8(0)
			if metrics != nil {
				available = metrics.mask
			}
			findings = append(findings, finding{
				probeId: "host/runtime-saturation", tier: tierWarn,
				class: "host-metrics-missing", target: target, sustain: 2, pageSustain: 3,
				symptom:   "An enabled inventory host has no complete fresh node-runtime observation.",
				mechanism: "A stopped node exporter, broken telemetry path, or unreachable host removed required load/capacity data. Missing series are unknown host state, not a healthy zero.",
				baseline:  "Every enabled inventory host exports fresh load1, CPU count, five-minute busy and I/O-wait ratios, and available-memory ratio at the one-minute cadence.",
				observed:  fmt.Sprintf("available_metric_mask=%02x required_metric_mask=%02x freshness_limit_s=%d", available, hostLoadMetricAll, int(hostLoadFreshness/time.Second)),
				evidence:  "The bounded Mimir response was joined only to enabled inventory names; raw labels from unknown series were discarded. Query gateway=" + gateway.name + ".",
				action:    "Check the target's direct SSH reachability, node exporter, fluent-bit/remote-write path, and current service health. Treat a simultaneously unreachable host as the incident; do not suppress it or infer green from siblings.",
				verify:    "Two consecutive runs contain the complete fresh metric family for this exact enabled host.",
				playbook:  "SIGNALS.md §8.14",
			})
			continue
		}
		findings = append(findings, healthyFinding("host/runtime-saturation", tierWarn, "host-metrics-missing", target))

		normalizedLoad := metrics.load1 / metrics.cores
		observed := fmt.Sprintf(
			"load1=%.2f logical_cpu_count=%.0f normalized_load1=%.3f cpu_execution_ratio_5m=%.4f io_wait_ratio_5m=%.4f memory_available_ratio=%.4f",
			metrics.load1,
			metrics.cores,
			normalizedLoad,
			metrics.cpuExecution,
			metrics.ioWait,
			metrics.memoryAvailableRatio,
		)
		if metrics.cpuExecution >= hostLoadCPUExecutionRatio && normalizedLoad >= hostLoadNormalizedRunnableRatio {
			findings = append(findings, finding{
				probeId: "host/runtime-saturation", tier: tierPage,
				class: "host-cpu-saturation", target: target, sustain: 2,
				symptom:   "The host has sustained CPU saturation with runnable work queued beyond its logical CPU capacity.",
				mechanism: "Runnable work exceeds the host's execution capacity while CPU idle time is exhausted. Service callbacks, allocation/GC churn, deployment overlap, or hostile traffic can make an otherwise reachable host stop serving work promptly.",
				baseline:  fmt.Sprintf("cpu_execution_ratio_5m < %.2f or normalized_load1 < %.2f", hostLoadCPUExecutionRatio, hostLoadNormalizedRunnableRatio),
				observed:  observed,
				evidence:  "Fresh node-exporter samples were evaluated through the loopback Mimir gateway " + gateway.name + "; no process command line or unbounded label was retained.",
				context:   "This is a capacity symptom, not its owner. Correlate §8.15 before changing limits. A legitimate sustained workload may require more hardware; a runaway process or overlapping retired generation requires a software or operational correction.",
				action:    "Identify the per-service and per-process CPU, allocation, goroutine, and RSS owners. Preserve bounded evidence. If an old draining generation is proven stuck and its replacement is ready, retire only that generation serially and verify service health; do not reboot the host to erase the cause.",
				verify:    "Two consecutive runs show CPU execution or normalized load below the alert band with public and internal service health intact.",
				playbook:  "SIGNALS.md §8.14",
			})
		} else {
			findings = append(findings, healthyFinding("host/runtime-saturation", tierPage, "host-cpu-saturation", target))
		}

		if metrics.ioWait >= hostLoadIOWaitRatio && normalizedLoad >= hostLoadNormalizedRunnableRatio {
			findings = append(findings, finding{
				probeId: "host/runtime-saturation", tier: tierPage,
				class: "host-io-saturation", target: target, sustain: 2,
				symptom:   "The host load is saturated by tasks waiting on I/O.",
				mechanism: "A storage, filesystem, network-block-device, or writeback stall is keeping tasks runnable or blocked without useful CPU progress.",
				baseline:  fmt.Sprintf("io_wait_ratio_5m < %.2f or normalized_load1 < %.2f", hostLoadIOWaitRatio, hostLoadNormalizedRunnableRatio),
				observed:  observed,
				evidence:  "Fresh aggregate CPU-mode and load gauges were used; this probe does not infer a device from the aggregate alone.",
				action:    "Inspect bounded per-device latency, queue depth, filesystem errors, and the owning service before mutation. Isolate a failed removable device when proven; do not restart unrelated services or call CPU load an I/O fault.",
				verify:    "Two consecutive runs show I/O wait or normalized load below the alert band and the implicated device path completes reads and writes normally.",
				playbook:  "SIGNALS.md §8.14",
			})
		} else {
			findings = append(findings, healthyFinding("host/runtime-saturation", tierPage, "host-io-saturation", target))
		}

		if metrics.memoryAvailableRatio <= hostLoadMemoryAvailableWarnRatio {
			findings = append(findings, finding{
				probeId: "host/runtime-saturation", tier: tierWarn,
				class: "host-memory-pressure", target: target, sustain: 2, pageSustain: 5,
				symptom:   "The host has little memory available for new workload or kernel buffers.",
				mechanism: "Anonymous allocations, retained service state, unreclaimable kernel memory, or an undersized host can force reclaim and eventually OOM-kill a service.",
				baseline:  fmt.Sprintf("memory_available_ratio > %.2f", hostLoadMemoryAvailableWarnRatio),
				observed:  observed,
				evidence:  "MemAvailable was divided by MemTotal from the same node-exporter target; filesystem cache was not misclassified as unavailable memory.",
				context:   "If per-client memory is already within its software ceiling and legitimate active-client demand reaches host capacity, recovery requires traffic reduction or additional hardware. Software optimization remains required when retained state exceeds the designed ceiling.",
				action:    "Correlate §8.15 and service-specific memory probes, then distinguish active-client capacity from leaked or retired state. Do not add swap or reboot merely to hide an unbounded owner.",
				verify:    "Two consecutive runs restore more than the minimum available-memory ratio without OOM or service-health regressions.",
				playbook:  "SIGNALS.md §8.14",
			})
		} else {
			findings = append(findings, healthyFinding("host/runtime-saturation", tierWarn, "host-memory-pressure", target))
		}
	}
	return findings, nil
}
