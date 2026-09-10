package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	workerChurnCPUCores       = 3.8
	workerChurnAllocBytesRate = float64(256 << 20)
	workerChurnFleetRatio     = 8.0
	workerChurnFreshness      = 90 * time.Second
)

// Signal worker-churn implements SIGNALS.md §2.12a. It detects a taskworker
// that is simultaneously CPU-saturated and allocating at an exceptional rate,
// even when its bounded live heap no longer trips the §2.12 memory-skew guard.
func NewWorkerChurnSignal() Signal {
	return &signalAdapter{
		number: "2.12a", key: "worker-churn", name: "Taskworker CPU/allocation churn",
		probe: workerChurnProbe{},
	}
}

type workerChurnProbe struct{}

func (workerChurnProbe) id() string             { return "runtime/worker-cpu-allocation-churn" }
func (workerChurnProbe) tier() string           { return tierWarn }
func (workerChurnProbe) cadence() time.Duration { return time.Minute }

type workerChurnMetrics struct {
	host      string
	block     string
	instance  string
	cpuRate   float64
	allocRate float64
	mask      uint8
}

func (workerChurnProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("worker churn: no services host in inventory for the loopback Mimir query")
	}

	query := strings.Join([]string{
		fmt.Sprintf(`label_replace(rate(process_cpu_seconds_total{env=%s,job="taskworker"}[1m]),"monitor_rate","cpu","job",".*")`, strconv.Quote(env.cfg.env)),
		fmt.Sprintf(`label_replace(rate(go_memstats_alloc_bytes_total{env=%s,job="taskworker"}[1m]),"monitor_rate","alloc","job",".*")`, strconv.Quote(env.cfg.env)),
	}, " or ")
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(query)
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("worker churn: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("worker churn: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"worker churn: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	now := env.now().UTC()
	workers := map[string]*workerChurnMetrics{}
	for _, series := range response.Data.Result {
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("worker churn: parse %s rate: %w", series.Metric["monitor_rate"], err)
		}
		age := now.Sub(observedAt)
		if age > workerChurnFreshness || age < -30*time.Second {
			continue
		}
		host := series.Metric["host"]
		if host == "" {
			continue
		}
		block := series.Metric["block"]
		instance := series.Metric["instance"]
		key := host + "\x00" + block + "\x00" + instance
		worker := workers[key]
		if worker == nil {
			worker = &workerChurnMetrics{host: host, block: block, instance: instance}
			workers[key] = worker
		}
		switch series.Metric["monitor_rate"] {
		case "cpu":
			worker.cpuRate = value
			worker.mask |= workerRateCPU
		case "alloc":
			worker.allocRate = value
			worker.mask |= workerRateAlloc
		}
	}

	pairedWorkers := make([]*workerChurnMetrics, 0, len(workers))
	cpuRates := make([]float64, 0, len(workers))
	allocRates := make([]float64, 0, len(workers))
	for _, worker := range workers {
		if worker.mask&(workerRateCPU|workerRateAlloc) != workerRateCPU|workerRateAlloc {
			continue
		}
		pairedWorkers = append(pairedWorkers, worker)
		cpuRates = append(cpuRates, worker.cpuRate)
		allocRates = append(allocRates, worker.allocRate)
	}
	if len(pairedWorkers) == 0 {
		return nil, fmt.Errorf("worker churn: Mimir returned no fresh paired taskworker CPU/allocation rates")
	}
	cpuMedian := medianFloat64(cpuRates)
	allocMedian := medianFloat64(allocRates)

	findings := []finding{}
	activeLog := ""
	activeLogSource := ""
	var activeLogErr error
	activeLogLoaded := false
	activeLogObservedAt := now
	for _, worker := range pairedWorkers {
		cpuRatio := safeRatio(worker.cpuRate, cpuMedian)
		allocRatio := safeRatio(worker.allocRate, allocMedian)
		if worker.cpuRate < workerChurnCPUCores ||
			worker.allocRate < workerChurnAllocBytesRate ||
			cpuRatio < workerChurnFleetRatio ||
			allocRatio < workerChurnFleetRatio {
			continue
		}

		if !activeLogLoaded {
			activeLog, activeLogSource, activeLogErr = readTaskLifecycleLog(ctx, env, "eval", 2*time.Minute, 5000)
			activeLogLoaded = true
			// The gateway timeout and journal fallback can outlive the metric
			// query by more than the parser's future-skew allowance. Refresh the
			// comparison clock after collection so fresh fallback lines remain
			// attributable to this exact executor.
			activeLogObservedAt = env.now().UTC()
		}
		activeTasks := parseExecutorActiveTasks(activeLog, worker.host, worker.block, activeLogObservedAt)
		activeSummary := formatExecutorActiveTasks(activeTasks, 8)
		scoreActive := false
		closeActive := false
		for _, task := range activeTasks {
			switch task.name {
			case "UpdateClientScores":
				scoreActive = true
			case "CloseExpiredContracts":
				closeActive = true
			}
		}

		target := worker.host
		if worker.block != "" {
			target += "/" + worker.block
		}
		observed := fmt.Sprintf(
			"cpu_cores_1m=%.3f fleet_samples=%d fleet_median_cpu_cores_1m=%.3f cpu_ratio_1m=%.1f alloc_bytes_per_s_1m=%.0f alloc_mib_per_s_1m=%.2f fleet_median_alloc_bytes_per_s_1m=%.0f alloc_ratio_1m=%.1f block=%s instance=%s metrics_gateway=%s",
			worker.cpuRate,
			len(pairedWorkers),
			cpuMedian,
			cpuRatio,
			worker.allocRate,
			worker.allocRate/float64(1<<20),
			allocMedian,
			allocRatio,
			worker.block,
			worker.instance,
			metricHost.name,
		)
		if activeSummary != "" {
			observed += fmt.Sprintf(
				" active_task_count=%d active_tasks=%s active_log_source=%s",
				len(activeTasks),
				activeSummary,
				activeLogSource,
			)
		}

		mechanism := "The conjunction of near-quota CPU, high allocation throughput, and fleet-relative skew identifies process-local object churn rather than a large but quiescent heap. Co-resident task heartbeats narrow the candidate work, but do not by themselves attribute every allocation to one task."
		action := "Profile or inspect the active task families on this exact executor and remove repeated encoding, copying, or unbounded materialization. Keep existing bounded writers and deadlines; do not normalize the signal by raising the CPU limit or restarting the worker merely to clear evidence."
		verify := "For two consecutive one-minute probes, either CPU falls below 3.8 cores, allocation falls below 256MiB/s, or both rates return within 8x of the fleet median; implicated tasks also complete inside their historical duration band."
		if scoreActive {
			mechanism = "UpdateClientScores is active on the exact hot executor. A target's exported score payload is caller-invariant unless that caller blocks a network present in the target; encoding the unchanged target separately for every caller multiplies gob work by the caller-location count and produces the observed CPU/allocation churn even when streaming keeps live heap bounded."
			action = "Deploy the target-oriented UpdateClientScores fanout and alias-aware cache: encode one zero-caller baseline per target, write one-byte aliases for unchanged callers, and independently encode full overrides only for callers whose blocked networks actually remove a provider. Retain the bounded streaming batches and rolling legacy-reader pass; do not raise the cgroup limit or restart to erase the evidence."
			verify = "A post-deploy UpdateClientScores run completes inside its historical band while its exact executor remains below the CPU/allocation guards for two consecutive probes; aliases preserve unfiltered selections, excluded callers still use overrides, and the score-byte signal drains after the legacy TTL."
			if closeActive {
				mechanism += " CloseExpiredContracts is active on the same host/block, so this process-local saturation can delay its Go work between otherwise short PostgreSQL statements; close-duration and open-contract age buckets remain the authoritative impact measures."
				verify += " The co-resident close checkpoint also returns below 120 seconds and its older-than-five/30-minute contract buckets fall on consecutive samples."
			}
		}

		evidence := "One-minute rates come from the taskworker process metrics pushed to Mimir, grouped by exact host/block/runtime instance and compared with the fresh paired fleet median. Two consecutive probe failures provide a two-minute sustain guard. Recent eval-active heartbeats are joined on host/block."
		if activeLogErr != nil {
			evidence += " The task-lifecycle lookup was degraded: " + activeLogErr.Error()
		} else if activeSummary == "" {
			evidence += " No fresh active-task heartbeat was available for this executor, so the rate finding remains valid without task attribution."
		}

		findingContext := "CPU saturation alone can be useful work, and allocation alone can be a short burst. Requiring both absolute guards and both fleet-skew guards avoids treating an ordinary busy worker as pathological churn; this signal complements the live-heap guard in §2.12."
		if closeActive {
			findingContext += " Exact co-residency proves a shared process budget, not that every slow close has this cause; retain database wait/vacuum and adjacent fast-executor controls from §2.6a."
		}

		findings = append(findings, finding{
			probeId: "runtime/worker-cpu-allocation-churn", tier: tierWarn,
			class: "worker-cpu-allocation-churn", target: target, frame: worker.instance, sustain: 2,
			symptom: fmt.Sprintf(
				"taskworker %s consumes %.3f CPU cores while allocating %.2fMiB/s",
				target,
				worker.cpuRate,
				worker.allocRate/float64(1<<20),
			),
			mechanism: mechanism,
			baseline: fmt.Sprintf(
				"For two consecutive one-minute samples, taskworkers remain below %.1f CPU cores or %.0fMiB/s allocation, or within %.0fx of both fresh fleet medians.",
				workerChurnCPUCores,
				workerChurnAllocBytesRate/float64(1<<20),
				workerChurnFleetRatio,
			),
			observed: observed,
			evidence: evidence,
			context:  findingContext,
			action:   action,
			verify:   verify,
			playbook: "SIGNALS.md 2.12a",
		})
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("runtime/worker-cpu-allocation-churn", tierWarn, "worker-cpu-allocation-churn", "taskworker-fleet")}, nil
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].target < findings[j].target })
	return findings, nil
}
