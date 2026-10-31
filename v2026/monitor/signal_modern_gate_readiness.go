package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// SIGNALS.md §2.29 maps to signal_modern_gate_readiness.go and
// signal_modern_gate_readiness_test.go. Modern-first priority within each
// FindProviders2 band replaced the modern provider gate (owner decision
// 2026-10-28, FP2-MODERN.md): the gate no longer turns on by itself, so there
// is no start to get ready for, and only
// quality_speed_requires_modern_provider: true in provider.yml turns it on,
// taking every provider the capability probe has not shown to be modern out
// of Quality and Speed. Each taskworker decides the gate from its own code and
// config, so this signal watches that an explicit switch is decided alike
// across the fleet: settled taskworkers that decide the gate differently
// WARN, judged on the processes that have run for five minutes, so a restart
// alone does not fire. A switch in the deployment's desired provider.yml that
// cannot be read holds the gate off, though it was most likely meant, and
// WARNs on its own.
func NewModernGateReadinessSignal() Signal {
	return newModernGateReadinessSignal(model.GetModernProviderGateFailure)
}

// gateFailure reads why the deployment's desired provider.yml holds the gate
// off by a failure, "" for none.
func newModernGateReadinessSignal(gateFailure func() string) Signal {
	return &signalAdapter{number: "2.29", key: "modern-gate-readiness", name: "Modern provider gate readiness", probe: modernGateReadinessProbe{gateFailure: gateFailure}}
}

const (
	// how long a taskworker process runs before the fleet's agreement is
	// judged on it: its stats collector registers the gate at its first
	// refresh, a minute or two after the start, and a drained process's
	// series stay readable for Mimir's five-minute lookback
	modernGateReadinessSettleAge = 5 * time.Minute
	// the largest Mimir answer read
	modernGateReadinessMaxResponseBytes = 64 * 1024
	modernGateReadinessProbeId          = "mimir/modern-gate-readiness"
	modernGateReadinessTarget           = "modern-provider-gate"
)

type modernGateReadinessProbe struct {
	// why the desired provider.yml holds the gate off by a failure, "" for
	// none
	gateFailure func() string
}

func (modernGateReadinessProbe) id() string             { return modernGateReadinessProbeId }
func (modernGateReadinessProbe) tier() string           { return tierWarn }
func (modernGateReadinessProbe) cadence() time.Duration { return 5 * time.Minute }

// What the signal reads from Mimir at one instant.
type modernGateReadinessObservation struct {
	// running taskworkers that publish the gate
	// (urnetwork_stats_provider_modern_gate_on), and those by their process
	// series, zero when Mimir has none
	publishing  int
	taskworkers int
	// the taskworkers that have run for modernGateReadinessSettleAge, on
	// which the fleet's agreement is judged
	settled modernGateReadinessFleet
}

// Taskworker processes and what they publish.
type modernGateReadinessFleet struct {
	// processes, those of them that publish the gate, and whether it holds
	// Quality and Speed on every one of those (min) and on any (max)
	taskworkers int
	publishing  int
	onEvery     bool
	onAny       bool
}

// One instant query's parts, each a fixed label value, so a single round
// trip reads the fleet's agreement on the gate. The gate is read per
// taskworker process against the processes: count, and min against max, so
// a taskworker without it, or on an older config, is seen. The agreement is
// read on the processes that started modernGateReadinessSettleAge ago, by
// their process series' instance, a nonce of each process.
func modernGateReadinessQuery(environment string) string {
	selector := "env=" + strconv.Quote(environment) + `,job="taskworker"`
	part := func(expression string, name string) string {
		return "label_replace(" + expression + `,"monitor_modern_gate_part",` + strconv.Quote(name) + `,"","")`
	}
	settledProcesses := "process_start_time_seconds{" + selector + "} < time() - " + strconv.Itoa(int(modernGateReadinessSettleAge/time.Second))
	settledGate := "urnetwork_stats_provider_modern_gate_on{" + selector + "} and on(instance) (" + settledProcesses + ")"
	return strings.Join([]string{
		part("count(urnetwork_stats_provider_modern_gate_on{"+selector+"})", "on_count"),
		part("count(process_start_time_seconds{"+selector+"})", "taskworkers"),
		part("count("+settledProcesses+")", "settled_taskworkers"),
		part("count("+settledGate+")", "settled_on_count"),
		part("min("+settledGate+")", "settled_on_min"),
		part("max("+settledGate+")", "settled_on_max"),
	}, " or ")
}

func (self modernGateReadinessProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	findings, err := self.observe(ctx, env)
	// A switch that cannot be read in the desired provider.yml holds the gate
	// off, which the fleet then agrees on without a word. It is read apart
	// from Mimir, which need not answer for it.
	failure := self.gateFailure()
	if failure == "" {
		return findings, err
	}
	unreadable := modernGateScheduleUnreadableFinding(failure)
	if err != nil {
		return []finding{unreadable, cannotObserveFinding(modernGateReadinessTarget, err)}, nil
	}
	return append(findings, unreadable), nil
}

