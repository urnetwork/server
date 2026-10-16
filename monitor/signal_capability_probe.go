package monitor

// The capability probe (server/capprobe) is the only writer of the modern
// provider gate's evidence. While capability_probe.yml switches it on, its
// chain of passes must keep running: a taskworker that runs a pass with
// settings that switch the probe off ends the chain, and a chain that
// finishes no pass has stopped. Both read the probe's per-taskworker series
// in Mimir against the desired config; no provider, task or client identity
// leaves either.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	capabilityProbeProbeId = "taskworker/capability-probe"
	capabilityProbeTarget  = "CapabilityProbe"
	// capprobe.SettingsResourceName; the monitor reads only its switch
	capabilityProbeSettingsResource = "capability_probe.yml"
	// A live chain finishes a pass at least every fifteen minutes: a pass
	// runs at most max_time_seconds (900), and a waiting pass runs every ten
	// minutes between sweeps. No pass for this long is a stall.
	capabilityProbeStallAfter = 30 * time.Minute
	// a stall this long pages while the gate is on, whose evidence then ages
	// toward stale_evidence. Evidence stays fresh for seven days after its
	// measurement (model.ProviderModernEvidenceMaxAge), so a shorter page
	// would fire through planned database maintenance with nothing at risk
	capabilityProbeStallPageAfter = 6 * time.Hour
	// how far back a finished pass is found, in processes that have exited
	// since included
	capabilityProbeLastPassLookback = 6 * time.Hour
	// the window disabled passes are counted over, in processes that have
	// exited since included
	capabilityProbeDisabledWindow = 30 * time.Minute
	// how far the query's evaluation time may be from the monitor's clock
	capabilityProbeEvaluationSkew = 5 * time.Minute
	capabilityProbeResponseMax    = 64 * 1024
)

// SIGNALS.md §2.19g maps to signal_capability_probe.go and
// signal_capability_probe_test.go.
func NewCapabilityProbeSignal() Signal {
	return newCapabilityProbeSignal(loadCapabilityProbeDesired)
}

func newCapabilityProbeSignal(loadDesired func() (capabilityProbeDesired, error)) Signal {
	return &signalAdapter{
		number: "2.19g", key: "capability-probe", name: "Capability probe chain liveness",
		probe: capabilityProbeProbe{loadDesired: loadDesired},
	}
}

type capabilityProbeProbe struct {
	// The seam keeps synthetic observations independent of the
	// workstation's active Config resources. Production reloads them each
	// cadence.
	loadDesired func() (capabilityProbeDesired, error)
}

func (capabilityProbeProbe) id() string             { return capabilityProbeProbeId }
func (capabilityProbeProbe) tier() string           { return tierWarn }
func (capabilityProbeProbe) cadence() time.Duration { return 5 * time.Minute }

// What the deployment's config asks of the probe.
type capabilityProbeDesired struct {
	// capability_probe.yml switches the probe on; absent, it ships off
	enabled bool
	// provider.yml's modern provider gate as every pass decodes it, nil for
	// off. It is on at an instant while its switch is true, or while the
	// switch is absent from its start on (RequiresModernProvider), so it
	// turns on by itself at the start; an unreadable switch is off.
	gate *model.EgressIndexSettings
}

// Whether the desired gate holds Quality and Speed at now.
func (self capabilityProbeDesired) gateOn(now time.Time) bool {
	return self.gate != nil && self.gate.RequiresModernProvider(now)
}

// Reads capability_probe.yml's switch as capprobe.LoadSettings does: an
// absent file or key is the default, off. The rest of the file is the
// taskworker's to validate; an invalid file that switches the probe on
// fails its passes, which the stall shows. provider.yml's modern provider
// gate is decoded by the model's own reader, so the switch and its start
// read here as every pass reads them.
func loadCapabilityProbeDesired() (capabilityProbeDesired, error) {
	desired := capabilityProbeDesired{}
	resource, err := server.Config.SimpleResource(capabilityProbeSettingsResource)
	switch {
	case errors.Is(err, server.ErrResourceNotFound):
	case err != nil:
		return desired, fmt.Errorf("%s is unavailable", capabilityProbeSettingsResource)
	default:
		var raw struct {
			Enabled *bool `yaml:"enabled"`
		}
		if err := resource.UnmarshalYamlE(&raw); err != nil {
			return desired, fmt.Errorf("%s does not parse", capabilityProbeSettingsResource)
		}
		desired.enabled = raw.Enabled != nil && *raw.Enabled
	}
	desired.gate = model.GetEgressIndexSettings()
	return desired, nil
}

