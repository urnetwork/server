package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const lokiTailersMarker = "monitor-signal-11.19-loki-tailers"

// Signal loki-tailers implements SIGNALS.md §11.19. It reads every bundled
// Loki child's own metrics because a healthy Grafana parent and connected
// external WebSocket do not prove that Loki's live-tail accounting is valid.
func NewLokiTailersSignal() Signal {
	return &signalAdapter{
		number: "11.19", key: "loki-tailers", name: "Loki live-tail accounting integrity",
		probe: lokiTailersProbe{},
	}
}

type lokiTailersProbe struct{}

func (lokiTailersProbe) id() string             { return "observability/loki-tailers" }
func (lokiTailersProbe) tier() string           { return tierWarn }
func (lokiTailersProbe) cadence() time.Duration { return time.Minute }

type lokiTailInstance struct {
	version       string
	processStart  float64
	tailsActive   float64
	streamsActive float64
	tailsSeen     bool
	streamsSeen   bool
	processSeen   bool
}

type lokiTailHostSample struct {
	instances []lokiTailInstance
	count     int
	countSeen bool
}

type lokiTailHostResult struct {
	host   *host
	sample lokiTailHostSample
	err    error
}

func (lokiTailersProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("loki tailers: no services hosts in inventory")
	}

	command := "# " + lokiTailersMarker + "\n" + lokiTailersScript
	results := make(chan lokiTailHostResult, len(hosts))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, configuredHost := range hosts {
		target := configuredHost
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- lokiTailHostResult{host: target, err: ctx.Err()}
				return
			}

			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				results <- lokiTailHostResult{host: target, err: err}
				return
			}
			sample, err := parseLokiTailHostSample(output)
			results <- lokiTailHostResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]lokiTailHostResult, 0, len(hosts))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })

	findings := make([]finding, 0, len(ordered)*2)
	for _, result := range ordered {
		target := result.host.name
		if result.err != nil {
			findings = append(findings, cannotObserveFinding(target+"/loki-tailers", result.err))
			continue
		}
		if result.sample.count == 0 {
			findings = append(findings, lokiTailChildMissingFinding(target))
			continue
		}
		findings = append(findings, healthyFinding(
			"observability/loki-tailers", tierWarn, "loki-tail-child-missing", target,
		))

		invalid := make([]string, 0, len(result.sample.instances))
		for instanceIndex, instance := range result.sample.instances {
			identity := lokiTailInstanceIdentity(instanceIndex, instance)
			switch {
			case !instance.tailsSeen || !instance.streamsSeen:
				invalid = append(invalid, fmt.Sprintf(
					"%s version=%s tails_present=%t streams_present=%t",
					identity, firstNonempty(instance.version, "unknown"), instance.tailsSeen, instance.streamsSeen,
				))
			case math.IsNaN(instance.tailsActive) || math.IsInf(instance.tailsActive, 0) ||
				math.IsNaN(instance.streamsActive) || math.IsInf(instance.streamsActive, 0):
				invalid = append(invalid, fmt.Sprintf(
					"%s version=%s tails_active=%s streams_active=%s",
					identity, firstNonempty(instance.version, "unknown"),
					formatLokiGauge(instance.tailsActive), formatLokiGauge(instance.streamsActive),
				))
			case instance.tailsActive < 0 || instance.streamsActive < 0:
				invalid = append(invalid, fmt.Sprintf(
					"%s version=%s tails_active=%s streams_active=%s",
					identity, firstNonempty(instance.version, "unknown"),
					formatLokiGauge(instance.tailsActive), formatLokiGauge(instance.streamsActive),
				))
			}
		}

		if len(invalid) == 0 {
			findings = append(findings, healthyFinding(
				"observability/loki-tailers", tierWarn, "loki-tail-accounting-invalid", target,
			))
			continue
		}
		findings = append(findings, lokiTailAccountingInvalidFinding(target, result.sample.count, invalid))
	}
	return findings, nil
}

func lokiTailInstanceIdentity(index int, instance lokiTailInstance) string {
	if instance.processSeen && instance.processStart > 0 {
		return fmt.Sprintf("process_start=%.0f", instance.processStart)
	}
	return fmt.Sprintf("instance=%d", index+1)
}

