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
	workerCapFreshness       = 90 * time.Second
	workerCapCPUFraction     = 0.90
	workerCapGoroutineFloor  = 10_000
	workerCapMaxResponseSize = 4 << 20
)

// Signal worker-cap implements SIGNALS.md §2.12b. Host CPU headroom does not
// reveal a Taskworker pinned near its own GOMAXPROCS while probes are queued.
func NewWorkerCapSignal() Signal {
	return &signalAdapter{
		number: "2.12b", key: "worker-cap", name: "Taskworker scheduler capacity",
		probe: workerCapProbe{},
	}
}

type workerCapProbe struct{}

func (workerCapProbe) id() string             { return "runtime/worker-scheduler-capacity" }
func (workerCapProbe) tier() string           { return tierWarn }
func (workerCapProbe) cadence() time.Duration { return time.Minute }

type workerCapMetrics struct {
	host, block, instance     string
	cpu, maxprocs, goroutines float64
	mask                      uint8
	ambiguous                 bool
}

const (
	workerCapCPU uint8 = 1 << iota
	workerCapMaxprocs
	workerCapGoroutines
	workerCapAll = workerCapCPU | workerCapMaxprocs | workerCapGoroutines
)

func workerCapQuery(environment string) string {
	env := strconv.Quote(environment)
	parts := make([]string, 0, 3)
	for _, metric := range []struct {
		name, label string
		rate        bool
	}{
		{"process_cpu_seconds_total", "cpu", true},
		{"go_sched_gomaxprocs_threads", "maxprocs", false},
		{"go_goroutines", "goroutines", false},
	} {
		selector := fmt.Sprintf(`%s{env=%s,job="taskworker"}`, metric.name, env)
		value := selector
		if metric.rate {
			value = fmt.Sprintf("rate(%s[5m])", selector)
		}
		// An instant-query evaluation timestamp is not proof of source
		// freshness. The raw counter/gauge must itself be recent.
		parts = append(parts, fmt.Sprintf(
			`label_replace((%s and (timestamp(%s) >= time() - 90)),"monitor_metric",%s,"job",".*")`,
			value, selector, strconv.Quote(metric.label),
		))
	}
	return strings.Join(parts, " or ")
}