// The probe's fleet series at one evaluation.
type capabilityProbeObservation struct {
	// disabled passes over the window, and those the running taskworkers
	// have run since they started
	disabledIncrease float64
	disabledRunning  float64
	// the newest pass finished in the lookback, zero for none
	lastPass time.Time
	// running taskworkers exporting the probe's series
	exporting int
	// the oldest running taskworker's start, zero for none in Mimir
	oldestStart time.Time
	// whether the modern provider gate holds Quality and Speed on any
	// running taskworker, as their stats collectors publish it
	// (urnetwork_stats_provider_modern_gate_on), and whether any publishes it
	// at all
	fleetGateOn    bool
	fleetGateKnown bool
}

// One bounded instant query of fleet aggregates, each tagged with its name.
func capabilityProbeQuery(environment string) string {
	selector := func(metric string, extra string) string {
		return fmt.Sprintf(`%s{env=%s,job="taskworker"%s}`, metric, strconv.Quote(environment), extra)
	}
	tag := func(name string, expression string) string {
		return fmt.Sprintf(`label_replace(%s,"monitor_capability_series",%s,"job",".*")`, expression, strconv.Quote(name))
	}
	disabled := selector("urnetwork_capability_probe_passes_total", `,result="disabled"`)
	lastPass := selector("urnetwork_capability_probe_last_pass_timestamp_seconds", "")
	return strings.Join([]string{
		tag("disabled_increase", fmt.Sprintf(`sum(increase(%s[%dm]))`, disabled, int(capabilityProbeDisabledWindow/time.Minute))),
		tag("disabled_running", fmt.Sprintf(`sum(%s)`, disabled)),
		tag("last_pass", fmt.Sprintf(`max(max_over_time(%s[%dh]))`, lastPass, int(capabilityProbeLastPassLookback/time.Hour))),
		tag("exporting", fmt.Sprintf(`count(%s)`, lastPass)),
		tag("oldest_start", fmt.Sprintf(`min(%s)`, selector("process_start_time_seconds", ""))),
		tag("gate_on", fmt.Sprintf(`max(%s)`, selector("urnetwork_stats_provider_modern_gate_on", ""))),
	}, " or ")
}

func (self capabilityProbeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	desired, err := self.loadDesired()
	if err != nil {
		return []finding{cannotObserveFinding("capability-probe/desired-config", err)}, nil
	}
	if !desired.enabled {
		// switched off, the probe runs nothing and its series stay still
		return []finding{
			healthyFinding(capabilityProbeProbeId, tierWarn, "capability-probe-disabled-pass", capabilityProbeTarget),
			healthyFinding(capabilityProbeProbeId, tierWarn, "capability-probe-stalled", capabilityProbeTarget),
		}, nil
	}
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return []finding{cannotObserveFinding("capability-probe/metrics", errors.New("no services host is configured for the loopback Mimir query"))}, nil
	}
	queryUrl := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(capabilityProbeQuery(env.cfg.env))
	output, _, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil,
		"curl -fsS --max-time 15 --max-filesize "+strconv.Itoa(capabilityProbeResponseMax)+" '"+queryUrl+"'")
	if err != nil {
		return []finding{cannotObserveFinding("capability-probe/metrics", fmt.Errorf("query Mimir through service gateways: %w", err))}, nil
	}
	now := env.now().UTC()
	observation, err := parseCapabilityProbeObservation(output, now)
	if err != nil {
		return []finding{cannotObserveFinding("capability-probe/metrics", err)}, nil
	}
	if observation.oldestStart.IsZero() {
		return []finding{cannotObserveFinding("capability-probe/metrics", errors.New("Mimir holds no running taskworker process series"))}, nil
	}
	return []finding{
		capabilityProbeDisabledPassFinding(observation),
		capabilityProbeStalledFinding(now, desired, observation),
	}, nil
}

