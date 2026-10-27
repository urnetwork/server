package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// SIGNALS.md §24.7 (drain-moves), which also owns §24.3: a draining connect
// process's admission gates and drain moves (connect/MOVE.md §2.3, §5.6, D13).
//
// A draining process refuses new client transports on every carrier and
// inbound exchange handshakes for a resident it does not have. A current
// process that refused within the refusal window while its drain gauge stayed
// at zero is stuck draining while the lb still sends it traffic.
//
// With drain moves on, a drain sends every resident a compulsory move to its
// own host, capable clients with targets and legacy clients the legacy migrate,
// and closes at the deadline a client whose resident is still local. A drained
// process whose closes exceed its legacy residents, once every compulsory move
// the mover itself sent from that process (evacuation, emergency) is taken off,
// closed capable clients too. Counts are exact per process: a counter's maximum
// less its minimum within the window, never an extrapolated rate.
//
// Drain moves run whatever the mover's mode is, so these rules apply as soon
// as the build runs. A fleet whose current processes run no such build reports
// a configured-off standing state every run (never posted, never healthy).
func NewDrainMovesSignal() Signal {
	return NewDrainMovesSignalWithSettings(DefaultConnectMoveSettings())
}

// The signal with explicit thresholds; settings must not be nil.
func NewDrainMovesSignalWithSettings(settings *ConnectMoveSettings) Signal {
	return &signalAdapter{
		number: "24.7", key: "drain-moves", name: "Connect drain gates and compulsory closes per drain",
		probe: drainMovesProbe{settings: settings},
	}
}

// Carries the thresholds; the probe holds no state between runs.
type drainMovesProbe struct {
	settings *ConnectMoveSettings
}

// The alert id of every finding.
const drainMovesProbeId = "mimir/connect-drain-moves"

func (drainMovesProbe) id() string                  { return drainMovesProbeId }
func (drainMovesProbe) tier() string                { return tierWarn }
func (self drainMovesProbe) cadence() time.Duration { return self.settings.Cadence }

// The tagged series of one drain-moves query, by tag.
var drainMovesShape = connectMoveShape{
	"start":            connectMoveRawProcessLabels,
	"start_time":       connectMoveRawProcessLabels,
	"refusal_series":   {"host", "block", "instance"},
	"refusals":         {"host", "block", "instance", "carrier"},
	"drain_recent":     {"host", "block", "instance"},
	"drained":          {"host", "block", "instance"},
	"closes":           {"host", "block", "instance"},
	"drain_moves":      {"host", "block", "instance", "capable"},
	"mover_compulsory": {"host", "block", "instance"},
}

// The carriers of connect/resident.go's drain refusals.
var drainRefusalCarriers = []string{"h1", "quic", "exchange"}

// One union query. The refusal parts read the current processes over the
// refusal window; the drain parts read every process that published within the
// drain window, including the drained ones that have since exited.
func drainMovesQuery(environment string, settings *ConnectMoveSettings, scope *connectMoveScope) string {
	selector := "{" + scope.selector(environment) + "}"
	with := func(matchers string) string { return "{" + scope.selector(environment) + "," + matchers + "}" }
	refusalWindow, drainWindow := connectMoveDuration(settings.RefusalWindow), connectMoveDuration(settings.DrainWindow)
	exact := func(metric string, matchers string, labels string) string {
		series := metric + selector
		if matchers != "" {
			series = metric + with(matchers)
		}
		return "sum by (" + labels + ") (max_over_time(" + series + "[" + drainWindow + "]) - min_over_time(" + series + "[" + drainWindow + "]))"
	}
	parts := []string{
		connectMoveTagged("start", "process_start_time_seconds"+selector),
		connectMoveTagged("start_time", "timestamp(process_start_time_seconds"+selector+")"),
		connectMoveTagged("refusal_series", "count by (host, block, instance) (urnetwork_connect_drain_refusals_total"+selector+")"),
		connectMoveTagged("refusals", "sum by (host, block, instance, carrier) (increase(urnetwork_connect_drain_refusals_total"+selector+"["+refusalWindow+"]))"),
		connectMoveTagged("drain_recent", "max by (host, block, instance) (max_over_time(urnetwork_connect_drain_residents_remaining"+selector+"["+refusalWindow+"]))"),
		connectMoveTagged("drained", "max by (host, block, instance) (max_over_time(urnetwork_connect_drain_residents_remaining"+selector+"["+drainWindow+"]))"),
		connectMoveTagged("closes", exact("urnetwork_connect_moves_compulsory_closed_total", "", "host, block, instance")),
		connectMoveTagged("drain_moves", exact("urnetwork_connect_drain_moves_sent_total", "", "host, block, instance, capable")),
		// a lazily created child's first push already counts its first send,
		// so the lifetime maximum bounds the window's sends from above
		connectMoveTagged("mover_compulsory", "sum by (host, block, instance) (max_over_time(urnetwork_connect_moves_sent_total"+with(`kind="compulsory"`)+"["+drainWindow+"]))"),
	}
	return strings.Join(parts, " or ")
}