func formatLokiGauge(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func lokiTailChildMissingFinding(host string) finding {
	return finding{
		probeId: "observability/loki-tailers", tier: tierWarn,
		class: "loki-tail-child-missing", target: host, sustain: 2,
		symptom:   fmt.Sprintf("%s has no locally observable Loki live-tail metrics", host),
		mechanism: "The Grafana bundle parent can remain alive while its Loki child is absent, starting, or no longer exposes the metrics needed to validate live-tail accounting. The probe enumerated loopback listeners and found no Prometheus endpoint carrying Loki's active-tail gauges.",
		baseline:  "Every active services host has at least one locally reachable Loki child throughout steady state; a rollout may temporarily expose two generations.",
		observed:  "loki_instances=0",
		context:   "This is observation loss for the host's Loki child, not proof that every replicated Loki process is down. The Grafana ingress and standing-tailer signals independently test the parent route and external streams.",
		action:    "Inspect the host's Grafana unit, parent /status response, and bounded child journal. Restore the Loki child or its local metrics endpoint; do not infer healthy tail accounting from a sibling replica.",
		verify:    "The host exposes a Loki Prometheus endpoint with finite, non-negative active-tail and active-stream gauges for two consecutive probes.",
		playbook:  "SIGNALS.md §11.19 and §11.9",
	}
}

func lokiTailAccountingInvalidFinding(host string, instances int, invalid []string) finding {
	return finding{
		probeId: "observability/loki-tailers", tier: tierWarn,
		class: "loki-tail-accounting-invalid", target: host, sustain: 1,
		symptom:   fmt.Sprintf("%s Loki exports impossible live-tail accounting for %d of %d process(es)", host, len(invalid), instances),
		mechanism: "Loki 3.7.3 increments each tail gauge once when it constructs a Tailer. At the one-hour tail_max_duration or after every ingester connection is lost, the tail loop calls close; the HTTP handler then unconditionally runs its deferred close. Tailer.close is not idempotent and decrements both gauges on every call, so one real tail can be subtracted twice. The same close path remains non-idempotent in the official Loki main source inspected on 2026-08-31.",
		baseline:  "loki_querier_tail_active and loki_querier_tail_active_streams are present, finite, and >= 0 on every exact Loki process",
		observed:  fmt.Sprintf("loki_instances=%d invalid_instances=%d details=%s", instances, len(invalid), strings.Join(invalid, "; ")),
		evidence:  "Direct loopback /metrics values from the exact Loki child; process_start_time_seconds separates overlapping rollout generations.",
		context:   "A negative gauge is an instrumentation invariant violation, not a negative number of real tails and not by itself proof of dropped logs. It makes these gauges unsafe for proving duplicate collectors or tail capacity; use the separate Loki dropped-stream, backend-EOF, and external-tailer findings for data-path health.",
		action:    "Build and deploy a Grafana image containing Warp commit ba01c98 or later. It compiles checksum-pinned Loki 3.7.3 with Tailer.close guarded by sync.Once and includes the earlier idle-tail transport fix 1e95aef. Preserve the one-hour lifecycle limit and HTTP cleanup path; do not clamp the exported gauge, lengthen tail_max_duration, or restart Loki merely to erase the negative value.",
		verify:    "After the patched Loki processes start, hold a synthetic tail past tail_max_duration and force an all-ingester-disconnect recovery; each lifecycle decrements once, both gauges remain non-negative, and this signal stays clear through two one-hour rotations.",
		playbook:  "SIGNALS.md §11.19",
	}
}

func parseLokiTailHostSample(output string) (lokiTailHostSample, error) {
	sample := lokiTailHostSample{}
	current := -1
	for lineNumber, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "instance_begin":
			if len(fields) != 2 || current >= 0 {
				return sample, fmt.Errorf("loki tailers line %d: invalid instance_begin", lineNumber+1)
			}
			sample.instances = append(sample.instances, lokiTailInstance{})
			current = len(sample.instances) - 1
		case "instance_end":
			if len(fields) != 1 || current < 0 {
				return sample, fmt.Errorf("loki tailers line %d: unexpected instance_end", lineNumber+1)
			}
			current = -1
		case "loki_count":
			if len(fields) != 2 || sample.countSeen {
				return sample, fmt.Errorf("loki tailers line %d: invalid loki_count", lineNumber+1)
			}
			count, err := strconv.Atoi(fields[1])
			if err != nil || count < 0 {
				return sample, fmt.Errorf("loki tailers line %d: invalid loki_count %q", lineNumber+1, fields[1])
			}
			sample.count = count
			sample.countSeen = true
		default:
			if current < 0 {
				return sample, fmt.Errorf("loki tailers line %d: field outside instance: %q", lineNumber+1, fields[0])
			}
			instance := &sample.instances[current]
			switch fields[0] {
			case "version":
				if len(fields) != 2 || instance.version != "" {
					return sample, fmt.Errorf("loki tailers line %d: invalid version", lineNumber+1)
				}
				instance.version = fields[1]
			case "process_start", "tails_active", "streams_active":
				if len(fields) != 2 {
					return sample, fmt.Errorf("loki tailers line %d: invalid %s", lineNumber+1, fields[0])
				}
				value, err := strconv.ParseFloat(fields[1], 64)
				if err != nil {
					return sample, fmt.Errorf("loki tailers line %d: invalid %s %q", lineNumber+1, fields[0], fields[1])
				}
				switch fields[0] {
				case "process_start":
					if instance.processSeen || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
						return sample, fmt.Errorf("loki tailers line %d: invalid process_start %q", lineNumber+1, fields[1])
					}
					instance.processStart, instance.processSeen = value, true
				case "tails_active":
					if instance.tailsSeen {
						return sample, fmt.Errorf("loki tailers line %d: duplicate tails_active", lineNumber+1)
					}
					instance.tailsActive, instance.tailsSeen = value, true
				case "streams_active":
					if instance.streamsSeen {
						return sample, fmt.Errorf("loki tailers line %d: duplicate streams_active", lineNumber+1)
					}
					instance.streamsActive, instance.streamsSeen = value, true
				}
			default:
				return sample, fmt.Errorf("loki tailers line %d: unknown field %q", lineNumber+1, fields[0])
			}
		}
	}
	if current >= 0 {
		return sample, fmt.Errorf("loki tailers: unterminated instance")
	}
	if !sample.countSeen {
		return sample, fmt.Errorf("loki tailers: missing loki_count")
	}
	if sample.count != len(sample.instances) {
		return sample, fmt.Errorf("loki tailers: count=%d instances=%d", sample.count, len(sample.instances))
	}
	return sample, nil
}

