package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
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
// A score-correlated outlier is enriched only when its exact runtime's complete
// fixed phase family is source-fresh, independently of the global alias marker.
// Phase occupancy does not attribute process allocation to a task.
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
	samples   workerMetricSamples
}

// Own cancellation even when an alternate source ignores its context.
func (workerChurnProbe) check(ctx context.Context, env *probeEnv) (result []finding, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer func() {
		if contextErr := ctx.Err(); contextErr != nil {
			result = nil
			if resultErr == nil || resultErr == contextErr {
				resultErr = contextErr
			} else {
				resultErr = &signalContextError{contextErr: contextErr, sourceErr: resultErr}
			}
		}
	}()
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("worker churn: no services host in inventory for the loopback Mimir query")
	}

	query := workerRatesQuery(env.cfg.env, "1m", false)
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(query)
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if ctx.Err() != nil {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("worker churn: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("worker churn: invalid Mimir response")
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("worker churn: Mimir response was not a successful vector")
	}

	now := env.now().UTC()
	if ctx.Err() != nil {
		return nil, nil
	}
	workers := map[string]*workerChurnMetrics{}
	bounded := len(response.Warnings) == 0
	for _, series := range response.Data.Result {
		kind := series.Metric["monitor_rate"]
		if kind != "cpu" && kind != "alloc" && kind != "cpu_source_time" && kind != "alloc_source_time" {
			// Returned evidence outside the issued family schema cannot
			// disappear into an apparently complete quiet fleet.
			bounded = false
			continue
		}
		if !workerMetricIdentity(series.Metric, env.cfg.env) {
			bounded = false
			continue
		}
		host, block, instance := series.Metric["host"], series.Metric["block"], series.Metric["instance"]
		key := workerScorePhaseKey(host, block, instance)
		worker := workers[key]
		if worker == nil {
			worker = &workerChurnMetrics{host: host, block: block, instance: instance, samples: workerMetricSamples{}}
			workers[key] = worker
		}
		worker.samples.observe(kind, series.Value, now)
	}

	cpuRates := make([]float64, 0, len(workers))
	allocRates := make([]float64, 0, len(workers))
	incomplete := 0
	for _, worker := range workers {
		if value, valid := worker.samples.qualified("cpu"); valid {
			worker.cpuRate, worker.mask = value, worker.mask|workerRateCPU
			cpuRates = append(cpuRates, value)
		}
		if value, valid := worker.samples.qualified("alloc"); valid {
			worker.allocRate, worker.mask = value, worker.mask|workerRateAlloc
			allocRates = append(allocRates, value)
		}
		if worker.mask != workerRateCPU|workerRateAlloc {
			incomplete++
		}
	}
	cpuMedian := workerMedianBounds(cpuRates, len(workers), bounded)
	allocMedian := workerMedianBounds(allocRates, len(workers), bounded)
	indeterminate := 0

	findings := []finding{}
	activeLog := ""
	activeLogSource := ""
	var activeLogErr error
	activeLogLoaded := false
	activeLogObservedAt := now
	aliasesReady := false
	var aliasesReadyErr error
	aliasesReadyLoaded := false
	phaseObservations := map[string]workerScorePhaseObservation{}
	var phaseObservationsErr error
	phaseObservationsLoaded := false
	for _, worker := range workers {
		if worker.mask != workerRateCPU|workerRateAlloc {
			continue
		}
		target := worker.host + "/" + worker.block
		// A below-absolute guard is independently healthy. Relative guards
		// use median bounds; omitted siblings must not lower the baseline.
		healthy := worker.cpuRate < workerChurnCPUCores || worker.allocRate < workerChurnAllocBytesRate ||
			worker.cpuRate/workerChurnFleetRatio < cpuMedian.lower ||
			worker.allocRate/workerChurnFleetRatio < allocMedian.lower
		if healthy {
			observed := healthyFinding("runtime/worker-cpu-allocation-churn", tierWarn, "worker-cpu-allocation-churn", target)
			observed.frame = worker.instance
			findings = append(findings, observed)
			continue
		}
		if worker.cpuRate/workerChurnFleetRatio < cpuMedian.upper ||
			worker.allocRate/workerChurnFleetRatio < allocMedian.upper {
			indeterminate++
			continue
		}

		if !activeLogLoaded {
			activeLog, activeLogSource, activeLogErr = readTaskLifecycleLog(ctx, env, "eval", 2*time.Minute, 5000)
			if ctx.Err() != nil {
				return nil, activeLogErr
			}
			activeLogLoaded = true
			// The gateway timeout and journal fallback can outlive the metric
			// query by more than the parser's future-skew allowance. Refresh the
			// comparison clock after collection so fresh fallback lines remain
			// attributable to this exact executor.
			activeLogObservedAt = env.now().UTC()
			if ctx.Err() != nil {
				return nil, activeLogErr
			}
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

		observed := fmt.Sprintf(
			"cpu_cores_1m=%.3f fleet_samples=%d %s cpu_ratio_1m=%s alloc_bytes_per_s_1m=%.0f alloc_mib_per_s_1m=%.2f %s alloc_ratio_1m=%s block=%s instance=%s metrics_gateway=%s",
			worker.cpuRate, len(workers),
			cpuMedian.summary("fleet_median_cpu_cores_1m", "%.3f"), workerMetricRatio(worker.cpuRate, cpuMedian.upper),
			worker.allocRate, worker.allocRate/float64(1024*1024),
			allocMedian.summary("fleet_median_alloc_bytes_per_s_1m", "%.0f"), workerMetricRatio(worker.allocRate, allocMedian.upper),
			worker.block, worker.instance, metricHost.name,
		)
		if !cpuMedian.complete || !allocMedian.complete {
			observed += " fleet_baseline=bounded_partial ratios=lower_bounds"
		}

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
		aliasEvidence := ""
		if scoreActive {
			observed += " score_alias_marker_scope=global score_exporter_capability=unverified"
			action = "First verify the exact outlier Taskworker artifact and score-exporter capability; the global alias marker cannot attest this process or its current code path. If the artifact is caller-oriented, deploy the target-oriented UpdateClientScores fanout and alias-aware cache. If target-oriented capability is independently proven, retain the process through terminal and collection boundaries and profile recurring source-load, target-map, gob-encode, or cache-write work before changing it. Preserve bounded streaming; do not restart or raise the CPU limit to erase the evidence."
			verify = "The exact outlier's exporter capability is independently established, consecutive score runs complete inside their normal band, and the executor returns below the CPU/allocation guards for two consecutive probes after terminal/collection boundaries without selection loss or Redis capacity pressure. A global marker alone does not close this process-local finding."
			if !aliasesReadyLoaded {
				if ctx.Err() != nil {
					return nil, aliasesReadyErr
				}
				aliasesReadyLoaded = true
				redisHost := env.cfg.redisEntryHost()
				if redisHost == nil {
					aliasesReadyErr = fmt.Errorf("no redis-cluster host in inventory")
				} else {
					aliasesReady, aliasesReadyErr = redisScoreAliasesReady(ctx, env, redisHost)
					if ctx.Err() != nil {
						return nil, aliasesReadyErr
					}
				}
			}
			switch {
			case aliasesReadyErr != nil:
				observed += " score_alias_schema_ready=unknown"
				mechanism = "UpdateClientScores has a fresh heartbeat on the outlier's host/block, but the global alias-schema marker lookup failed. Score export is a correlated allocation candidate, not established heap ownership. Neither marker state nor exact exporter capability is established."
				aliasEvidence = " The global score-alias compatibility marker could not be read."
			case aliasesReady:
				observed += " score_alias_schema_ready=true"
				mechanism = "UpdateClientScores has a fresh heartbeat on the outlier's host/block, and the global alias-schema marker shows that some writer previously completed a compatibility pass. That persistent shared marker is not provenance for this exact worker artifact or proof that its current export uses target-oriented fanout. Score export is a correlated allocation candidate, not established ownership of the process allocation."
			default:
				observed += " score_alias_schema_ready=false"
				mechanism = "UpdateClientScores has a fresh heartbeat on the outlier's host/block, and the global alias-schema marker is absent. This may be a pending or incomplete compatibility pass, legacy writers, or missing cache state; it does not identify this worker's artifact. Score export is a correlated allocation candidate, not established ownership of the process allocation."
			}
			if !phaseObservationsLoaded {
				if ctx.Err() != nil {
					return nil, phaseObservationsErr
				}
				phaseObservationsLoaded = true
				phaseObservations, phaseObservationsErr = loadWorkerScorePhaseObservations(ctx, env, metricHosts, metricHost)
				if ctx.Err() != nil {
					return nil, phaseObservationsErr
				}
			}
			phaseObservation, phaseReady := phaseObservations[workerScorePhaseKey(worker.host, worker.block, worker.instance)]
			if phaseObservationsErr != nil || !phaseReady {
				observed += " score_phase_observability=unavailable"
				mechanism += " The complete source-fresh phase set is unavailable for this exact runtime; the profiling boundary is explicitly unobservable, not healthy. The process-rate finding remains valid."
				aliasEvidence += " Phase lookup failed or returned incomplete, stale, or different-generation data; no partial set or other worker was substituted."
			} else {
				observed += " " + phaseObservation.summary()
				mechanism += " Fixed phase telemetry is complete for this exact runtime: " + phaseObservation.discriminator() + ". Phase occupancy and deterministic work bytes select a profiling boundary, not heap ownership; completed-span seconds are not CPU time and work bytes are not process heap allocations."
				aliasEvidence += " Each phase family was filtered by its underlying source timestamp before the exact host/block/instance join; the instant-query evaluation timestamp alone is not freshness evidence."
			}
			if closeActive {
				mechanism += " CloseExpiredContracts is active on the same host/block, so this process-local saturation can delay its Go work between otherwise short PostgreSQL statements; close-duration and open-contract age buckets remain the authoritative impact measures."
				verify += " The co-resident close checkpoint also returns below 120 seconds and its older-than-five/30-minute contract buckets fall on consecutive samples."
			}
		}

		evidence := "One-minute rates come from the taskworker process metrics pushed to Mimir, grouped by exact host/block/runtime instance and compared with the returned fleet median. Missing scalar values retain their runtime and can establish skew only against the greatest possible median; partial ratios are lower bounds. Each rate has its own fresh underlying counter timestamp. Two consecutive probe failures provide a two-minute sustain guard. Recent eval-active heartbeats are joined on host/block."
		if activeLogErr != nil {
			evidence += fmt.Sprintf(" The task-lifecycle lookup was degraded (error_class=%s).", classifyObservationError(activeLogErr))
		} else if activeSummary == "" {
			evidence += " No fresh active-task heartbeat was available for this executor, so the rate finding remains valid without task attribution."
		}
		evidence += aliasEvidence

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
	if len(workers) == 0 || incomplete > 0 || indeterminate > 0 || !bounded {
		findings = append(findings, workerMetricCoverage("taskworker-fleet/churn-metrics", len(workers), incomplete, indeterminate, bounded))
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].target != findings[j].target {
			return findings[i].target < findings[j].target
		}
		return findings[i].frame < findings[j].frame
	})
	return findings, nil
}