// The finding shared by every class of this probe.
func drainMovesFinding(target string, class string) finding {
	return finding{
		probeId: drainMovesProbeId, tier: tierWarn, class: class, target: target, sustain: 2,
		baseline: "Refusals only while a process drains; and per drained process, compulsory closes at most its legacy residents (the drain moves it sent with capable=\"false\") plus every compulsory move the mover sent from it (SIGNALS.md §24.3, §24.7).",
		evidence: "One Mimir instant query through a services host's loopback Mimir. Only inventory host and block names, fixed carriers and counts leave the source; process instance nonces and response bodies do not.",
		context:  "A client that redials into a draining process before the lb drops it is refused by design. With drain moves on, every resident gets a compulsory move to its own host's addresses (capable=\"true\") or the legacy migrate (capable=\"false\"), each with a drain intent; the Redis control's drainMoves false returns drains to the legacy migrate. A drain that cannot write a resident's intent sends the legacy migrate without one and counts no drain move for it; the straggler sweep closes it, not the deadline.",
		playbook: "SIGNALS.md §24.7",
	}
}

func drainMovesTelemetryIncomplete(target string, observed string) finding {
	incomplete := drainMovesFinding(target, "drain-moves-telemetry-incomplete")
	incomplete.symptom = "The connect drain gates and drain moves cannot be judged for every desired slot"
	incomplete.mechanism = "A desired slot without a fresh current process, an ambiguous push, an unknown carrier or capable value, or an unreadable Mimir answer leaves the drain rules unknown. A process that stops publishing hides its refusals; a sibling cannot stand in for it."
	incomplete.observed = observed
	incomplete.action = "Restore each desired slot's stats push and the services host's loopback Mimir path, then read §10.2 for the drain itself."
	incomplete.verify = "Every desired slot has a fresh current process, for two runs."
	return incomplete
}

