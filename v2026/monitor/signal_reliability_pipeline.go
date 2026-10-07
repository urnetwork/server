package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §3.7 maps to signal_reliability_pipeline.go and signal_reliability_pipeline_test.go.
func NewReliabilityPipelineSignal() Signal {
	return &signalAdapter{number: "3.7", key: "reliability-pipeline", name: "Reliability pipeline load", probe: redisReliabilityLoadProbe{}}
}

type redisReliabilityLoadProbe struct{}

func (redisReliabilityLoadProbe) id() string             { return "redis/reliability-load" }
func (redisReliabilityLoadProbe) tier() string           { return tierWarn }
func (redisReliabilityLoadProbe) cadence() time.Duration { return 5 * time.Minute }

func (redisReliabilityLoadProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	host := env.cfg.hostByRole("redis-cluster")
	if host == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	ports := host.redisNodePorts()
	if len(ports) == 0 {
		return nil, fmt.Errorf("no redis node ports configured")
	}
	portArgs := make([]string, len(ports))
	for i, port := range ports {
		portArgs[i] = strconv.Itoa(port)
	}
	// The discovery set occupies one cluster slot. Read it once with redirect
	// following; reading it inside the node loop duplicates the same cluster
	// metric under every entry-port identity. Tabs preserve each command's
	// complete output as one field, and Go validates every number instead of
	// coercing malformed output to zero. redis-cli 8 emits latency as four
	// numeric columns (min max avg samples), while older versions used labelled
	// fields; parseRedisLatency supports both formats.
	out, err := env.runner.shell(ctx, host, fmt.Sprintf(`blocks=$(timeout 2 redis-cli -c --raw -p %d SCARD client_reliability_stats_blocks 2>/dev/null)
for p in %s; do
  latency=$(timeout 2 redis-cli --raw -p "$p" --latency 2>/dev/null | tr '\r' '\n' | awk 'NF { last=$0 } END { print last }')
  printf '%%s\t%%s\t%%s\n' "$p" "$blocks" "$latency"
done`, ports[0], strings.Join(portArgs, " ")))
	if err != nil {
		return nil, err
	}
	findings := []finding{}
	type reliabilitySample struct {
		port    int
		blocks  int
		latency float64
	}
	samples := []reliabilitySample{}
	blockCount := 0
	haveBlockCount := false
	inconsistentBlockCount := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		port, blocks, latency, parseErr := parseRedisReliabilitySample(line)
		if parseErr != nil {
			target := host.name
			if first, _, ok := strings.Cut(line, "\t"); ok && strings.TrimSpace(first) != "" {
				target += ":" + strings.TrimSpace(first)
			}
			findings = append(findings, cannotObserveFinding(
				"redis/reliability-load/"+target,
				parseErr,
			))
			continue
		}
		samples = append(samples, reliabilitySample{port: port, blocks: blocks, latency: latency})
		if !haveBlockCount {
			blockCount = blocks
			haveBlockCount = true
		} else if blocks != blockCount && !inconsistentBlockCount {
			inconsistentBlockCount = true
			findings = append(findings, cannotObserveFinding(
				"redis/reliability-load/"+host.name,
				fmt.Errorf("inconsistent cluster block-set cardinality: %d on the first sample, %d on Redis %d", blockCount, blocks, port),
			))
		}
	}

	if haveBlockCount && !inconsistentBlockCount && 5 < blockCount {
		maxLatency := 0.0
		maxLatencyPort := 0
		for _, sample := range samples {
			if maxLatency < sample.latency {
				maxLatency = sample.latency
				maxLatencyPort = sample.port
			}
		}
		findings = append(findings, finding{
			probeId: "redis/reliability-load", tier: tierWarn,
			class: "reliability-pipeline-degraded", target: host.name, sustain: 2,
			symptom:   fmt.Sprintf("reliability pipeline has %d pending discovery blocks across the Redis cluster", blockCount),
			mechanism: "A growing cluster-wide discovery set means the drain cannot keep up or cannot consume the writers' key format. The set occupies one Redis Cluster slot, so it must not be counted once per entry port.",
			baseline:  "SCARD client_reliability_stats_blocks is about 3 for the cluster; local Redis latency remains below 10ms even while busy.",
			observed:  fmt.Sprintf("blocks=%d nodes_sampled=%d max_latency_ms=%.2f max_latency_port=%d", blockCount, len(samples), maxLatency, maxLatencyPort),
			evidence:  "The SCARD value was collected once with MOVED redirection enabled; node-local latency was sampled independently on every configured Redis process.",
			action:    "Verify the taskworker rollup understands the active writer format and advances its durable high-water mark; use node-local latency alerts to distinguish backlog from a sick Redis process.",
			verify:    "The one cluster-scoped block set converges near 3 and UpdateClientScores freshness remains healthy.",
			playbook:  "SIGNALS.md §3.7 and §2.8",
		})
	}

	for _, sample := range samples {
		if sample.latency <= 10 {
			continue
		}
		target := fmt.Sprintf("%s:%d", host.name, sample.port)
		findings = append(findings, finding{
			probeId: "redis/reliability-load", tier: tierWarn,
			class: "reliability-pipeline-degraded", target: target, sustain: 2,
			symptom:   fmt.Sprintf("reliability workload on Redis %d has %.1fms average local latency", sample.port, sample.latency),
			mechanism: "Elevated node-local latency distinguishes Redis degradation from the reliability pipeline's normal high command volume; the discovery-set size is a separate cluster-wide metric.",
			baseline:  "Local Redis latency remains below 10ms even while the reliability pipeline is busy.",
			observed:  fmt.Sprintf("blocks=%d latency_avg_ms=%.2f", sample.blocks, sample.latency),
			action:    "Attribute SLOWLOG keys and verify sharded reliability writers plus the taskworker drain before treating high operations as an incident.",
			verify:    "Local latency returns below 10ms; separately confirm the cluster block set converges near 3 and UpdateClientScores freshness remains healthy.",
			playbook:  "SIGNALS.md §3.7 and §2.8",
		})
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("redis/reliability-load", tierWarn, "reliability-pipeline-degraded", host.name))
	}
	return findings, nil
}

