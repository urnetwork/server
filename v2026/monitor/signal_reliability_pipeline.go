// Judge the cluster discovery backlog and each configured node's latency from
// independently completed native measurements, retaining partial positives.
package monitor

import (
	"context"
	"encoding/base64"
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

// Sample one cluster-scoped block count per host and every configured node.
type redisReliabilityLoadProbe struct{}

func (redisReliabilityLoadProbe) id() string             { return "redis/reliability-load" }
func (redisReliabilityLoadProbe) tier() string           { return tierWarn }
func (redisReliabilityLoadProbe) cadence() time.Duration { return 5 * time.Minute }

// Missing coverage withholds health without erasing an independently observed
// backlog or slow node. Only configured identities can supply measurements.
func (redisReliabilityLoadProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	entryHost := env.cfg.redisEntryHost()
	hosts := env.cfg.redisSweepHosts()
	if entryHost == nil || len(hosts) == 0 {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	findings := []finding{}
	type reliabilitySample struct {
		host    *host
		port    int
		latency float64
	}
	samples := []reliabilitySample{}
	blockCount := 0
	haveBlockCount := false
	configuredNodes := 0
	failures := []redisHostFailure{}
	for _, nodeHost := range hosts {
		ports := nodeHost.redisNodePorts()
		if len(ports) == 0 {
			return nil, fmt.Errorf("no redis node ports configured")
		}
		configuredNodes += len(ports)
		portArgs := make([]string, len(ports))
		expectedPorts := make(map[int]bool, len(ports))
		for i, port := range ports {
			if port <= 0 || 65535 < port || expectedPorts[port] {
				return nil, fmt.Errorf("invalid configured redis node ports")
			}
			expectedPorts[port] = true
			portArgs[i] = strconv.Itoa(port)
		}
		// Preserve raw bytes through encoding and capture both pipeline statuses.
		// The child trailer distinguishes native exit 124 from the sampling timeout.
		// GNU base64 -w 0 emits no newline, but the BSD encoder appends one that
		// would split the record row. The encoder group deletes it and, under
		// pipefail, reports one status for both commands.
		out, err := env.runner.shell(ctx, nodeHost, fmt.Sprintf(`/bin/bash -o pipefail <<'REDIS_RELIABILITY'
printf 'blocks\t'
timeout 2 redis-cli -c --raw -p %d SCARD client_reliability_stats_blocks 2>/dev/null | { base64 -w 0 | tr -d '\n'; } 2>/dev/null
source_statuses=("${PIPESTATUS[@]}")
printf '\t%%s\t%%s\n' "${source_statuses[0]}" "${source_statuses[1]}"
for p in %s; do
  printf 'latency\t%%s\t' "$p"
  timeout 2 sh -c '
    if redis-cli --raw -p "$1" --latency 2>/dev/null; then source_status=0; else source_status=$?; fi
    printf "\nreliability_exit=%%s\n" "$source_status"
    exit "$source_status"
  ' sh "$p" 2>/dev/null | { base64 -w 0 | tr -d '\n'; } 2>/dev/null
  source_statuses=("${PIPESTATUS[@]}")
  printf '\t%%s\t%%s\n' "${source_statuses[0]}" "${source_statuses[1]}"
done
printf 'complete\t%d\n'
REDIS_RELIABILITY`, ports[0], strings.Join(portArgs, " "), len(ports)))
		if err != nil {
			failures = append(failures, redisHostFailure{host: nodeHost, err: err})
			continue
		}
		observation := parseRedisReliabilityObservation(out, ports)
		for _, port := range ports {
			if latency, ok := observation.portLatencies[port]; ok {
				samples = append(samples, reliabilitySample{host: nodeHost, port: port, latency: latency})
			}
		}
		if !observation.blocksKnown || len(observation.unobservedPorts) > 0 || !observation.complete || observation.invalidRows {
			unknown := cannotObserveFinding(nodeHost.name+"/reliability-load", fmt.Errorf("incomplete Redis reliability observation"))
			unknown.observed += fmt.Sprintf(" observed_nodes=%d configured_nodes=%d unobserved_ports=%v block_count_observed=%t complete_frame=%t invalid_rows=%t",
				len(observation.portLatencies), len(ports), observation.unobservedPorts, observation.blocksKnown, observation.complete, observation.invalidRows)
			findings = append(findings, unknown)
		}
		// The entry host owns the cluster reading; another host can stand in when
		// it is unavailable. Missing local latency does not invalidate this count.
		if observation.blocksKnown && (!haveBlockCount || nodeHost == entryHost) {
			blockCount = observation.blocks
			haveBlockCount = true
		}
	}
	if len(failures) == len(hosts) {
		return nil, redisHostFailuresError(failures)
	}
	for _, failure := range failures {
		findings = append(findings, redisHostCannotObserveFinding(failure, "reliability-load"))
	}

	if haveBlockCount && 5 < blockCount {
		observed := fmt.Sprintf("blocks=%d nodes_sampled=%d nodes_configured=%d", blockCount, len(samples), configuredNodes)
		if len(samples) > 0 {
			maxSample := samples[0]
			for _, sample := range samples[1:] {
				if maxSample.latency < sample.latency {
					maxSample = sample
				}
			}
			observed += fmt.Sprintf(" max_latency_ms=%.2f max_latency_host=%s max_latency_port=%d", maxSample.latency, maxSample.host.name, maxSample.port)
		} else {
			observed += " latency_observed=false"
		}
		if len(failures) > 0 {
			observed += " " + redisUnobservedHosts(failures)
		}
		findings = append(findings, finding{
			probeId: "redis/reliability-load", tier: tierWarn,
			class: "reliability-pipeline-degraded", target: entryHost.name, sustain: 2,
			symptom:   fmt.Sprintf("reliability pipeline has %d pending discovery blocks across the Redis cluster", blockCount),
			mechanism: "A growing cluster-wide discovery set means the drain cannot keep up or cannot consume the writers' key format. The set occupies one Redis Cluster slot, so it must not be counted once per entry port.",
			baseline:  "SCARD client_reliability_stats_blocks is about 3 for the cluster; local Redis latency remains below 10ms even while busy.",
			observed:  observed,
			evidence:  "The SCARD source completed successfully with MOVED redirection enabled. Node latency has independent per-port authority; nodes_sampled and nodes_configured describe its coverage.",
			action:    "Verify the taskworker rollup understands the active writer format and advances its durable high-water mark; use node-local latency alerts to distinguish backlog from a sick Redis process.",
			verify:    "The one cluster-scoped block set converges near 3 and UpdateClientScores freshness remains healthy.",
			playbook:  "SIGNALS.md §3.7 and §2.8",
		})
	}

	for _, sample := range samples {
		if sample.latency <= 10 {
			continue
		}
		target := fmt.Sprintf("%s:%d", sample.host.name, sample.port)
		findings = append(findings, finding{
			probeId: "redis/reliability-load", tier: tierWarn,
			class: "reliability-pipeline-degraded", target: target, sustain: 2,
			symptom:   fmt.Sprintf("reliability workload on Redis %d has %.1fms average local latency", sample.port, sample.latency),
			mechanism: "Elevated node-local latency distinguishes Redis degradation from the reliability pipeline's normal high command volume; the discovery-set size is a separate cluster-wide metric.",
			baseline:  "Local Redis latency remains below 10ms even while the reliability pipeline is busy.",
			observed:  fmt.Sprintf("latency_avg_ms=%.2f", sample.latency),
			action:    "Attribute SLOWLOG keys and verify sharded reliability writers plus the taskworker drain before treating high operations as an incident.",
			verify:    "Local latency returns below 10ms; separately confirm the cluster block set converges near 3 and UpdateClientScores freshness remains healthy.",
			playbook:  "SIGNALS.md §3.7 and §2.8",
		})
	}
	if len(findings) == 0 {
		findings = append(findings, healthyFinding("redis/reliability-load", tierWarn, "reliability-pipeline-degraded", entryHost.name))
	}
	return findings, nil
}

// Keep block and node authority separate, including exact missing port coverage.
type redisReliabilityObservation struct {
	blocks          int
	blocksKnown     bool
	portLatencies   map[int]float64
	unobservedPorts []int
	complete        bool
	invalidRows     bool
}

// Reconcile framed records against inventory. Duplicated measurements lose
// authority, foreign rows cannot substitute for a configured port, and a
// missing terminal frame withholds health while completed positives survive.
func parseRedisReliabilityObservation(output string, ports []int) redisReliabilityObservation {
	observation := redisReliabilityObservation{portLatencies: make(map[int]float64, len(ports))}
	expectedPorts := make(map[int]bool, len(ports))
	seenPorts := make(map[int]bool, len(ports))
	for _, port := range ports {
		expectedPorts[port] = true
	}
	blocksSeen, completeSeen := false, false
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if completeSeen {
			observation.complete = false
			observation.invalidRows = true
		}
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "blocks":
			if blocksSeen {
				observation.blocksKnown = false
				observation.invalidRows = true
				continue
			}
			blocksSeen = true
			if len(fields) != 4 || fields[2] != "0" || fields[3] != "0" {
				continue
			}
			body, ok := decodeRedisReliabilityBody(fields[1])
			if !ok {
				continue
			}
			observation.blocks, observation.blocksKnown = parseRedisReliabilityCount(body)
		case "latency":
			if len(fields) < 2 {
				observation.invalidRows = true
				continue
			}
			port, ok := parseRedisReliabilityCount(fields[1])
			if !ok || strconv.Itoa(port) != fields[1] || !expectedPorts[port] {
				observation.invalidRows = true
				continue
			}
			if seenPorts[port] {
				delete(observation.portLatencies, port)
				observation.invalidRows = true
				continue
			}
			seenPorts[port] = true
			if len(fields) != 5 || (fields[3] != "0" && fields[3] != "124") || fields[4] != "0" {
				continue
			}
			body, ok := decodeRedisReliabilityBody(fields[2])
			if !ok {
				continue
			}
			if fields[3] == "0" {
				if !strings.HasSuffix(body, "\nreliability_exit=0") {
					continue
				}
				body = strings.TrimSuffix(body, "\nreliability_exit=0")
			}
			// A timeout-owned cutoff has no child-exit trailer. Any child exit
			// marker in a 124 body fails the strict latency grammar below.
			latency, err := parseRedisLatency(body)
			if err == nil {
				observation.portLatencies[port] = latency
			}
		case "complete":
			if completeSeen {
				continue
			}
			completeSeen = true
			observation.complete = len(fields) == 2 && fields[1] == strconv.Itoa(len(ports))
		default:
			observation.invalidRows = true
		}
	}
	for _, port := range ports {
		if _, ok := observation.portLatencies[port]; !ok {
			observation.unobservedPorts = append(observation.unobservedPorts, port)
		}
	}
	return observation
}