// The verdict on what the taskworkers publish to Mimir. A read that fails is
// an error, never quiet: the fleet's agreement is then unknown, and the run
// is no evidence that a disagreement has cleared.
func (modernGateReadinessProbe) observe(ctx context.Context, env *probeEnv) ([]finding, error) {
	now := env.now()
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("modern gate readiness: no services host is configured for the loopback Mimir query")
	}
	command := "curl -fsS --max-time 15 --max-filesize " + strconv.Itoa(modernGateReadinessMaxResponseBytes) +
		" --data-urlencode " + shellSingleQuote("query="+modernGateReadinessQuery(env.cfg.env)) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(now.Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, _, err := mimirShellFirstServiceGatewayParsed(ctx, env.runner, metricHosts, nil, command, modernGateReadinessMaxResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("modern gate readiness: query Mimir through service gateways: %w", err)
	}
	parts, err := parseModernGateReadinessResponse(out, now)
	if err != nil {
		return nil, err
	}
	observation, err := newModernGateReadinessObservation(parts)
	if err != nil {
		return nil, err
	}
	return evaluateModernGateReadiness(observation), nil
}

// One series of a response: its labels and value.
type modernGateReadinessSeries struct {
	labels map[string]string
	value  float64
}

// The response's series by part. Fixed error text only: response labels and
// bodies never become findings.
func parseModernGateReadinessResponse(raw string, at time.Time) (map[string][]modernGateReadinessSeries, error) {
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
	}
	if modernGateReadinessMaxResponseBytes < len(raw) || json.Unmarshal([]byte(raw), &response) != nil {
		return nil, fmt.Errorf("modern gate readiness: undecodable Mimir response")
	}
	if response.Status != "success" || response.Data.ResultType != "vector" || 0 < len(response.Warnings) {
		return nil, fmt.Errorf("modern gate readiness: Mimir status=%q result_type=%q warnings=%d", response.Status, response.Data.ResultType, len(response.Warnings))
	}
	parts := map[string][]modernGateReadinessSeries{}
	for _, series := range response.Data.Result {
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil || observedAt.Unix() != at.Unix() || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("modern gate readiness: invalid sample")
		}
		part := series.Metric["monitor_modern_gate_part"]
		parts[part] = append(parts[part], modernGateReadinessSeries{labels: series.Metric, value: value})
	}
	return parts, nil
}

// The observation the parts give. A part present more than once, or one the
// query does not ask for, is an invalid response; a part absent is an
// aggregate over no series, as in Mimir, and reads as zero.
func newModernGateReadinessObservation(parts map[string][]modernGateReadinessSeries) (modernGateReadinessObservation, error) {
	observation := modernGateReadinessObservation{}
	for part, series := range parts {
		if len(series) != 1 {
			return modernGateReadinessObservation{}, fmt.Errorf("modern gate readiness: invalid %s series", part)
		}
		value := series[0].value
		switch part {
		case "on_count":
			observation.publishing = int(value)
		case "taskworkers":
			observation.taskworkers = int(value)
		case "settled_taskworkers":
			observation.settled.taskworkers = int(value)
		case "settled_on_count":
			observation.settled.publishing = int(value)
		case "settled_on_min":
			observation.settled.onEvery = 1 <= value
		case "settled_on_max":
			observation.settled.onAny = 1 <= value
		default:
			return modernGateReadinessObservation{}, fmt.Errorf("modern gate readiness: invalid unknown series")
		}
	}
	return observation, nil
}

// The signal's verdict, a pure function of the observation and of no clock:
// the gate has no start any more. The settled taskworkers decide the gate
// alike when its min equals its max and, while it is on anywhere, every one
// publishes it, since one that does not decides it off. With the gate off
// everywhere, one without it agrees, as code from before the gate does.
func evaluateModernGateReadiness(observation modernGateReadinessObservation) []finding {
	settled := observation.settled
	if settled.onEvery != settled.onAny || settled.onAny && settled.publishing < settled.taskworkers {
		return []finding{modernGateFleetDisagreesFinding(observation)}
	}
	return []finding{healthyFinding(modernGateReadinessProbeId, tierWarn, "modern-gate-fleet-disagrees", modernGateReadinessTarget)}
}

// What the provider.yml switch does, the action every finding names.
const modernGateReadinessOverrides = "The gate is on only while provider.yml sets `quality_speed_requires_modern_provider: true`; `quality_speed_requires_modern_provider: false`, or no switch at all, keeps it off, and the next score export restores the buckets (modern-first priority replaced the gate, FP2-MODERN.md). A change is a config release through the config-updater and a restart of every taskworker and API block, since a running container keeps the config it was created with (capprobe/README.md, \"Switching it on\", has the same procedure). `quality_speed_modern_provider_start` is retired and ignored. Confirm the switch with `bringyourctl provider inspect` after the restart"

