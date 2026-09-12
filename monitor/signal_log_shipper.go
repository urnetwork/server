package monitor

import (
	"context"
	"fmt"
	"regexp"
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
restarts=$(read_property NRestarts)
fluent_bit_version=unknown
if command -v dpkg-query >/dev/null 2>&1; then
  candidate_version=$(dpkg-query --show '--showformat=${Version}' fluent-bit 2>/dev/null || true)
  case "$candidate_version" in
    ''|*[!0-9A-Za-z.+:~_-]*) ;;
    *) fluent_bit_version=$candidate_version ;;
  esac
fi
restart_reason=none
if [ "$restarts" -gt 0 ]; then
  restart_reason=other-or-unobservable
  coredump=$(journalctl -b -n 400 --no-pager --quiet -o cat \
    COREDUMP_COMM=fluent-bit COREDUMP_SIGNAL=11 2>/dev/null || true)
  if printf '%s\n' "$coredump" | grep -Fq 'add_metric_histogram' && \
     printf '%s\n' "$coredump" | grep -Fq 'finish_duplicate_histogram_summary_sum_count' && \
     printf '%s\n' "$coredump" | grep -Fq 'parse_histogram_summary_name'; then
    restart_reason=prometheus-histogram-decoder-crash
  fi
fi
printf '%s\n' \
  'observation_schema=2' \
  "active_state=$(read_property ActiveState)" \
  "sub_state=$(read_property SubState)" \
  "result=$(read_property Result)" \
  "restarts=$restarts" \
  "nofile_hard=$(read_property LimitNOFILE)" \
  "nofile_soft=$(read_property LimitNOFILESoft)" \
  "fluent_bit_version=$fluent_bit_version" \
  "restart_reason=$restart_reason"
`

const (
	logShipperRestartNone             = "none"
	logShipperRestartOther            = "other-or-unobservable"
	logShipperRestartHistogramDecoder = "prometheus-histogram-decoder-crash"
)

var logShipperVersionRe = regexp.MustCompile(`^(?:unknown|[0-9A-Za-z.+:~_-]{1,64})$`)

type logShipperSample struct {
	activeState   string
	subState      string
	result        string
	restarts      int
	nofileHard    uint64
	nofileSoft    uint64
	version       string
	restartReason string
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
		findings = append(findings, evaluateLogShipper(target, result.sample, result.host.hasRole("redis-cluster"))...)
	}
	return findings, nil
}

func parseLogShipperSample(raw string) (logShipperSample, error) {
	required := []string{
		"observation_schema", "active_state", "sub_state", "result", "restarts",
		"nofile_hard", "nofile_soft", "fluent_bit_version", "restart_reason",
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
	if values["observation_schema"] != "2" {
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
	if !logShipperVersionRe.MatchString(values["fluent_bit_version"]) {
		return logShipperSample{}, fmt.Errorf("log shipper: invalid fluent_bit_version")
	}
	restartReason := values["restart_reason"]
	if restartReason != logShipperRestartNone &&
		restartReason != logShipperRestartOther &&
		restartReason != logShipperRestartHistogramDecoder {
		return logShipperSample{}, fmt.Errorf("log shipper: invalid restart_reason")
	}
	if (restarts == 0) != (restartReason == logShipperRestartNone) {
		return logShipperSample{}, fmt.Errorf("log shipper: restart reason does not match restart count")
	}
	return logShipperSample{
		activeState: values["active_state"], subState: values["sub_state"],
		result: values["result"], restarts: restarts, nofileHard: hard, nofileSoft: soft,
		version: values["fluent_bit_version"], restartReason: restartReason,
	}, nil
}

func evaluateLogShipper(target string, sample logShipperSample, redisClusterHost bool) []finding {
	observed := fmt.Sprintf(
		"active_state=%s sub_state=%s result=%s restarts=%d nofile_soft=%d nofile_hard=%d fluent_bit_version=%s restart_reason=%s",
		sample.activeState, sample.subState, sample.result, sample.restarts,
		sample.nofileSoft, sample.nofileHard, sample.version, sample.restartReason,
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

	if sample.restartReason == logShipperRestartHistogramDecoder {
		mechanism := "The current-boot core record contains the exact Fluent Bit/cmetrics duplicate-histogram parsing stack. The stack proves a Prometheus histogram decoder crash but does not name the offending scrape source, metric family, or target."
		contextText := "This is an exact shipper crash cause, not proof that the scraped service, Mimir, or an application failed. A recovered unit can be active while the restart counter and bounded core metadata preserve the event."
		action := "Inventory the bounded Prometheus scrape inputs on this host and reproduce the schema of candidate histogram families. Exclude or correct only the proven producer-side family; do not raise Mimir limits or force a Fluent Bit major upgrade as the first correction."
		verify := "After the authorized producer-side correction, require the offending histogram absent or schema-compatible, required source metrics fresh, a stable Fluent Bit process, and fresh host data in both Mimir and Loki for ten minutes."
		if redisClusterHost {
			mechanism += " On a Redis-cluster host, the optional command latency histogram is the strongest bounded candidate because its command-dependent bucket layouts exercise this path; ordinary commandstats counters do not require it."
			contextText += " The stack alone is not metric-family attribution; confirm the Redis exporter unit and family before applying that host-specific fix."
			action = "Confirm the Redis exporter still exposes the optional command latency histogram, then deploy the unit that excludes only that family while retaining commandstats rate/duration counters. Do not raise Mimir limits or force a Fluent Bit major upgrade as the first correction."
			verify = "After the authorized Redis exporter rollout, require the optional latency histogram family absent, required commandstats metrics fresh, a stable Fluent Bit process, and fresh Redis data in both Mimir and Loki for ten minutes."
		}
		findings = append(findings, finding{
			probeId: "observability/log-shipper", tier: tierWarn,
			class: "log-shipper-prometheus-histogram-decoder-crash", target: target, sustain: 1,
			symptom:   fmt.Sprintf("%s Fluent Bit crashed while decoding a Prometheus histogram", target),
			mechanism: mechanism,
			baseline:  "No current-boot Fluent Bit restart has the Prometheus histogram decoder crash stack.", observed: observed,
			evidence: fmt.Sprintf("restarts=%d fluent_bit_version=%s restart_reason=%s", sample.restarts, sample.version, sample.restartReason),
			context:  contextText,
			action:   action,
			verify:   verify,
			playbook: "SIGNALS.md §11.14",
		})
	} else {
		findings = append(findings, healthyFinding("observability/log-shipper", tierWarn, "log-shipper-prometheus-histogram-decoder-crash", target))
	}

	if running && sample.restarts > 0 && sample.restartReason != logShipperRestartHistogramDecoder {
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
