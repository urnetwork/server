package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const mimirShutdownMarker = "monitor-signal-11.21-mimir-shutdown"

// Signal mimir-shutdown implements SIGNALS.md §11.21. It reads the exact
// shutdown-flush setting from each bundled Mimir child without returning the
// rest of the rendered configuration, which can contain credentials.
func NewMimirShutdownSignal() Signal {
	return &signalAdapter{
		number: "11.21", key: "mimir-shutdown", name: "Mimir shutdown durability configuration",
		probe: mimirShutdownProbe{},
	}
}

type mimirShutdownProbe struct{}

func (mimirShutdownProbe) id() string             { return "observability/mimir-shutdown" }
func (mimirShutdownProbe) tier() string           { return tierWarn }
func (mimirShutdownProbe) cadence() time.Duration { return 5 * time.Minute }

type mimirShutdownInstance struct {
	port      int
	flush     bool
	flushSeen bool
}

type mimirShutdownHostSample struct {
	instances []mimirShutdownInstance
	count     int
	countSeen bool
}

type mimirShutdownHostResult struct {
	host   *host
	sample mimirShutdownHostSample
	err    error
}

func (mimirShutdownProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("mimir shutdown: no services hosts in inventory")
	}

	command := "# " + mimirShutdownMarker + "\n" + mimirShutdownScript
	results := make(chan mimirShutdownHostResult, len(hosts))
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
				results <- mimirShutdownHostResult{host: target, err: ctx.Err()}
				return
			}

			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				results <- mimirShutdownHostResult{host: target, err: err}
				return
			}
			sample, err := parseMimirShutdownHostSample(output)
			results <- mimirShutdownHostResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]mimirShutdownHostResult, 0, len(hosts))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })

	findings := make([]finding, 0, len(ordered)+1)
	complete := true
	observableHosts := 0
	instanceCount := 0
	disabled := []string{}
	for _, result := range ordered {
		target := result.host.name
		if result.err != nil {
			complete = false
			findings = append(findings, cannotObserveFinding(target+"/mimir-shutdown", result.err))
			continue
		}
		if result.sample.count == 0 {
			complete = false
			findings = append(findings, mimirShutdownChildMissingFinding(target))
			continue
		}

		observableHosts++
		instanceCount += result.sample.count
		findings = append(findings, healthyFinding(
			"observability/mimir-shutdown", tierWarn, "mimir-shutdown-child-missing", target,
		))
		for _, instance := range result.sample.instances {
			if !instance.flush {
				disabled = append(disabled, fmt.Sprintf("%s:%d=false", target, instance.port))
			}
		}
	}

	if len(disabled) > 0 {
		findings = append(findings, mimirShutdownFlushDisabledFinding(
			len(hosts), observableHosts, instanceCount, disabled,
		))
	} else if complete {
		findings = append(findings, healthyFinding(
			"observability/mimir-shutdown", tierWarn,
			"mimir-shutdown-flush-disabled", "mimir-fleet",
		))
	}
	return findings, nil
}

func mimirShutdownChildMissingFinding(host string) finding {
	return finding{
		probeId: "observability/mimir-shutdown", tier: tierWarn,
		class: "mimir-shutdown-child-missing", target: host, sustain: 2,
		symptom:   fmt.Sprintf("%s has no locally observable Mimir shutdown configuration", host),
		mechanism: "The Grafana bundle parent can remain alive while its Mimir child is absent, starting, or no longer exposes a loopback configuration endpoint. Without the exact child setting, this host cannot prove that its unshipped TSDB head survives a clean rollout.",
		baseline:  "Every active services host has at least one locally reachable Mimir child whose rendered configuration contains exactly one Boolean blocks_storage.tsdb.flush_blocks_on_shutdown value; a rollout may temporarily expose two generations.",
		observed:  "mimir_instances=0",
		context:   "This is host-local observation loss, not proof that every replicated Mimir process is down. The probe emits only the selected Boolean and never returns the full rendered configuration because it can contain credentials.",
		action:    "Inspect the host's Grafana unit, parent status, and bounded child journal. Restore the Mimir child or its loopback configuration endpoint before claiming shutdown durability from a sibling replica.",
		verify:    "The host exposes at least one Mimir configuration with flush_blocks_on_shutdown=true for two consecutive probes.",
		playbook:  "SIGNALS.md §11.21 and §11.2",
	}
}