func modernGateFleetDisagreesFinding(observation modernGateReadinessObservation) finding {
	settled := observation.settled
	onIn := "none of them"
	switch {
	case settled.onEvery:
		onIn = "all of them"
	case settled.onAny:
		onIn = "some of them"
	}
	boolValue := func(value bool) int {
		if value {
			return 1
		}
		return 0
	}
	return finding{
		probeId: modernGateReadinessProbeId, tier: tierWarn, class: "modern-gate-fleet-disagrees",
		target: modernGateReadinessTarget, sustain: 2,
		symptom:   fmt.Sprintf("The taskworkers disagree on the modern provider gate: of %d that have run for five minutes, %d publish the gate, and it is on in %s", settled.taskworkers, settled.publishing, onIn),
		mechanism: "Each taskworker decides the gate from its own code and config, and only quality_speed_requires_modern_provider: true turns it on. One created before a config release that set or cleared the switch decides the old way, and one still running code from before the gate was retired turns the gate on by itself at its old start while the switch is absent. The score export a taskworker runs publishes the buckets FindProviders2 answers from, so where they disagree Quality and Speed follow whichever taskworker ran the last pass, and the dashboard counts the taskworker that filled the last snapshot.",
		baseline:  "Every taskworker that has run for five minutes decides the gate alike: the min of urnetwork_stats_provider_modern_gate_on over them equals its max, and while the gate is on anywhere every one publishes it, since one without it decides it off. A taskworker without the gate beside a gate off everywhere agrees. WARN otherwise.",
		observed:  fmt.Sprintf("settled_taskworkers=%d settled_publishing=%d gate_on_min=%d gate_on_max=%d running_taskworkers=%d running_publishing=%d", settled.taskworkers, settled.publishing, boolValue(settled.onEvery), boolValue(settled.onAny), observation.taskworkers, observation.publishing),
		evidence:  "count, min and max of urnetwork_stats_provider_modern_gate_on over the taskworker processes whose process_start_time_seconds is five minutes old, matched by instance, against the count of those processes; no host or process identity leaves Mimir.",
		context:   "Only processes that have run for five minutes are compared: a new process registers the gate at its stats collector's first refresh, a minute or two after it starts, and a drained process's series stay readable for five minutes, so a restart alone does not fire. A config release that sets or clears the switch makes the fleet disagree until its last taskworker has restarted and the drained processes' series have gone stale. API, alt and proxy processes decide the gate for their own readers but publish no gauge, so this does not see them.",
		action:    "Find the taskworkers whose urnetwork_stats_provider_modern_gate_on is missing or differs (by host and block), confirm each runs the current build and its host publishes the current config (WARP_CONFIG_VERSION), and restart or redeploy those that differ. " + modernGateReadinessOverrides + ".",
		verify:    "Over the taskworkers that have run for five minutes, the count of urnetwork_stats_provider_modern_gate_on equals theirs and its min equals its max, on two consecutive reads.",
		playbook:  "SIGNALS.md §2.29, FP2FIX.md (modern provider gate) and FP2-MODERN.md",
	}
}

// failure is the decoder's fixed text, never a value from the file.
func modernGateScheduleUnreadableFinding(failure string) finding {
	return finding{
		probeId: modernGateReadinessProbeId, tier: tierWarn, class: "modern-gate-schedule-unreadable",
		target: modernGateReadinessTarget, frame: "provider.yml", sustain: 1,
		symptom:   "The deployment's desired provider.yml holds the modern provider gate off by a failure: " + failure,
		mechanism: "A quality_speed_requires_modern_provider that cannot be read, is given twice or has no value reads as false, and so does a provider.yml that cannot be read. The gate is then off, as with no switch at all, though a switch was most likely meant: a true that failed leaves the gate off, and the fleet agrees on that without a word, which only the taskworkers' logs said before.",
		baseline:  "provider.yml sets quality_speed_requires_modern_provider true or false, or leaves it out, which is off. The retired quality_speed_modern_provider_start is ignored and is never a failure.",
		observed:  "failure=" + failure,
		evidence:  "The monitor's own decode of the deployment's desired provider.yml (model.GetModernProviderGateFailure), the decoder every taskworker and API block runs on the config it was created with.",
		context:   "The taskworkers decide by the config of their last restart, so until the release reaches them they may still decide by the earlier switch; the fleet's own gauges show which.",
		action:    "Correct the key in provider.yml: `quality_speed_requires_modern_provider: false` keeps the gate off on purpose, and `true` turns it on. Release the config through the config-updater and restart every taskworker and API block (capprobe/README.md, \"Switching it on\").",
		verify:    "The finding clears on the next run after the release, and once the taskworkers have restarted urnetwork_stats_provider_modern_gate_on shows the meant switch on every one.",
		playbook:  "SIGNALS.md §2.29, FP2FIX.md (modern provider gate) and FP2-MODERN.md",
	}
}
