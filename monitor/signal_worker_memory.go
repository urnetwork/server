package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	workerMemoryAbsoluteBytes       = float64(8 << 30)
	workerMemorySparseAbsoluteBytes = float64(16 << 30)
	workerMemorySkewRatio           = 4.0
	workerMemoryMetricFreshness     = 90 * time.Second
)

// Signal worker-memory implements SIGNALS.md §2.12. It compares fresh Go
// runtime metrics across taskworker processes so a single allocation-heavy
// executor cannot hide behind ample host memory or a healthy fleet median.
func NewWorkerMemorySignal() Signal {
	return &signalAdapter{
		number: "2.12", key: "worker-memory", name: "Taskworker allocated-heap skew",
		probe: workerMemoryProbe{},
	}
}

type workerMemoryProbe struct{}

func (workerMemoryProbe) id() string             { return "runtime/worker-memory-skew" }
func (workerMemoryProbe) tier() string           { return tierWarn }
func (workerMemoryProbe) cadence() time.Duration { return time.Minute }

type mimirInstantResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type workerRuntimeMetrics struct {
	host        string
	block       string
	instance    string
	heap        float64
	objects     float64
	rss         float64
	allocTotal  float64
	gcCycles    float64
	startTime   float64
	cpuRate     float64
	allocRate   float64
	gcRate      float64
	gcPauseRate float64
	rateMask    uint8
}

const (
	workerRateCPU uint8 = 1 << iota
	workerRateAlloc
	workerRateGC
	workerRateGCPause
	workerRateAll = workerRateCPU | workerRateAlloc | workerRateGC | workerRateGCPause
)