// Rejects unknown, repeated, non-finite or skewed samples without rendering
// remote values.
func parseCapabilityProbeObservation(output string, now time.Time) (capabilityProbeObservation, error) {
	observation := capabilityProbeObservation{}
	var response mimirInstantResponse
	if len(output) > capabilityProbeResponseMax || json.Unmarshal([]byte(output), &response) != nil {
		return observation, errors.New("the Mimir response is not a bounded JSON document")
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return observation, fmt.Errorf("Mimir status=%q result_type=%q", response.Status, response.Data.ResultType)
	}
	seen := map[string]bool{}
	for _, result := range response.Data.Result {
		name := result.Metric["monitor_capability_series"]
		if seen[name] {
			return observation, fmt.Errorf("Mimir returned series %q twice", name)
		}
		seen[name] = true
		evaluatedAt, value, err := mimirInstantValue(result.Value)
		if err != nil {
			return observation, fmt.Errorf("parse series %q: %w", name, err)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return observation, fmt.Errorf("series %q has an invalid value", name)
		}
		if skew := now.Sub(evaluatedAt); skew < -capabilityProbeEvaluationSkew || capabilityProbeEvaluationSkew < skew {
			return observation, fmt.Errorf("series %q was evaluated %s from the monitor's clock", name, skew.Round(time.Second))
		}
		unixTime := func() time.Time {
			if value == 0 {
				return time.Time{}
			}
			return unixFloatTime(value)
		}
		switch name {
		case "disabled_increase":
			observation.disabledIncrease = value
		case "disabled_running":
			observation.disabledRunning = value
		case "last_pass":
			observation.lastPass = unixTime()
		case "exporting":
			observation.exporting = int(value)
		case "gate_on":
			observation.fleetGateOn = 1 <= value
			observation.fleetGateKnown = true
		case "oldest_start":
			observation.oldestStart = unixTime()
		default:
			return observation, errors.New("Mimir returned an unknown series")
		}
	}
	return observation, nil
}

// Any disabled pass while the config switches the probe on: one in the
// window, or one a running taskworker ran since it started, which covers a
// pass before the process's first scrape.
func capabilityProbeDisabledPassFinding(observation capabilityProbeObservation) finding {
	if observation.disabledIncrease <= 0 && observation.disabledRunning <= 0 {
		return healthyFinding(capabilityProbeProbeId, tierWarn, "capability-probe-disabled-pass", capabilityProbeTarget)
	}
	return finding{
		probeId: capabilityProbeProbeId, tier: tierWarn,
		class: "capability-probe-disabled-pass", target: capabilityProbeTarget, sustain: 1,
		symptom:   "A taskworker ran a capability probe pass with settings that switch the probe off, while capability_probe.yml switches it on",
		mechanism: "A taskworker container keeps the config it started with. One that started before capability_probe.yml switched the probe on, or before its host published that config, reads the probe as off, and a pass it claims ends the probe's chain: it schedules no successor. The taskworkers that read it as on schedule the chain again within ten minutes, but the stale one ends it again at each pass it claims, and the sweep restarts each time.",
		baseline:  "While capability_probe.yml switches the probe on, no running taskworker has run a disabled pass, and none has in the last 30 minutes: every taskworker started with the probe switched on.",
		observed:  fmt.Sprintf("disabled_passes_30m=%g disabled_passes_running_taskworkers=%g", observation.disabledIncrease, observation.disabledRunning),
		evidence:  "Only fleet sums of urnetwork_capability_probe_passes_total{result=\"disabled\"} leave Mimir; no host, task or provider identity is included.",
		action:    "Find the taskworkers whose urnetwork_capability_probe_passes_total{result=\"disabled\"} is above zero, confirm their hosts publish the current config (WARP_CONFIG_VERSION), and restart them (capprobe/README.md, \"Switching it on\"). Then check that the probe's pending_task row exists (run_once_key capability_probe).",
		verify:    "Every running taskworker reports zero disabled passes, the probe's pending_task row exists, and passes_total{result=~\"measured|waiting\"} advances.",
		playbook:  "SIGNALS.md §2.19g and capprobe/README.md",
	}
}