func parseRedisReliabilitySample(line string) (port int, blocks int, latency float64, err error) {
	fields := strings.SplitN(strings.TrimSuffix(line, "\r"), "\t", 3)
	if len(fields) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid reliability sample %q: expected three tab-separated fields", line)
	}
	port, err = strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil || port <= 0 {
		return 0, 0, 0, fmt.Errorf("invalid Redis port %q", fields[0])
	}
	blocks, err = strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil || blocks < 0 {
		return 0, 0, 0, fmt.Errorf("invalid SCARD result on Redis %d: %q", port, fields[1])
	}
	latency, err = parseRedisLatency(fields[2])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid latency result on Redis %d: %w", port, err)
	}
	return port, blocks, latency, nil
}

func parseRedisLatency(output string) (float64, error) {
	var sample string
	for _, line := range strings.FieldsFunc(output, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if strings.TrimSpace(line) != "" {
			sample = strings.TrimSpace(line)
		}
	}
	if sample == "" {
		return 0, fmt.Errorf("empty output")
	}

	var value string
	fields := strings.Fields(sample)
	for i, field := range fields {
		if strings.TrimRight(field, ":") == "avg" && i+1 < len(fields) {
			value = fields[i+1]
			break
		}
	}
	if value == "" && len(fields) >= 4 {
		// redis-cli 8: <min> <max> <avg> <samples>
		value = fields[2]
	}
	value = strings.Trim(value, " ,()")
	latency, err := strconv.ParseFloat(value, 64)
	if err != nil || latency < 0 || math.IsInf(latency, 0) || math.IsNaN(latency) {
		return 0, fmt.Errorf("unrecognized output %q", sample)
	}
	return latency, nil
}