const lokiTailersScript = `set -u
for required in ss curl awk sort; do
  if ! command -v "$required" >/dev/null 2>&1; then
    printf 'loki tailers probe prerequisite missing: %s\n' "$required" >&2
    exit 1
  fi
done
loki_count=0
ports=$(ss -ltnH 2>/dev/null | awk '$4 ~ /^127[.]0[.]0[.]1:[0-9]+$/ {sub(/.*:/, "", $4); print $4}' | sort -n -u)
for port in $ports; do
  metrics=$(curl -fsS --max-time 10 "http://127.0.0.1:${port}/metrics" 2>/dev/null || true)
  case "$metrics" in
    *'loki_querier_tail_active'*) ;;
    *) continue ;;
  esac

  loki_count=$((loki_count+1))
  printf 'instance_begin %s\n' "$port"
  printf '%s\n' "$metrics" | awk '
    /^loki_build_info[{]/ {
      if (match($1, /[,{]version="[^"]+"/)) {
        value=substr($1, RSTART, RLENGTH)
        sub(/^[,{]version="/, "", value)
        sub(/"$/, "", value)
        print "version", value
      }
    }
    /^process_start_time_seconds / {print "process_start", $2}
    /^loki_querier_tail_active / {print "tails_active", $2}
    /^loki_querier_tail_active_streams / {print "streams_active", $2}
  '
  printf 'instance_end\n'
done
printf 'loki_count %s\n' "$loki_count"
`