func (self drainMovesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	target := env.cfg.env + "/connect-drains"
	if !settings.valid() {
		return []finding{drainMovesTelemetryIncomplete(target, "visibility_reason=settings_invalid")}, nil
	}
	scope, reason := newConnectMoveScope(env, settings)
	if reason != "" {
		return []finding{drainMovesTelemetryIncomplete(target, "visibility_reason="+reason)}, nil
	}
	at := env.now().Add(-settings.EvaluationLag).UTC().Truncate(time.Second)
	frame, reason, err := readConnectMoveFrame(ctx, env, settings, scope, drainMovesQuery(env.cfg.env, settings, scope), at, drainMovesShape)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return []finding{drainMovesTelemetryIncomplete(target, "visibility_reason="+reason)}, nil
	}
	census, reason := newConnectMoveCensus(frame, env.cfg.env, scope, settings)
	if reason != "" {
		return []finding{drainMovesTelemetryIncomplete(target, "visibility_reason="+reason)}, nil
	}
	invalid := ""
	frame.each("refusals", func(values []string, _ float64) {
		known := false
		for _, carrier := range drainRefusalCarriers {
			known = known || values[3] == carrier
		}
		if !known {
			invalid = "unknown_refusal_carrier"
		}
	})
	frame.each("drain_moves", func(values []string, _ float64) {
		if values[3] != "true" && values[3] != "false" {
			invalid = "unknown_capable_value"
		}
	})
	if invalid != "" {
		return []finding{drainMovesTelemetryIncomplete(target, "visibility_reason="+invalid)}, nil
	}
	// Historical ranges can outlive their process-start series. Validate their
	// slots too, before any source label reaches a public finding.
	desiredSlots := map[string]bool{}
	for _, hostName := range scope.hosts {
		for _, block := range scope.blocks {
			desiredSlots[connectMoveSlot(hostName, block)] = true
		}
	}
	invalidProcessSeries := 0
	for _, tag := range []string{"refusal_series", "refusals", "drain_recent", "drained", "closes", "drain_moves", "mover_compulsory"} {
		frame.each(tag, func(values []string, _ float64) {
			if !desiredSlots[connectMoveSlot(values[0], values[1])] {
				delete(frame.tagSeries[tag], connectMoveKey(values...))
				invalidProcessSeries++
			}
		})
	}
	findings := []finding{}
	deployedProcesses, legacyProcesses := []*connectMoveProcess{}, []*connectMoveProcess{}
	for _, process := range census.processes() {
		_, gatesKnown := frame.value("refusal_series", process.values()...)
		_, capableKnown := frame.value("drain_moves", process.values("true")...)
		_, legacyKnown := frame.value("drain_moves", process.values("false")...)
		if gatesKnown || capableKnown || legacyKnown {
			deployedProcesses = append(deployedProcesses, process)
		} else {
			legacyProcesses = append(legacyProcesses, process)
		}
	}
	observed := census.coverage() + fmt.Sprintf(" drain_gate_processes=%d legacy_processes=%d refusal_window=%s drain_window=%s",
		len(deployedProcesses), len(legacyProcesses), connectMoveDuration(settings.RefusalWindow), connectMoveDuration(settings.DrainWindow))

	// §24.7: refusals while the drain gauge stayed at zero
	missingRefusalProcesses := 0
	for _, process := range deployedProcesses {
		slotTarget := target + "/" + process.host + "/" + process.block
		carrierRefusals, refusals := []string{}, 0.0
		carrierCount, countKnown := frame.value("refusal_series", process.values()...)
		refusalsKnown := countKnown && carrierCount == float64(len(drainRefusalCarriers))
		for _, carrier := range drainRefusalCarriers {
			value, known := frame.value("refusals", process.values(carrier)...)
			refusalsKnown = refusalsKnown && known
			refusals += value
			if 0 < value {
				carrierRefusals = append(carrierRefusals, carrier+":"+connectMoveNumber(math.Round(value)))
			}
		}
		draining, drainKnown := frame.value("drain_recent", process.values()...)
		if !refusalsKnown || !drainKnown {
			missingRefusalProcesses++
		}
		if !drainKnown {
			continue
		}
		if refusals <= 0 || 0 < draining {
			if refusalsKnown {
				findings = append(findings, healthyFinding(self.id(), tierWarn, "drain-refusals-outside-drain", slotTarget))
			}
			continue
		}
		stuck := drainMovesFinding(slotTarget, "drain-refusals-outside-drain")
		stuck.symptom = fmt.Sprintf("Connect %s/%s refused new admissions within %s while its drain gauge stayed at zero", process.host, process.block, connectMoveDuration(settings.RefusalWindow))
		stuck.mechanism = "Only a draining process refuses new client transports and inbound exchange handshakes, and a drain with residents sets urnetwork_connect_drain_residents_remaining above zero. Refusals with the gauge at zero throughout, from a process that still publishes, mean it is stuck draining while the lb still sends it traffic."
		stuck.observed = fmt.Sprintf("slot=%s/%s refusals=%s by_carrier=%s drain_gauge_max=0 process_age_s=%s ", process.host, process.block, connectMoveNumber(math.Round(refusals)), connectMoveNames(carrierRefusals, len(drainRefusalCarriers)), connectMoveNumber(math.Round(float64(frame.at.Unix())-process.start))) + observed
		stuck.context = "A process whose drain found no residents never sets the gauge, but it exits within the drain's bound; this alert needs two consecutive runs of a current, still publishing process. " + stuck.context
		stuck.action = "Confirm the drain on the host (§10.1: the stop child, `[c]drain` log lines) and the lb's view of the slot (§10.4). A process stuck draining serves nothing new: finish its stop under the deploy owner's authority rather than restarting siblings."
		stuck.verify = "The slot's current process refuses nothing outside a drain for a full refusal window."
		findings = append(findings, stuck)
	}

	// §24.3: closes per drained process against its legacy residents
	// Both drain-move children and the close counter are eager. Include their
	// process keys even if the drain gauge's range is missing.
	drainProcessValues := map[string][]string{}
	for _, tag := range []string{"drained", "drain_moves", "closes"} {
		frame.each(tag, func(values []string, _ float64) {
			drainProcessValues[connectMoveKey(values[:3]...)] = values[:3]
		})
	}
	judgedDrains, legacyOnlyDrains, excessDrains := 0, 0, []string{}
	missingDrainProcesses := 0
	for _, values := range drainProcessValues {
		capable, capableKnown := frame.value("drain_moves", values[0], values[1], values[2], "true")
		legacy, legacyKnown := frame.value("drain_moves", values[0], values[1], values[2], "false")
		drainedMax, drainedKnown := frame.value("drained", values...)
		closes, closesKnown := frame.value("closes", values...)
		// The mover's compulsory-send children are lazy; their absent maximum
		// is zero only for a process with the initialized drain-code series.
		moverCompulsory, _ := frame.value("mover_compulsory", values...)
		// a build from before drain moves has neither series; its drains are not judged
		if !capableKnown && !legacyKnown {
			continue
		}
		if !capableKnown || !legacyKnown || !drainedKnown || !closesKnown {
			missingDrainProcesses++
			continue
		}
		if drainedMax <= 0 && capable+legacy <= 0 {
			continue
		}
		if capable+legacy <= 0 {
			legacyOnlyDrains++
			continue
		}
		judgedDrains++
		excess := closes - moverCompulsory - legacy
		if float64(settings.MinExcessCloses) <= excess {
			excessDrains = append(excessDrains, fmt.Sprintf("%s/%s:closes=%s,legacy=%s,capable=%s,mover_compulsory=%s",
				values[0], values[1], connectMoveNumber(closes), connectMoveNumber(legacy), connectMoveNumber(capable), connectMoveNumber(moverCompulsory)))
		}
	}
	sort.Strings(excessDrains)
	complete := census.complete() && missingRefusalProcesses == 0 && missingDrainProcesses == 0 && invalidProcessSeries == 0
	if complete {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "drain-moves-telemetry-incomplete", target))
	} else {
		incompleteObserved := observed + fmt.Sprintf(" missing_refusal_processes=%d missing_drain_processes=%d invalid_process_series=%d", missingRefusalProcesses, missingDrainProcesses, invalidProcessSeries)
		findings = append(findings, drainMovesTelemetryIncomplete(target, incompleteObserved))
	}
	if len(deployedProcesses) == 0 && complete {
		state := drainMovesFinding(target, "drain-moves-not-deployed")
		state.sustain, state.configuredOff = 1, true
		state.symptom = "No current connect process runs the drain gates and drain moves"
		state.mechanism = "A process without urnetwork_connect_drain_refusals_total or initialized drain-move children runs a build from before the drain gates (connect/MOVE.md phase 0); its drains use the legacy migrate and refuse nothing. The drain rules have nothing to judge, which is not health."
		state.observed = observed
		state.action = "None while the build is not deployed. After its rollout, every current process exports the drain refusal counters."
		state.verify = "Every current process exports urnetwork_connect_drain_refusals_total."
		findings = append(findings, state)
	}
	drainObserved := fmt.Sprintf("judged_drains=%d drains_without_drain_moves=%d excess_drains=%d", judgedDrains, legacyOnlyDrains, len(excessDrains))
	if 0 < len(excessDrains) {
		excess := drainMovesFinding(target, "drain-closes-excess")
		excess.sustain = 1
		excess.symptom = fmt.Sprintf("%d drained connect processes closed more clients at their deadlines than they had legacy residents", len(excessDrains))
		excess.mechanism = "A drain closes, with reason moved, each client whose resident is still local at its compulsory deadline, migrate time plus 60 s. Legacy clients get the legacy migrate and the close; a capable client is closed only when its replacement did not connect in time. Closes above the legacy residents, after every compulsory move the mover sent from that process is taken off, are capable clients failing their replacement."
		excess.observed = drainObserved + " drains=" + connectMoveNames(excessDrains, settings.MaxListed) + " " + observed
		excess.context = "A process killed before its deadlines counts no close, and a drained process whose final push did not land is missing, so the count can only be low. " + excess.context
		excess.action = "Read the drained slots' urnetwork_connect_drain_moves_sent_total{capable} beside urnetwork_connect_move_capable_residents, and the move results of the drain window (§24.6) for the failure reasons: the replacement dials this same host's addresses, where the sibling block must accept nominations."
		excess.verify = "The next drains of the named slots close at most their legacy residents."
		excess.playbook = "SIGNALS.md §24.3"
		findings = append(findings, excess)
	} else if complete {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "drain-closes-excess", target))
	}
	return findings, nil
}