func (workerMemoryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("worker memory: no services host in inventory for the loopback Mimir query")
	}

	metricNames := strings.Join([]string{
		"go_memstats_heap_alloc_bytes",
		"go_memstats_heap_objects",
		"go_memstats_alloc_bytes_total",
		"go_gc_duration_seconds_count",
		"process_resident_memory_bytes",
		"process_start_time_seconds",
	}, "|")
	query := fmt.Sprintf(
		`{__name__=~%s,env=%s,job="taskworker"}`,
		strconv.Quote(metricNames),
		strconv.Quote(env.cfg.env),
	)
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(query)
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("worker memory: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("worker memory: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("worker memory: Mimir status=%q result_type=%q error=%q", response.Status, response.Data.ResultType, response.Error)
	}

	now := env.now().UTC()
	workers := map[string]*workerRuntimeMetrics{}
	for _, series := range response.Data.Result {
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("worker memory: parse %s sample: %w", series.Metric["__name__"], err)
		}
		age := now.Sub(observedAt)
		if age > workerMemoryMetricFreshness || age < -30*time.Second {
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
			worker = &workerRuntimeMetrics{host: host, block: block, instance: instance}
			workers[key] = worker
		}
		switch series.Metric["__name__"] {
		case "go_memstats_heap_alloc_bytes":
			worker.heap = value
		case "go_memstats_heap_objects":
			worker.objects = value
		case "go_memstats_alloc_bytes_total":
			worker.allocTotal = value
		case "go_gc_duration_seconds_count":
			worker.gcCycles = value
		case "process_resident_memory_bytes":
			worker.rss = value
		case "process_start_time_seconds":
			worker.startTime = value
		}
	}

	heaps := make([]float64, 0, len(workers))
	for _, worker := range workers {
		if worker.heap > 0 {
			heaps = append(heaps, worker.heap)
		}
	}
	if len(heaps) == 0 {
		return nil, fmt.Errorf("worker memory: Mimir returned no fresh taskworker heap samples")
	}
	median := medianFloat64(heaps)

	// Heap size alone cannot distinguish an actively allocating executor from
	// a completed task whose unreachable objects are waiting for the next GC.
	// Best-effort five-minute rates make that distinction without weakening the
	// base heap alert when an older Mimir front cannot evaluate the rate query.
	rateQuery := strings.Join([]string{
		fmt.Sprintf(`label_replace(rate(process_cpu_seconds_total{env=%s,job="taskworker"}[5m]),"monitor_rate","cpu","job",".*")`, strconv.Quote(env.cfg.env)),
		fmt.Sprintf(`label_replace(rate(go_memstats_alloc_bytes_total{env=%s,job="taskworker"}[5m]),"monitor_rate","alloc","job",".*")`, strconv.Quote(env.cfg.env)),
		fmt.Sprintf(`label_replace(rate(go_gc_duration_seconds_count{env=%s,job="taskworker"}[5m]),"monitor_rate","gc","job",".*")`, strconv.Quote(env.cfg.env)),
		fmt.Sprintf(`label_replace(rate(go_gc_duration_seconds_sum{env=%s,job="taskworker"}[5m]),"monitor_rate","gc_pause","job",".*")`, strconv.Quote(env.cfg.env)),
	}, " or ")
	rateURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(rateQuery)
	rateOut, _, rateErr := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		metricHost,
		"curl -fsS --max-time 15 '"+rateURL+"'",
	)
	if rateErr == nil {
		var rateResponse mimirInstantResponse
		if err := json.Unmarshal([]byte(rateOut), &rateResponse); err != nil {
			rateErr = fmt.Errorf("decode five-minute rate response: %w", err)
		} else if rateResponse.Status != "success" || rateResponse.Data.ResultType != "vector" {
			rateErr = fmt.Errorf("five-minute rate status=%q result_type=%q error=%q", rateResponse.Status, rateResponse.Data.ResultType, rateResponse.Error)
		} else {
			for _, series := range rateResponse.Data.Result {
				observedAt, value, err := mimirInstantValue(series.Value)
				if err != nil {
					rateErr = fmt.Errorf("parse %s five-minute rate: %w", series.Metric["monitor_rate"], err)
					break
				}
				age := now.Sub(observedAt)
				if age > workerMemoryMetricFreshness || age < -30*time.Second {
					continue
				}
				key := series.Metric["host"] + "\x00" + series.Metric["block"] + "\x00" + series.Metric["instance"]
				worker := workers[key]
				if worker == nil {
					continue
				}
				switch series.Metric["monitor_rate"] {
				case "cpu":
					worker.cpuRate = value
					worker.rateMask |= workerRateCPU
				case "alloc":
					worker.allocRate = value
					worker.rateMask |= workerRateAlloc
				case "gc":
					worker.gcRate = value
					worker.rateMask |= workerRateGC
				case "gc_pause":
					worker.gcPauseRate = value
					worker.rateMask |= workerRateGCPause
				}
			}
		}
	}
	cpuRates := make([]float64, 0, len(workers))
	allocRates := make([]float64, 0, len(workers))
	for _, worker := range workers {
		if worker.rateMask&workerRateCPU != 0 {
			cpuRates = append(cpuRates, worker.cpuRate)
		}
		if worker.rateMask&workerRateAlloc != 0 {
			allocRates = append(allocRates, worker.allocRate)
		}
	}
	cpuMedian := medianFloat64OrZero(cpuRates)
	allocMedian := medianFloat64OrZero(allocRates)

	findings := []finding{}
	activeLog := ""
	var activeLogErr error
	activeLogLoaded := false
	activeLogObservedAt := now
	for _, worker := range workers {
		if worker.heap <= 0 {
			continue
		}
		ratio := worker.heap / median
		outlier := worker.heap >= workerMemoryAbsoluteBytes && ratio >= workerMemorySkewRatio
		if len(heaps) < 3 {
			outlier = worker.heap >= workerMemorySparseAbsoluteBytes
		}
		if !outlier {
			continue
		}
		if !activeLogLoaded {
			activeLog, activeLogErr = env.runner.warpctl(
				ctx,
				"logs", env.cfg.env, "taskworker",
				"--since=2m", "--limit=5000", "--query=eval", "--utc",
			)
			activeLogLoaded = true
			// A degraded fleet gateway can spend tens of seconds before the
			// host-journal fallback returns. Compare those returned heartbeat
			// timestamps with the clock after collection, not the older metric
			// query clock, or genuinely fresh lines appear to be in the future.
			activeLogObservedAt = env.now().UTC()
		}

		target := worker.host
		if worker.block != "" {
			target += "/" + worker.block
		}
		processAge := int64(0)
		if worker.startTime > 0 {
			processAge = max(int64(0), now.Unix()-int64(worker.startTime))
		}
		activeTasks := parseExecutorActiveTasks(activeLog, worker.host, worker.block, activeLogObservedAt)
		observed := fmt.Sprintf(
			"heap_alloc_bytes=%.0f heap_gib=%.2f fleet_samples=%d fleet_median_bytes=%.0f fleet_median_gib=%.2f fleet_ratio=%.1f heap_objects=%.0f rss_bytes=%.0f alloc_total_bytes=%.0f gc_cycles=%.0f process_age_s=%d block=%s instance=%s",
			worker.heap, bytesToGiB(worker.heap), len(heaps), median, bytesToGiB(median), ratio,
			worker.objects, worker.rss, worker.allocTotal, worker.gcCycles, processAge, worker.block, worker.instance,
		)
		if rateErr == nil && worker.rateMask == workerRateAll {
			observed += fmt.Sprintf(
				" cpu_cores_5m=%.3f fleet_median_cpu_cores_5m=%.3f cpu_ratio_5m=%.1f alloc_bytes_per_s_5m=%.0f alloc_mib_per_s_5m=%.2f fleet_median_alloc_bytes_per_s_5m=%.0f alloc_ratio_5m=%.1f gc_cycles_per_s_5m=%.3f gc_pause_seconds_per_s_5m=%.6f",
				worker.cpuRate, cpuMedian, safeRatio(worker.cpuRate, cpuMedian),
				worker.allocRate, worker.allocRate/float64(1<<20), allocMedian, safeRatio(worker.allocRate, allocMedian),
				worker.gcRate, worker.gcPauseRate,
			)
		}
		if summary := formatExecutorActiveTasks(activeTasks, 8); summary != "" {
			observed += fmt.Sprintf(" active_task_count=%d active_tasks=%s", len(activeTasks), summary)
		}
		evidence := "The values come from the process's pushed Go runtime and process metrics in Mimir. Five-minute CPU/allocation/GC rates distinguish active allocation pressure from a completed task's heap awaiting collection. The active_tasks field joins the same host/block to authoritative taskworker eval-active heartbeats; compare those task families on other executors."
		if rateErr != nil {
			evidence += " The best-effort five-minute rate lookup failed: " + rateErr.Error()
		} else if worker.rateMask != workerRateAll {
			evidence += " Mimir did not return every five-minute rate for this exact worker identity."
		}
		if activeLogErr != nil {
			evidence += " The best-effort active-task log lookup failed: " + activeLogErr.Error()
		}
		findings = append(findings, finding{
			probeId: "runtime/worker-memory-skew", tier: tierWarn,
			class: "worker-memory-skew", target: target, frame: worker.instance, sustain: 2,
			symptom: fmt.Sprintf(
				"taskworker %s holds %.2fGiB of allocated Go heap, %.1fx the %.2fGiB fleet median",
				target, bytesToGiB(worker.heap), ratio, bytesToGiB(median),
			),
			mechanism: "HeapAlloc is process Go-heap allocation, not filesystem cache or RSS retained after scavenging. It includes reachable objects plus unreachable objects not yet reclaimed by the next GC; either shape makes an allocation-heavy taskworker scan and collect a much larger heap, so unrelated tasks assigned to that process can overrun while identical work completes normally on peer executors.",
			baseline: fmt.Sprintf(
				"Fresh taskworker heap remains below %.0fGiB or within %.0fx of the fleet median for two consecutive one-minute samples; a sparse fleet uses a %.0fGiB absolute guard.",
				bytesToGiB(workerMemoryAbsoluteBytes), workerMemorySkewRatio, bytesToGiB(workerMemorySparseAbsoluteBytes),
			),
			observed: observed,
			evidence: evidence,
			context:  "A healthy host and cgroup do not clear this alert: ample free RAM, no OOM event, and no CPU throttle can coexist with severe process-local allocator/GC contention. Executor correlation is evidence of co-residency, not proof that any one task owns the entire heap.",
			action:   "Identify the co-resident task families, then roll out their bounded or streaming working-set fixes. Preserve task deadlines and durable evidence; do not restart the worker merely to erase the heap unless an explicitly authorized operational emergency requires it.",
			verify:   "The affected process returns below both the absolute and fleet-skew guards, heap-object count contracts, and the same task families complete inside their historical bands on consecutive scheduled runs without an OOM or deadline increase.",
			playbook: "SIGNALS.md 2.12",
		})
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("runtime/worker-memory-skew", tierWarn, "worker-memory-skew", "taskworker-fleet")}, nil
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].target < findings[j].target })
	return findings, nil
}