func (workerCapProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("worker cap: no services host in inventory")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(workerCapQuery(env.cfg.env))
	output, gateway, err := shellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryURL+"'")
	if err != nil {
		return nil, fmt.Errorf("worker cap: query Mimir: %w", err)
	}
	if len(output) > workerCapMaxResponseSize {
		return nil, fmt.Errorf("worker cap: oversized metric response")
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("worker cap: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("worker cap: Mimir response was not a successful vector")
	}
	now := env.now().UTC()
	workers := map[string]*workerCapMetrics{}
	for _, series := range response.Data.Result {
		kind := series.Metric["monitor_metric"]
		if kind != "cpu" && kind != "maxprocs" && kind != "goroutines" {
			continue
		}
		host, block, instance := series.Metric["host"], series.Metric["block"], series.Metric["instance"]
		if !safeServiceLoadLabel(host) || !safeServiceLoadLabel(block) || !safeServiceLoadLabel(instance) {
			continue
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("worker cap: invalid metric value")
		}
		if age := now.Sub(observedAt); age > workerCapFreshness || age < -30*time.Second ||
			math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			continue
		}
		key := host + "\x00" + block + "\x00" + instance
		worker := workers[key]
		if worker == nil {
			worker = &workerCapMetrics{host: host, block: block, instance: instance}
			workers[key] = worker
		}
		switch kind {
		case "cpu":
			worker.ambiguous = worker.ambiguous || worker.mask&workerCapCPU != 0
			worker.cpu, worker.mask = value, worker.mask|workerCapCPU
		case "maxprocs":
			worker.ambiguous = worker.ambiguous || worker.mask&workerCapMaxprocs != 0
			worker.maxprocs, worker.mask = value, worker.mask|workerCapMaxprocs
		case "goroutines":
			worker.ambiguous = worker.ambiguous || worker.mask&workerCapGoroutines != 0
			worker.goroutines, worker.mask = value, worker.mask|workerCapGoroutines
		}
	}
	findings := []finding{}
	paired, incomplete := 0, 0
	for _, worker := range workers {
		if worker.ambiguous || worker.mask != workerCapAll || worker.maxprocs < 1 {
			incomplete++
			continue
		}
		paired++
		target := worker.host + "/" + worker.block
		if worker.cpu < workerCapCPUFraction*worker.maxprocs ||
			worker.goroutines < workerCapGoroutineFloor {
			healthy := healthyFinding("runtime/worker-scheduler-capacity", tierWarn,
				"worker-scheduler-capacity", target)
			healthy.frame = worker.instance
			findings = append(findings, healthy)
			continue
		}
		findings = append(findings, finding{
			probeId: "runtime/worker-scheduler-capacity", tier: tierWarn,
			class: "worker-scheduler-capacity", target: target, frame: worker.instance, sustain: 2,
			symptom:   "A Taskworker is near its own Go scheduler parallelism while many goroutines remain resident.",
			mechanism: "The process can be CPU-limited by GOMAXPROCS even when its host has spare cores. Probe retry chains, tunnel/DNS dials, and co-resident tasks compete within that process; CPU use alone does not identify which stage is slow.",
			baseline:  fmt.Sprintf("Five-minute process CPU is below %.0f%% of its own GOMAXPROCS, or the runtime has fewer than %d goroutines.", 100*workerCapCPUFraction, workerCapGoroutineFloor),
			observed: fmt.Sprintf("cpu_cores_5m=%.3f gomaxprocs=%.0f cpu_to_gomaxprocs=%.3f goroutines=%.0f block=%s instance=%s metrics_gateway=%s",
				worker.cpu, worker.maxprocs, worker.cpu/worker.maxprocs, worker.goroutines,
				worker.block, worker.instance, gateway.name),
			evidence: "Source-fresh process CPU, GOMAXPROCS, and goroutine metrics are joined by exact host/block/runtime instance. The instant-query evaluation time alone is not accepted as source freshness.",
			context:  "This is process scheduler pressure, not proof of host CPU exhaustion or an unbounded goroutine leak. A provider probe holds its worker across spaced retries; gVisor starts TCP dispatcher workers per private tunnel based on GOMAXPROCS, so lifting the cap without controlling tunnel fanout may multiply goroutines. Correlate with §2.19 throughput and exact task ownership.",
			action:   "Measure per-process task ownership, probe-stage latency, and CPU/DB/API headroom. Bound detached DoH dials and distribute durable probe shards before raising aggregate admission. Do not simply remove GOMAXPROCS or weaken probe guards to clear this warning.",
			verify:   "For two fresh cadences the exact process is below the scheduler-pressure threshold, due probe queues and measured coverage improve, and PostgreSQL/API/host capacity and provider-quality controls remain healthy.",
			playbook: "SIGNALS.md §2.12b and §2.19",
		})
	}
	if paired == 0 || incomplete > 0 {
		findings = append(findings, finding{
			probeId: "runtime/worker-scheduler-capacity", tier: tierWarn,
			class: "worker-scheduler-unobservable", target: env.cfg.env + "/taskworker", sustain: 2,
			symptom:   "Taskworker CPU/GOMAXPROCS capacity is not observable for every returned runtime.",
			mechanism: "Missing, stale, malformed, or different-generation process metrics cannot establish a scheduler-capacity ratio. A healthy host CPU panel is not a substitute.",
			baseline:  "Every reported Taskworker runtime has one fresh CPU rate, GOMAXPROCS gauge, and goroutine gauge with the same host/block/instance identity.",
			observed:  fmt.Sprintf("paired_runtimes=%d incomplete_runtimes=%d", paired, incomplete),
			evidence:  "Only fixed metric names and aggregate runtime counts enter the alert; raw series and source errors remain private.",
			context:   "This is a visibility gap, not a declaration that probe throughput is healthy or unhealthy.",
			action:    "Restore complete source-fresh runtime exports and inspect exact process generations before evaluating CPU headroom.",
			verify:    "Every active Taskworker runtime has the complete exact-instance metric tuple on two cadences.",
			playbook:  "SIGNALS.md §2.12b",
		})
	} else {
		findings = append(findings, healthyFinding("runtime/worker-scheduler-capacity", tierWarn,
			"worker-scheduler-unobservable", env.cfg.env+"/taskworker"))
	}
	return findings, nil
}
