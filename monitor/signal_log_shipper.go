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

// SIGNALS.md §11.14 maps to signal_log_shipper.go and
// signal_log_shipper_test.go. Fluent Bit is a host unit, so container-only
// health checks cannot observe its permanent-failure state or fd budget.
func NewLogShipperSignal() Signal {
	return &signalAdapter{
		number: "11.14", key: "log-shipper", name: "Host log and metric shipper",
		probe: logShipperProbe{},
	}
}

type logShipperProbe struct{}

func (logShipperProbe) id() string             { return "observability/log-shipper" }
func (logShipperProbe) tier() string           { return tierPage }
func (logShipperProbe) cadence() time.Duration { return time.Minute }

const logShipperMarker = "monitor-signal-11.14-log-shipper"

const logShipperCommand = `# ` + logShipperMarker + `
set -u
properties=$(systemctl show fluent-bit.service \
  -p ActiveState -p SubState -p Result -p NRestarts \
  -p LimitNOFILE -p LimitNOFILESoft --no-pager 2>/dev/null) || exit 41
read_property() {
  printf '%s\n' "$properties" | awk -F= -v key="$1" '$1 == key {print substr($0, index($0, "=")+1); found=1} END {exit !found}'
}
printf '%s\n' \
  'observation_schema=1' \
  "active_state=$(read_property ActiveState)" \
  "sub_state=$(read_property SubState)" \
  "result=$(read_property Result)" \
  "restarts=$(read_property NRestarts)" \
  "nofile_hard=$(read_property LimitNOFILE)" \
  "nofile_soft=$(read_property LimitNOFILESoft)"
`

type logShipperSample struct {
	activeState string
	subState    string
	result      string
	restarts    int
	nofileHard  uint64
	nofileSoft  uint64
}

type logShipperResult struct {
	host   *host
	sample logShipperSample
	err    error
}

func logShipperHosts(cfg *monitorConfig) []*host {
	roles := []string{"services", "pg-primary", "redis-cluster", "subtensor", "backup"}
	hosts := []*host{}
	for _, target := range cfg.hosts {
		for _, role := range roles {
			if target.hasRole(role) {
				hosts = append(hosts, target)
				break
			}
		}
	}
	return hosts
}

func (logShipperProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := logShipperHosts(env.cfg)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("log shipper: no managed hosts in inventory")
	}
	results := make(chan logShipperResult, len(hosts))
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
				results <- logShipperResult{host: target, err: ctx.Err()}
				return
			}
			output, err := env.runner.shell(ctx, target, logShipperCommand)
			if err != nil {
				results <- logShipperResult{host: target, err: err}
				return
			}
			sample, err := parseLogShipperSample(output)
			results <- logShipperResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]logShipperResult, 0, len(hosts))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })

	findings := make([]finding, 0, len(ordered)*3)
	for _, result := range ordered {
		target := result.host.name
		if result.err != nil {
			findings = append(findings, cannotObserveFinding(target+"/log-shipper", result.err))
			continue
		}
		findings = append(findings, evaluateLogShipper(target, result.sample)...)
	}
	return findings, nil
}

func parseLogShipperSample(raw string) (logShipperSample, error) {
	required := []string{
		"observation_schema", "active_state", "sub_state", "result", "restarts",
		"nofile_hard", "nofile_soft",
	}
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
	}
	values := map[string]string{}
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !allowed[key] {
			return logShipperSample{}, fmt.Errorf("log shipper: malformed or unexpected observation field")
		}
		if _, exists := values[key]; exists {
			return logShipperSample{}, fmt.Errorf("log shipper: duplicate %s field", key)
		}
		values[key] = strings.TrimSpace(value)
	}
	for _, key := range required {
		if values[key] == "" {
			return logShipperSample{}, fmt.Errorf("log shipper: observation omitted %s", key)
		}
	}
	if values["observation_schema"] != "1" {
		return logShipperSample{}, fmt.Errorf("log shipper: unsupported observation schema")
	}
	restarts, err := strconv.Atoi(values["restarts"])
	if err != nil || restarts < 0 {
		return logShipperSample{}, fmt.Errorf("log shipper: invalid restarts")
	}
	hard, err := strconv.ParseUint(values["nofile_hard"], 10, 64)
	if err != nil {
		return logShipperSample{}, fmt.Errorf("log shipper: invalid nofile_hard")
	}
	soft, err := strconv.ParseUint(values["nofile_soft"], 10, 64)
	if err != nil {
		return logShipperSample{}, fmt.Errorf("log shipper: invalid nofile_soft")
	}
	if soft > hard {
		return logShipperSample{}, fmt.Errorf("log shipper: soft fd limit exceeds hard limit")
	}
	return logShipperSample{
		activeState: values["active_state"], subState: values["sub_state"],
		result: values["result"], restarts: restarts, nofileHard: hard, nofileSoft: soft,
	}, nil
}