// No measured or waiting pass for capabilityProbeStallAfter while the config
// switches the probe on. A taskworker fleet that restarted since the last
// pass is given the stall's span from its oldest process's start: a switch
// on restarts it, and its first pass can take a pass's whole budget. The
// stall pages past capabilityProbeStallPageAfter while the gate is on: on
// any running taskworker as they publish it, which is what is deployed, or,
// while none publishes it, the desired gate at now, by its switch or, with
// the switch absent, from its start.
func capabilityProbeStalledFinding(now time.Time, desired capabilityProbeDesired, observation capabilityProbeObservation) finding {
	reference := observation.lastPass
	if reference.Before(observation.oldestStart) {
		reference = observation.oldestStart
	}
	age := now.Sub(reference)
	if age <= capabilityProbeStallAfter {
		return healthyFinding(capabilityProbeProbeId, tierWarn, "capability-probe-stalled", capabilityProbeTarget)
	}
	// the gate the running taskworkers decide by, which is what the evidence
	// matters to; the desired provider.yml while none publishes it
	gateOn, gateSource := desired.gateOn(now), "desired"
	if observation.fleetGateKnown {
		gateOn, gateSource = observation.fleetGateOn, "fleet"
	}
	tier := tierWarn
	if gateOn && capabilityProbeStallPageAfter < age {
		tier = tierPage
	}
	lastPassAge := "none_in_6h"
	if !observation.lastPass.IsZero() {
		lastPassAge = strconv.FormatInt(int64(now.Sub(observation.lastPass)/time.Second), 10)
	}
	gate := "off"
	if gateOn {
		gate = "on"
	}
	gateSchedule := "unknown"
	if desired.gate != nil {
		gateSchedule = strings.ReplaceAll(desired.gate.ModernProviderGateSchedule(), " ", "_")
	}
	return finding{
		probeId: capabilityProbeProbeId, tier: tier,
		class: "capability-probe-stalled", target: capabilityProbeTarget, sustain: 1,
		symptom:   fmt.Sprintf("No taskworker has finished a measured or waiting capability probe pass for %s while capability_probe.yml switches the probe on", age.Round(time.Minute)),
		mechanism: "The probe is the only writer of the modern provider gate's evidence, and a live chain finishes a pass at least every fifteen minutes. A chain whose task row is gone, whose passes fail (settings, prober shard, funding readiness, a page read or an evidence write), or whose taskworkers run an image without the probe refreshes nothing; with the gate on, providers' evidence ages toward stale_evidence and they leave Quality and Speed.",
		baseline:  "The newest pass a taskworker finished (urnetwork_capability_probe_last_pass_timestamp_seconds) is under 30 minutes old, counted from the oldest running taskworker's start when none has finished since; PAGE past six hours with the gate on: on any running taskworker as their stats collectors publish it (urnetwork_stats_provider_modern_gate_on), or, while none does, by the desired provider.yml's quality_speed_requires_modern_provider: true or, with that switch absent, from the gate's start. Evidence stays fresh for seven days after its measurement, so a shorter outage, planned database maintenance included, takes nothing out.",
		observed:  fmt.Sprintf("last_pass_age_seconds=%s oldest_taskworker_uptime_seconds=%d taskworkers_exporting=%d gate=%s gate_source=%s desired_gate_schedule=%s", lastPassAge, int64(now.Sub(observation.oldestStart)/time.Second), observation.exporting, gate, gateSource, gateSchedule),
		evidence:  "Only fleet aggregates of the probe's pass timestamp, the taskworkers' start times and their modern provider gate leave Mimir; no host, task or provider identity is included.",
		action:    "Check that the probe's pending_task row exists (run_once_key capability_probe, capprobe/README.md), then read passes_total{result=\"failed\"}, readiness_failures_total and the taskworker log's [capprobe] and task error lines as SIGNALS.md §2.19g says. taskworkers_exporting=0 means no running taskworker has the probe.",
		verify:    "passes_total{result=~\"measured|waiting\"} advances and the newest last_pass_timestamp_seconds is under 30 minutes old.",
		playbook:  "SIGNALS.md §2.19g and capprobe/README.md",
	}
}