// Encoding confines arbitrary native output to its own record. The bound is
// well above a two-second latency sample; neither body nor decoder errors escape.
func decodeRedisReliabilityBody(encoded string) (string, bool) {
	if len(encoded) > 64*1024 {
		return "", false
	}
	body, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(body) != encoded {
		return "", false
	}
	return strings.TrimSuffix(string(body), "\n"), true
}

// Accept only a bounded unsigned decimal integer, preserving explicit zero.
func parseRedisReliabilityCount(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, digit := range value {
		if digit < '0' || '9' < digit {
			return 0, false
		}
	}
	count, err := strconv.Atoi(value)
	return count, err == nil
}

// Validate every retained update, including sample count and the min/max/avg
// bounds. Numeric and legacy labelled Redis formats share the same grammar;
// an error preceding a numeric last line cannot masquerade as a measurement.
func parseRedisLatency(output string) (float64, error) {
	latency := 0.0
	haveSample := false
	for _, line := range strings.FieldsFunc(output, func(r rune) bool { return r == '\r' || r == '\n' }) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) == 8 && fields[0] == "min:" && fields[2] == "max:" && fields[4] == "avg:" && fields[7] == "samples)" &&
			strings.HasSuffix(fields[1], ",") && strings.HasSuffix(fields[3], ",") && strings.HasPrefix(fields[6], "(") {
			fields = []string{strings.TrimSuffix(fields[1], ","), strings.TrimSuffix(fields[3], ","), fields[5], strings.TrimPrefix(fields[6], "(")}
		}
		if len(fields) != 4 {
			return 0, fmt.Errorf("invalid latency sample shape")
		}
		values := [3]float64{}
		for i, value := range fields[:3] {
			for _, digit := range value {
				if (digit < '0' || '9' < digit) && digit != '.' {
					return 0, fmt.Errorf("invalid latency number")
				}
			}
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || parsed < 0 || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
				return 0, fmt.Errorf("invalid latency number")
			}
			values[i] = parsed
		}
		count, ok := parseRedisReliabilityCount(fields[3])
		if !ok || count == 0 || values[1] < values[0] || values[2] < values[0] || values[1] < values[2] {
			return 0, fmt.Errorf("invalid latency sample bounds")
		}
		latency = values[2]
		haveSample = true
	}
	if !haveSample {
		return 0, fmt.Errorf("empty latency sample")
	}
	return latency, nil
}