func evaluateLogShipper(target string, sample logShipperSample) []finding {
	observed := fmt.Sprintf(
		"active_state=%s sub_state=%s result=%s restarts=%d nofile_soft=%d nofile_hard=%d",
		sample.activeState, sample.subState, sample.result, sample.restarts,
		sample.nofileSoft, sample.nofileHard,
	)
	findings := []finding{}
	running := sample.activeState == "active" && sample.subState == "running"
	if !running {
		findings = append(findings, finding{
			probeId: "observability/log-shipper", tier: tierPage,
			class: "log-shipper-down", target: target, sustain: 1,
			symptom:   fmt.Sprintf("%s is not shipping host logs and metrics", target),
			mechanism: "The host-managed fluent-bit unit is not active/running. Warp containers can remain healthy while this independent unit permanently stops, removing that host from Loki and Mimir.",
			baseline:  "fluent-bit.service is active/running on every managed Warp, database, Redis, backup, and Subtensor host.", observed: observed,
			evidence: fmt.Sprintf("service=%s/%s result=%s", sample.activeState, sample.subState, sample.result),
			context:  "This is affirmative shipper-process loss. It does not identify whether the original trigger was configuration, fd exhaustion, credentials, or an output failure.",
			action:   "Inspect the bounded fluent-bit journal and effective unit limits, fix the first startup/output failure, then restart only fluent-bit. Do not reboot the host or infer workload failure from missing telemetry.",
			verify:   "Require active/running state, the expected fd budget, fresh per-host Mimir metrics, and a fresh labeled Warp record in Loki.",
			playbook: "SIGNALS.md §11.14",
		})
	} else {
		findings = append(findings, healthyFinding("observability/log-shipper", tierPage, "log-shipper-down", target))
	}

	if sample.nofileSoft < 65536 || sample.nofileHard < 65536 {
		findings = append(findings, finding{
			probeId: "observability/log-shipper", tier: tierWarn,
			class: "log-shipper-fd-budget", target: target, sustain: 2,
			symptom:   fmt.Sprintf("%s Fluent Bit fd budget can fail as configured collectors grow", target),
			mechanism: "Fluent Bit allocates descriptors per collector timer and output worker. The historical 1024 soft limit exhausted at startup even though the hard limit looked large.",
			baseline:  "Both LimitNOFILESoft and LimitNOFILE are at least 65536.", observed: observed,
			evidence: fmt.Sprintf("nofile_soft=%d nofile_hard=%d", sample.nofileSoft, sample.nofileHard),
			context:  "A currently running unit can still fail on its next configuration-driven restart if the startup descriptor budget is too small.",
			action:   "Apply the shared Fluent Bit systemd override and restart only the shipper after validating its rendered inputs.",
			verify:   "Read both effective limits, require at least 65536, and confirm fresh Mimir and Loki data after one controlled shipper restart.",
			playbook: "SIGNALS.md §11.14",
		})
	} else {
		findings = append(findings, healthyFinding("observability/log-shipper", tierWarn, "log-shipper-fd-budget", target))
	}

	if running && sample.restarts > 0 {
		findings = append(findings, finding{
			probeId: "observability/log-shipper", tier: tierWarn,
			class: "log-shipper-churn", target: target, sustain: 2,
			symptom:   fmt.Sprintf("%s Fluent Bit has restarted within its current unit activation", target),
			mechanism: "systemd recorded one or more automatic restarts. The retry policy avoids permanent failure, but repeated starts can create telemetry gaps and usually preserve an actionable first error in the unit journal.",
			baseline:  "NRestarts remains zero during steady state.", observed: observed,
			evidence: fmt.Sprintf("restarts=%d result=%s", sample.restarts, sample.result),
			context:  "This is process churn, not proof of missing downstream data; verify Mimir and Loki freshness independently.",
			action:   "Inspect the first bounded error before the restart and repair that cause. Do not clear the counter or reboot merely to hide the evidence.",
			verify:   "Require a stable process and fresh per-host data in both outputs for ten minutes.",
			playbook: "SIGNALS.md §11.14",
		})
	} else {
		findings = append(findings, healthyFinding("observability/log-shipper", tierWarn, "log-shipper-churn", target))
	}
	return findings
}