func mimirShutdownFlushDisabledFinding(hostCount, observableHosts, instanceCount int, disabled []string) finding {
	return finding{
		probeId: "observability/mimir-shutdown", tier: tierWarn,
		class: "mimir-shutdown-flush-disabled", target: "mimir-fleet", sustain: 1,
		symptom: fmt.Sprintf(
			"%d of %d directly observed Mimir process(es) will not flush their partial TSDB head on clean shutdown",
			len(disabled), instanceCount,
		),
		mechanism: "Mimir has not yet uploaded its current incomplete TSDB head to object storage. With flush_blocks_on_shutdown=false and the Grafana data directory intentionally ephemeral, removing a cleanly stopped container discards that unshipped head instead of reusing it, producing bounded holes in otherwise independent metric series.",
		baseline:  "Every active Mimir process on every enabled services host renders blocks_storage.tsdb.flush_blocks_on_shutdown: true and retains the Grafana parent's 120-second Mimir child stop allowance inside Warpctl's 3,600-second container drain.",
		observed: fmt.Sprintf(
			"configured_hosts=%d observable_hosts=%d mimir_instances=%d disabled_instances=%d details=%s",
			hostCount, observableHosts, instanceCount, len(disabled), strings.Join(disabled, ";"),
		),
		evidence: "Each value comes from the exact process's loopback /config endpoint. The remote filter emits only the selected Boolean; rendered credentials and unrelated configuration never leave the host.",
		context:  "This is a software deployment gap, not a Grafana panel-query defect and not a hardware-capacity alert. Historical Mimir gaps are unrecoverable and clear only when they age out of the dashboard window; this setting prevents new clean-restart loss.",
		action:   "Build and deploy Grafana from an intentional local Warp checkout containing commit 7176ccd, after §8.13 can read the exact Warpctl identity. Keep each generation's TSDB directory private and ephemeral, retain the 120-second Mimir child stop allowance inside Warpctl's 3,600-second container drain, and do not zero-fill the dashboard or shared-mount one TSDB directory into overlapping containers. The generated unit's separate 60-second timeout stops only the Warpctl controller and does not truncate a normal container drain. The first rollout still begins with old children configured false; explicitly flushing those old ingesters is an operator-controlled production mutation if preserving their current partial heads is required.",
		verify:   "Require every exact loopback Mimir config to report flush_blocks_on_shutdown=true, then perform a controlled Grafana rollout followed by the full rollout. SIGNALS.md §11.20 must show no new bounded build-info gap through the next restart and block-upload window; old gaps can only age out.",
		playbook: "SIGNALS.md §11.21, §11.20, and §8.13",
	}
}

func parseMimirShutdownHostSample(output string) (mimirShutdownHostSample, error) {
	sample := mimirShutdownHostSample{}
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
				return sample, fmt.Errorf("mimir shutdown line %d: invalid instance_begin", lineNumber+1)
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil || port < 1 || port > 65535 {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid port %q", lineNumber+1, fields[1])
			}
			sample.instances = append(sample.instances, mimirShutdownInstance{port: port})
			current = len(sample.instances) - 1
		case "flush":
			if len(fields) != 2 || current < 0 || sample.instances[current].flushSeen {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid flush", lineNumber+1)
			}
			value, err := strconv.ParseBool(fields[1])
			if err != nil {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid flush %q", lineNumber+1, fields[1])
			}
			sample.instances[current].flush = value
			sample.instances[current].flushSeen = true
		case "instance_end":
			if len(fields) != 1 || current < 0 {
				return sample, fmt.Errorf("mimir shutdown line %d: unexpected instance_end", lineNumber+1)
			}
			if !sample.instances[current].flushSeen {
				return sample, fmt.Errorf("mimir shutdown line %d: instance omitted flush", lineNumber+1)
			}
			current = -1
		case "mimir_count":
			if len(fields) != 2 || current >= 0 || sample.countSeen {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid mimir_count", lineNumber+1)
			}
			count, err := strconv.Atoi(fields[1])
			if err != nil || count < 0 {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid mimir_count %q", lineNumber+1, fields[1])
			}
			sample.count = count
			sample.countSeen = true
		default:
			return sample, fmt.Errorf("mimir shutdown line %d: unknown field %q", lineNumber+1, fields[0])
		}
	}
	if current >= 0 {
		return sample, fmt.Errorf("mimir shutdown: unterminated instance")
	}
	if !sample.countSeen {
		return sample, fmt.Errorf("mimir shutdown: missing mimir_count")
	}
	if sample.count != len(sample.instances) {
		return sample, fmt.Errorf("mimir shutdown: count=%d instances=%d", sample.count, len(sample.instances))
	}
	return sample, nil
}

const mimirShutdownScript = `set -u
for required in ss curl awk sort; do
  if ! command -v "$required" >/dev/null 2>&1; then
    printf 'mimir shutdown probe prerequisite missing: %s\n' "$required" >&2
    exit 1
  fi
done
mimir_count=0
ports=$(ss -ltnH 2>/dev/null | awk '$4 ~ /^127[.]0[.]0[.]1:[0-9]+$/ {sub(/.*:/, "", $4); print $4}' | sort -n -u)
for port in $ports; do
  flush=$(curl -fsS --max-time 10 "http://127.0.0.1:${port}/config" 2>/dev/null | awk '
    $1 == "flush_blocks_on_shutdown:" && ($2 == "true" || $2 == "false") {print $2}
  ' || true)
  case "$flush" in
    true|false) ;;
    *) continue ;;
  esac

  mimir_count=$((mimir_count+1))
  printf 'instance_begin %s\n' "$port"
  printf 'flush %s\n' "$flush"
  printf 'instance_end\n'
done
printf 'mimir_count %s\n' "$mimir_count"
`