// shellFirstServiceGateway queries a loopback service through the first
// reachable services host. Service gateways front the same replicated
// backend, so one host's SSH or local proxy failure must not make that backend
// unobservable. The gateway that served the primary query is preferred for a
// related follow-up, avoiding a repeated timeout on a gateway that just
// failed.
func shellFirstServiceGateway(
	ctx context.Context,
	runner probeRunner,
	hosts []*host,
	preferred *host,
	command string,
) (string, *host, error) {
	ordered := make([]*host, 0, len(hosts))
	if preferred != nil {
		ordered = append(ordered, preferred)
	}
	for _, candidate := range hosts {
		if candidate != preferred {
			ordered = append(ordered, candidate)
		}
	}

	errs := make([]error, 0, len(ordered))
	for _, candidate := range ordered {
		out, err := runner.shell(ctx, candidate, command)
		if err == nil {
			return out, candidate, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", candidate.name, err))
		if ctx.Err() != nil {
			break
		}
	}
	return "", nil, errors.Join(errs...)
}

func mimirInstantValue(raw []json.RawMessage) (time.Time, float64, error) {
	if len(raw) != 2 {
		return time.Time{}, 0, fmt.Errorf("value has %d fields, want 2", len(raw))
	}
	var timestamp float64
	if err := json.Unmarshal(raw[0], &timestamp); err != nil {
		return time.Time{}, 0, fmt.Errorf("timestamp: %w", err)
	}
	var valueString string
	if err := json.Unmarshal(raw[1], &valueString); err != nil {
		var numericValue float64
		if numericErr := json.Unmarshal(raw[1], &numericValue); numericErr != nil {
			return time.Time{}, 0, fmt.Errorf("value: %w", err)
		}
		return unixFloatTime(timestamp), numericValue, nil
	}
	value, err := strconv.ParseFloat(valueString, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("value %q: %w", valueString, err)
	}
	return unixFloatTime(timestamp), value, nil
}

func unixFloatTime(value float64) time.Time {
	seconds, fraction := math.Modf(value)
	return time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC()
}

func medianFloat64(values []float64) float64 {
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func medianFloat64OrZero(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return medianFloat64(values)
}

func safeRatio(value, baseline float64) float64 {
	if baseline <= 0 {
		return 0
	}
	return value / baseline
}

func bytesToGiB(value float64) float64 { return value / float64(uint64(1)<<30) }
