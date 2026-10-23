package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/model"
)

// SIGNALS.md §24.4 (mover), which also owns §24.1 and §24.5: the connect
// mover's mode against the Redis control, its pauses and failures, and the
// host imbalance it exists to remove (connect/MOVE.md §5.5, §5.9).
//
// The mover ships off. A fleet whose processes run no mover, or whose mover is
// off, in dry run or on for less than the imbalance window, is a standing state
// reported every run: a configured-off alert that is never posted, never
// healthy and never clears an earlier incident. Only a fleet that agrees with
// the control, on for the whole window, has its imbalance judged.
//
// One Redis read takes the control `{connect_move}control`; one Mimir query
// takes every current process's mode, tick outcomes, sends and send errors, the
// fleet's budget refusals and dry-run would-sends, and the normalized load of
// each host at every step of the imbalance window. A host the control
// evacuates empties by design and is left out of the load. The monitor never
// writes the control.
func NewMoverSignal() Signal {
	return NewMoverSignalWithSettings(DefaultConnectMoveSettings())
}

// The signal with explicit thresholds.
func NewMoverSignalWithSettings(settings *ConnectMoveSettings) Signal {
	return &signalAdapter{
		number: "24.4", key: "mover", name: "Connect mover mode, pauses and host imbalance",
		probe: moverProbe{settings: settings},
	}
}

// Carries the thresholds; the probe holds no state between runs.
type moverProbe struct {
	settings *ConnectMoveSettings
}

// The alert id of every finding.
const moverProbeId = "mimir/connect-mover"

func (moverProbe) id() string             { return moverProbeId }
func (moverProbe) tier() string           { return tierWarn }
func (moverProbe) cadence() time.Duration { return 5 * time.Minute }

// The control hash every connect process reads each mover tick
// (model.connectMoveControlKey).
const moverControlKey = "{connect_move}control"

// The tick outcomes of connect/move.go, in the order the mover reaches them.
var moverTickOutcomes = []string{"off", "draining", "store_error", "no_load", "in_band", "db_paused", "no_target", "no_candidate", "budget_refused", "send_failed", "moved"}

// The outcomes of a tick that wanted to move: the source had excess and
// candidates, so the database guard, the targets, the budget or the send
// decided it.
var moverActingTickOutcomes = []string{"db_paused", "no_target", "budget_refused", "send_failed", "moved"}

// The Redis control as the mover narrows it (model.NarrowConnectMoveControl),
// under the code defaults: the probe cannot read a process's code switches.
type moverControl struct {
	// absent, present or unreadable
	state string
	// the fixed reason an unreadable control could not be read
	reason string
	// the mode the narrowed control gives every process
	mode int
	// whether the control sets the mode, rather than the code default
	modeSet bool
	// fields whose values do not parse, which narrow as far as they can
	// (mode off, no budget, no balance source, drain moves off)
	invalidFields []string
	// fields outside the control's set, counted and never named
	unknownFields int
	// hosts the control evacuates; their residents leave by design
	evacuatedHosts map[string]bool
}

// Reads the control once from the first Redis host the monitor may contact.
// An unreadable control is a fixed reason; only a parent that ended returns
// an error. The reply is a RESP2 field/value array; values are never rendered.
func readMoverControl(ctx context.Context, env *probeEnv, settings *ConnectMoveSettings) (*moverControl, error) {
	control := &moverControl{state: "unreadable", evacuatedHosts: map[string]bool{}}
	excludedHosts := map[string]bool{}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		for _, name := range scoped.excludedNames() {
			excludedHosts[name] = true
		}
	}
	var redisHost *host
	for _, candidate := range env.cfg.hostsWithRole("redis-cluster") {
		if !candidate.disabled && !excludedHosts[candidate.name] && 0 < candidate.redisEntryPort {
			redisHost = candidate
			break
		}
	}
	if redisHost == nil {
		control.reason = "redis_host_unavailable"
		return control, nil
	}
	output, err := env.runner.redis(ctx, redisHost, redisHost.redisEntryPort, "-c", "--json", "HGETALL", moverControlKey)
	if contextErr := ctx.Err(); contextErr != nil {
		if err != nil {
			return nil, &signalContextError{contextErr: contextErr, sourceErr: err}
		}
		return nil, contextErr
	}
	if err != nil {
		control.reason = "redis_read_failed"
		return control, nil
	}
	var reply []string
	if json.Unmarshal([]byte(output), &reply) != nil || len(reply)%2 != 0 {
		control.reason = "invalid_control_reply"
		return control, nil
	}
	fieldValues := map[string]string{}
	for i := 0; i < len(reply); i += 2 {
		if _, duplicate := fieldValues[reply[i]]; duplicate {
			control.reason = "invalid_control_reply"
			return control, nil
		}
		fieldValues[reply[i]] = reply[i+1]
	}
	control.state = "absent"
	if 0 < len(fieldValues) {
		control.state = "present"
	}
	parsesBool := func(value string) bool {
		value = strings.TrimSpace(value)
		if value == "on" || value == "off" {
			return true
		}
		_, err := strconv.ParseBool(value)
		return err == nil
	}
	for field, value := range fieldValues {
		valid := true
		switch field {
		case model.ConnectMoveControlFieldMode:
			_, valid = model.ParseConnectMoveMode(value)
			control.modeSet = true
		case model.ConnectMoveControlFieldMaxPerMinute:
			_, err := strconv.Atoi(strings.TrimSpace(value))
			valid = err == nil
		case model.ConnectMoveControlFieldTolerance:
			tolerance, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			valid = err == nil && !math.IsNaN(tolerance)
		case model.ConnectMoveControlFieldDrainMoves:
			valid = parsesBool(value)
		case model.ConnectMoveControlFieldEvacuateHosts:
			for _, name := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
				control.evacuatedHosts[name] = true
			}
		default:
			control.unknownFields++
		}
		if !valid {
			control.invalidFields = append(control.invalidFields, field)
		}
	}
	sort.Strings(control.invalidFields)
	narrowed := model.NarrowConnectMoveControl(fieldValues, model.ConnectMoveControlCeiling{
		EnableMove:       true,
		EnableDrainMoves: true,
		DefaultMode:      settings.DefaultMode,
	})
	control.mode = int(narrowed.Mode)
	return control, nil
}

// The control's state for observed text: never a field value.
func (self *moverControl) observed() string {
	if self.state == "unreadable" {
		return "control=unreadable control_reason=" + self.reason
	}
	text := "control=" + self.state + " control_mode=" + connectMoveModeName(self.mode) + " control_sets_mode=" + strconv.FormatBool(self.modeSet)
	text += " control_invalid_fields=" + connectMoveNames(self.invalidFields, len(self.invalidFields)+1)
	text += " control_unknown_fields=" + strconv.Itoa(self.unknownFields) + " evacuated_hosts=" + strconv.Itoa(len(self.evacuatedHosts))
	return text
}

// The desired hosts the load reads: every desired connect host the control
// does not evacuate.
func moverLoadHosts(scope *connectMoveScope, control *moverControl) []string {
	hosts := []string{}
	for _, name := range scope.hosts {
		if !control.evacuatedHosts[name] {
			hosts = append(hosts, name)
		}
	}
	return hosts
}

// The tagged series of one mover query, by tag.
var moverShape = connectMoveShape{
	"start":            connectMoveRawProcessLabels,
	"start_time":       connectMoveRawProcessLabels,
	"mode":             connectMoveRawProcessLabels,
	"mode_time":        connectMoveRawProcessLabels,
	"ticks":            {"host", "block", "instance", "outcome"},
	"sent":             {"host", "block", "instance"},
	"send_errors":      {"host", "block", "instance"},
	"refused":          {"cap"},
	"would_send":       {"source_host"},
	"excess":           {"host"},
	"load":             {"host"},
	"load_start":       {"host"},
	"arrivals":         {"target_host"},
	"ticks_window":     {"outcome"},
	"publish_failed":   {"host"},
	"imbalance_min":    {},
	"imbalance_points": {},
	"empty_points":     {},
	"on_points":        {},
	"mode_points":      {},
	"load_points":      {},
	"expected_points":  {},
}

// One union query. Per-process parts keep instance as a private join key; the
// sustained parts evaluate the imbalance at every step of the window, gated on
// the mover being on and every load host publishing at that step.
func moverQuery(environment string, settings *ConnectMoveSettings, scope *connectMoveScope, loadHosts []string) string {
	selector := "{" + scope.selector(environment) + "}"
	with := func(matchers string) string { return "{" + scope.selector(environment) + "," + matchers + "}" }
	tickWindow, window, step := connectMoveDuration(settings.TickWindow), connectMoveDuration(settings.ImbalanceWindow), connectMoveDuration(settings.SustainStep)
	loadSelector := selector
	if len(loadHosts) < len(scope.hosts) {
		evacuated := []string{}
		loadHostSet := map[string]bool{}
		for _, name := range loadHosts {
			loadHostSet[name] = true
		}
		for _, name := range scope.hosts {
			if !loadHostSet[name] {
				evacuated = append(evacuated, regexp.QuoteMeta(name))
			}
		}
		loadSelector = with("host!~" + strconv.Quote(strings.Join(evacuated, "|")))
	}
	load := "max by (host) (urnetwork_connect_move_host_normalized_load" + loadSelector + ")"
	on := "(max(urnetwork_connect_mover_mode" + selector + ") == 2)"
	complete := "(count(" + load + ") == " + strconv.Itoa(len(loadHosts)) + ")"
	ratio := "(max(" + load + ") / min(" + load + " > 0))"
	gated := func(expression string) string {
		return "(" + expression + " and on() " + on + " and on() " + complete + ")"
	}
	sustained := func(function string, expression string) string {
		return function + "(" + expression + "[" + window + ":" + step + "])"
	}
	parts := []string{
		connectMoveTagged("start", "process_start_time_seconds"+selector),
		connectMoveTagged("start_time", "timestamp(process_start_time_seconds"+selector+")"),
		connectMoveTagged("mode", "urnetwork_connect_mover_mode"+selector),
		connectMoveTagged("mode_time", "timestamp(urnetwork_connect_mover_mode"+selector+")"),
		connectMoveTagged("ticks", "sum by (host, block, instance, outcome) (increase(urnetwork_connect_mover_ticks_total"+selector+"["+tickWindow+"]))"),
		connectMoveTagged("sent", "sum by (host, block, instance) (increase(urnetwork_connect_moves_sent_total"+selector+"["+tickWindow+"]))"),
		connectMoveTagged("send_errors", "sum by (host, block, instance) (increase(urnetwork_connect_move_send_errors_total"+selector+"["+tickWindow+"]))"),
		connectMoveTagged("refused", "sum by (cap) (increase(urnetwork_connect_move_budget_refused_total"+selector+"["+tickWindow+"]))"),
		connectMoveTagged("would_send", "sum by (source_host) (increase(urnetwork_connect_moves_would_send_total"+selector+"["+tickWindow+"]))"),
		connectMoveTagged("excess", "max by (host) (urnetwork_connect_move_host_excess"+selector+")"),
		connectMoveTagged("load", load),
		connectMoveTagged("load_start", "max by (host) (urnetwork_connect_move_host_normalized_load"+loadSelector+" offset "+window+")"),
		connectMoveTagged("arrivals", "sum by (target_host) (increase(urnetwork_connect_moves_arrived_total"+with(`kind=~"advisory|compulsory"`)+"["+window+"]))"),
		connectMoveTagged("ticks_window", "sum by (outcome) (increase(urnetwork_connect_mover_ticks_total"+selector+"["+window+"]))"),
		connectMoveTagged("publish_failed", "sum by (host) (increase(urnetwork_connect_move_load_publishes_total"+with(`outcome="failed"`)+"["+window+"]))"),
		connectMoveTagged("imbalance_min", sustained("min_over_time", gated(ratio))),
		connectMoveTagged("imbalance_points", sustained("count_over_time", gated(ratio))),
		connectMoveTagged("empty_points", sustained("count_over_time", gated("(count("+load+" == 0) > 0)"))),
		connectMoveTagged("on_points", sustained("count_over_time", on)),
		connectMoveTagged("mode_points", sustained("count_over_time", "count(urnetwork_connect_mover_mode"+selector+")")),
		connectMoveTagged("load_points", sustained("count_over_time", complete)),
		connectMoveTagged("expected_points", sustained("count_over_time", "vector(1)")),
	}
	return strings.Join(parts, " or ")
}

// One current process that runs the mover.
type moverProcess struct {
	process       *connectMoveProcess
	mode          int
	outcomeTicks  map[string]float64
	ticks         float64
	sent          float64
	sendErrors    float64
	storeErrorPct float64
}

// The current processes by whether they run the mover. A deployed process's
// mode is pushed with its start; a process without the gauge runs a build
// without the mover.
type moverFleet struct {
	deployed   []*moverProcess
	legacy     []*connectMoveProcess
	incoherent int
	minMode    int
	maxMode    int
}

// Reads every current process's mode and tick outcomes. A lazily created
// send counter that is absent on a deployed process has counted nothing. A
// mode pushed apart from its start, or outside the three modes, is incoherent.
func newMoverFleet(frame *connectMoveFrame, census *connectMoveCensus, environment string) (*moverFleet, string) {
	fleet := &moverFleet{minMode: math.MaxInt, maxMode: -1}
	knownOutcomes := map[string]bool{}
	for _, outcome := range moverTickOutcomes {
		knownOutcomes[outcome] = true
	}
	invalid := ""
	frame.each("ticks", func(values []string, value float64) {
		if !knownOutcomes[values[3]] {
			invalid = "unknown_tick_outcome"
		}
	})
	if invalid != "" {
		return nil, invalid
	}
	for _, process := range census.processes() {
		mode, deployed := frame.value("mode", process.rawValues(environment)...)
		if !deployed {
			fleet.legacy = append(fleet.legacy, process)
			continue
		}
		pushedAt, ok := frame.value("mode_time", process.rawValues(environment)...)
		if !ok || pushedAt != process.pushedAt || math.Trunc(mode) != mode || mode < 0 || 2 < mode {
			fleet.incoherent++
			continue
		}
		mover := &moverProcess{process: process, mode: int(mode), outcomeTicks: map[string]float64{}}
		for _, outcome := range moverTickOutcomes {
			ticks, _ := frame.value("ticks", process.values(outcome)...)
			mover.outcomeTicks[outcome] = ticks
			mover.ticks += ticks
		}
		mover.sent, _ = frame.value("sent", process.values()...)
		mover.sendErrors, _ = frame.value("send_errors", process.values()...)
		if 0 < mover.ticks {
			mover.storeErrorPct = mover.outcomeTicks["store_error"] / mover.ticks
		}
		fleet.deployed = append(fleet.deployed, mover)
		fleet.minMode = min(fleet.minMode, mover.mode)
		fleet.maxMode = max(fleet.maxMode, mover.mode)
	}
	return fleet, ""
}

// The slot names of processes, for observed text.
func moverSlotNames(processes []*connectMoveProcess, limit int) string {
	names := make([]string, len(processes))
	for i, process := range processes {
		names[i] = process.host + "/" + process.block
	}
	return connectMoveNames(names, limit)
}

// The fleet's modes for observed text.
func (self *moverFleet) observed() string {
	modeCounts := map[int]int{}
	for _, mover := range self.deployed {
		modeCounts[mover.mode]++
	}
	return fmt.Sprintf("mover_processes=%d legacy_processes=%d mode_off=%d mode_dry_run=%d mode_on=%d",
		len(self.deployed), len(self.legacy), modeCounts[0], modeCounts[1], modeCounts[2])
}

// The fleet's tick outcomes over a window as shares of all ticks and of the
// acting ticks, for observed text. The mix is per tick, not per move.
func moverTickMix(outcomeTicks map[string]float64, prefix string) string {
	total, acting := 0.0, 0.0
	for _, outcome := range moverTickOutcomes {
		total += outcomeTicks[outcome]
	}
	for _, outcome := range moverActingTickOutcomes {
		acting += outcomeTicks[outcome]
	}
	text := prefix + "ticks=" + connectMoveNumber(math.Round(total)) + " " + prefix + "acting_ticks=" + connectMoveNumber(math.Round(acting))
	for _, outcome := range moverTickOutcomes {
		share := 0.0
		if 0 < total {
			share = outcomeTicks[outcome] / total
		}
		text += " " + prefix + outcome + "_share=" + strconv.FormatFloat(share, 'f', 3, 64)
	}
	return text
}

// The finding shared by every class of this probe.
func moverFinding(target string, class string) finding {
	return finding{
		probeId: moverProbeId, tier: tierWarn, class: class, target: target, sustain: 2,
		baseline: "Every desired connect slot has a current process that runs the mover; the fleet's modes agree with the Redis control; ticks do not fail closed or pause for most of the window; and, with the mover on for the whole window, no host stays above 1.5 times the least loaded host's normalized load (SIGNALS.md §24.1).",
		evidence: "One Mimir instant query through a services host's loopback Mimir, and one HGETALL of the Redis control. Only inventory host and block names, fixed label values and counts leave the source; process instance nonces, control values and response bodies do not.",
		context:  "The mover ships off: until an operator sets the control, the bands are design targets (connect/MOVE.md §5.8), not production baselines. The control can only narrow the code settings, so a process reads at most what its EnableMove allows. Tick outcome shares are per tick, not per move: every tick of a process that is off or within the band counts off or in_band.",
		playbook: "SIGNALS.md §24.4",
	}
}

// A configured-off standing state: reported every run, never posted, never
// healthy, and holding this signal's other incidents open while it lasts.
func moverState(target string, class string, symptom string, mechanism string, observed string, action string, verify string) finding {
	state := moverFinding(target, class)
	state.sustain, state.configuredOff = 1, true
	state.symptom, state.mechanism, state.observed, state.action, state.verify = symptom, mechanism, observed, action, verify
	return state
}

func moverTelemetryIncomplete(target string, observed string) finding {
	incomplete := moverFinding(target, "mover-telemetry-incomplete")
	incomplete.symptom = "The connect mover's mode and tick outcomes cannot be established for every desired slot"
	incomplete.mechanism = "A desired slot without a fresh current process, an ambiguous or incoherent push, an unreadable or malformed Mimir answer, or an unavailable services gateway leaves the fleet's mover state unknown. A healthy sibling cannot stand in for a missing slot, and a missing series is never mode off."
	incomplete.observed = observed
	incomplete.action = "Restore each desired slot's stats push and the services host's loopback Mimir path. A slot that went stale right after the mover left off points first at the target breaker gauge, whose host label collides with the stats pusher's grouping label and fails the process's every push (SIGNALS.md §24.6)."
	incomplete.verify = "Every desired slot has a fresh current process whose mode is pushed with its start, for two runs."
	return incomplete
}

func (self moverProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	target := env.cfg.env + "/connect-mover"
	if !settings.valid() {
		return []finding{moverTelemetryIncomplete(target, "visibility_reason=settings_invalid")}, nil
	}
	scope, reason := newConnectMoveScope(env, settings)
	if reason != "" {
		return []finding{moverTelemetryIncomplete(target, "visibility_reason="+reason)}, nil
	}
	control, err := readMoverControl(ctx, env, settings)
	if err != nil {
		return nil, err
	}
	findings := []finding{}
	controlKnown := control.state != "unreadable"
	if controlKnown {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "mover-control-unobservable", target))
	} else {
		unobservable := moverFinding(target, "mover-control-unobservable")
		unobservable.symptom = "The mover's Redis control cannot be read, so the fleet's modes cannot be compared with it"
		unobservable.mechanism = "Without the control the probe cannot tell an operator's off from processes that fail closed, nor which hosts are evacuated by design, so the imbalance rule is not judged. The gauges are still read."
		unobservable.observed = control.observed()
		unobservable.action = "Read `redis-cli -c HGETALL '{connect_move}control'` on the Redis host of §10.4 and restore the monitor's Redis path; correlate with §3 when the mover's own store_error ticks rise too. Never write the control from the monitor."
		unobservable.verify = "The control reads for two runs."
		findings = append(findings, unobservable)
	}
	loadHosts := moverLoadHosts(scope, control)
	at := env.now().Add(-settings.EvaluationLag).UTC().Truncate(time.Second)
	frame, reason, err := readConnectMoveFrame(ctx, env, settings, scope, moverQuery(env.cfg.env, settings, scope, loadHosts), at, moverShape)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return append(findings, moverTelemetryIncomplete(target, "visibility_reason="+reason)), nil
	}
	census, reason := newConnectMoveCensus(frame, env.cfg.env, scope, settings)
	if reason != "" {
		return append(findings, moverTelemetryIncomplete(target, "visibility_reason="+reason)), nil
	}
	fleet, reason := newMoverFleet(frame, census, env.cfg.env)
	if reason != "" {
		return append(findings, moverTelemetryIncomplete(target, "visibility_reason="+reason)), nil
	}
	complete := census.complete() && fleet.incoherent == 0
	observed := census.coverage() + " incoherent_slots=" + strconv.Itoa(fleet.incoherent) + " " + fleet.observed() + " " + control.observed()
	if complete {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "mover-telemetry-incomplete", target))
	} else {
		findings = append(findings, moverTelemetryIncomplete(target, observed))
	}

	if 0 < len(control.invalidFields) {
		invalid := moverFinding(target, "mover-control-invalid")
		invalid.sustain = 1
		invalid.symptom = "The mover's Redis control holds a value that does not parse: " + connectMoveNames(control.invalidFields, len(control.invalidFields)+1)
		invalid.mechanism = "A control field that does not parse narrows as far as it can: mode off, a fleet budget of zero, an infinite band (no host is a balance source) or drain moves off (model.NarrowConnectMoveControl). The operator's intent is lost silently; the mover reads it every tick."
		invalid.observed = control.observed()
		invalid.action = "Correct the named field with the operator who owns the control: mode is off, dry_run or on; maxPerMinute an integer; tolerance a number; drainMoves true or false. The monitor never writes the control."
		invalid.verify = "Every control field parses, and the fleet's modes match the control's for two runs."
		findings = append(findings, invalid)
	} else if controlKnown {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "mover-control-invalid", target))
	}

	if len(fleet.deployed) == 0 {
		if complete {
			findings = append(findings, moverState(target, "mover-not-deployed",
				"No current connect process runs the mover",
				"A process with no mode series runs a build without the mover (SIGNALS.md §24.4). Placement is unchanged; the imbalance, pause and failure rules have nothing to judge, which is not health.",
				observed,
				"None while the mover's build is not deployed. After its rollout, every desired slot's current process exports urnetwork_connect_mover_mode.",
				"Every current process exports the mode gauge."))
		}
		return findings, nil
	}

	// §24.4: the fleet agrees with itself and with the control
	divergentReasons := []string{}
	if fleet.minMode != fleet.maxMode {
		divergentReasons = append(divergentReasons, "processes_disagree")
	}
	if 0 < len(fleet.legacy) && 0 < fleet.maxMode {
		divergentReasons = append(divergentReasons, "processes_without_mover")
	}
	if controlKnown && (fleet.maxMode != control.mode || fleet.minMode != control.mode) {
		divergentReasons = append(divergentReasons, "fleet_differs_from_control")
	}
	if 0 < len(divergentReasons) {
		belowProcesses, belowStoreErrors := []*connectMoveProcess{}, 0
		for _, mover := range fleet.deployed {
			if mover.mode < fleet.maxMode || (controlKnown && mover.mode < control.mode) {
				belowProcesses = append(belowProcesses, mover.process)
				if settings.StoreErrorShare <= mover.storeErrorPct {
					belowStoreErrors++
				}
			}
		}
		divergent := moverFinding(target, "mover-mode-divergent")
		divergent.sustain = settings.DivergenceSustain
		divergent.symptom = "The connect fleet's mover modes disagree with each other or with the Redis control"
		divergent.mechanism = "Each process exports its mode after the control narrowed its code settings, and the control can only narrow. A process at 0 while others read 1 or 2 cannot read the control (store_error ticks, it fails closed) or runs with EnableMove off; a process with no mode series runs a build without the mover; a fleet above the control runs a code default that is not off."
		divergent.observed = observed + " divergence=" + strings.Join(divergentReasons, ",") +
			" below_mode_slots=" + moverSlotNames(belowProcesses, settings.MaxListed) +
			" below_mode_store_error_slots=" + strconv.Itoa(belowStoreErrors) +
			" legacy_slots=" + moverSlotNames(fleet.legacy, settings.MaxListed)
		divergent.context = "A control change reaches every process within one 10 s tick and one 15 s push, and a rollout leaves old and new builds side by side for minutes; this alert needs " + strconv.Itoa(settings.DivergenceSustain) + " consecutive runs."
		divergent.action = "For each slot below the fleet's mode, read its urnetwork_connect_mover_ticks_total{outcome=\"store_error\"}: rising means it cannot read the control (correlate with §3); flat means its code settings turn the mover off. For a slot without the mode gauge, compare its running artifact with the rollout (§8.12). Do not set the control from the monitor."
		divergent.verify = "Every current process reads the control's mode, for three runs."
		findings = append(findings, divergent)
	} else if complete {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "mover-mode-divergent", target))
	}

	// §24.5: failures and pauses while the mover is meant to act
	active := 0 < fleet.maxMode || (controlKnown && 0 < control.mode)
	failingProcesses := []*connectMoveProcess{}
	fleetOutcomeTicks := map[string]float64{}
	for _, mover := range fleet.deployed {
		for outcome, ticks := range mover.outcomeTicks {
			fleetOutcomeTicks[outcome] += ticks
		}
		if 0 < mover.ticks && settings.StoreErrorShare <= mover.storeErrorPct {
			failingProcesses = append(failingProcesses, mover.process)
		}
	}
	tickMix := moverTickMix(fleetOutcomeTicks, "window_")
	window := connectMoveDuration(settings.TickWindow)
	if active && 0 < len(failingProcesses) {
		failing := moverFinding(target, "mover-store-errors")
		failing.symptom = fmt.Sprintf("%d connect processes failed closed on a Redis error for at least %.0f%% of their mover ticks", len(failingProcesses), 100*settings.StoreErrorShare)
		failing.mechanism = "A tick whose control, load table, cooldown or budget read or write fails ends store_error and moves nothing; an unreadable control also reads as mode off. The mover is meant to act, so these processes are not placing clients."
		failing.observed = fmt.Sprintf("window=%s store_error_slots=%s %s", window, moverSlotNames(failingProcesses, settings.MaxListed), tickMix)
		failing.action = "Correlate with the Redis signals of §3 for the same window, then the failing slots' Redis path. The mover recovers at its next tick once Redis answers; no mover action is needed."
		failing.verify = "Every acting process's store_error share falls below the threshold for two runs."
		findings = append(findings, failing)
	} else if complete && active {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "mover-store-errors", target))
	}
	if 1 <= fleet.maxMode {
		acting := 0.0
		for _, outcome := range moverActingTickOutcomes {
			acting += fleetOutcomeTicks[outcome]
		}
		if float64(settings.MinActingTicks) <= acting && settings.DbPausedShare <= fleetOutcomeTicks["db_paused"]/acting {
			paused := moverFinding(target, "mover-db-paused")
			paused.symptom = fmt.Sprintf("The database guard paused %.0f%% of the mover's acting ticks", 100*fleetOutcomeTicks["db_paused"]/acting)
			paused.mechanism = "A tick that would move pauses while its process's PostgreSQL pool acquire wait is above 50 ms, because every arrival writes to the database. Fleet-wide db_paused while the pool signals of §2 are hot is the guard working."
			paused.observed = "window=" + window + " " + tickMix
			paused.action = "Read the database, not the mover: §2.11a, §5.12 and the pool signals of §2. Placement resumes on its own when the acquire wait falls."
			paused.verify = "The paused share of acting ticks falls below the threshold for two runs."
			findings = append(findings, paused)
		} else if complete {
			findings = append(findings, healthyFinding(self.id(), tierWarn, "mover-db-paused", target))
		}
	}
	attemptingProcesses, erringProcesses := []*connectMoveProcess{}, []*connectMoveProcess{}
	attempts, sendErrors := 0.0, 0.0
	for _, mover := range fleet.deployed {
		processAttempts := mover.sent + mover.sendErrors
		if processAttempts <= 0 {
			continue
		}
		attemptingProcesses = append(attemptingProcesses, mover.process)
		attempts += processAttempts
		sendErrors += mover.sendErrors
		if 0 < mover.sendErrors && settings.SendErrorMinShare <= mover.sendErrors/processAttempts {
			erringProcesses = append(erringProcesses, mover.process)
		}
	}
	if settings.SendErrorMinProcesses <= len(attemptingProcesses) && len(erringProcesses) == len(attemptingProcesses) {
		burst := moverFinding(target, "move-send-errors-fleetwide")
		burst.sustain = 1
		burst.symptom = fmt.Sprintf("Every one of %d sending connect processes failed most of its move sends", len(attemptingProcesses))
		burst.mechanism = "A send error is a move the budget reserved and the executor did not send, usually because the resident left. One process's errors are ordinary churn; errors on every sending process at once point at the move primitives."
		burst.observed = fmt.Sprintf("window=%s sending_slots=%s send_errors=%s attempts=%s %s", window, moverSlotNames(attemptingProcesses, settings.MaxListed), connectMoveNumber(math.Round(sendErrors)), connectMoveNumber(math.Round(attempts)), tickMix)
		burst.action = "Read urnetwork_connect_move_send_errors_total by kind and trigger and the connect logs at V(1) for `[move]` send failures; compare the running artifact with the move primitives' last change. Turning the mover off through the control stops the sends within 10 s."
		burst.verify = "Send errors fall below half of attempts on at least one sending process for two runs."
		findings = append(findings, burst)
	} else if complete {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "move-send-errors-fleetwide", target))
	}

	if !complete {
		return findings, nil
	}

	// the fleet's widest mode is the state; a divergent fleet reports it too,
	// beside the divergence, so no rule it leaves unjudged can clear
	switch fleet.maxMode {
	case 0:
		findings = append(findings, moverState(target, "mover-off",
			"The connect mover is off: placement is not managed",
			"Every process that runs the mover reads mode 0. The mover still computes the load, so the imbalance is observed, but its rule applies only while the mover is on (SIGNALS.md §24.1). This is the shipped default, not health; a control that asks for more is reported as divergence.",
			observed+" "+moverInstantImbalance(frame, settings)+" "+tickMix,
			"None while an operator keeps the mover off. To start it, follow connect/MOVE.md §7: dry run first, then on through the control.",
			"An operator sets the control and every process reads its mode."))
		return findings, nil
	case 1:
		findings = append(findings, moverState(target, "mover-dry-run",
			"The connect mover is in dry run: it reserves budget and counts the moves it would send",
			"Dry run does everything except the cooldown, the intent and the send (connect/MOVE.md §5.5), so nothing moves and the imbalance rule does not apply. Compare would-sends with the host excess before turning it on (MOVE.md §7 phase 3).",
			observed+" "+moverDryRun(frame, settings)+" "+tickMix,
			"Compare each source host's would-sends with its excess; turn the mover on through the control only when they agree.",
			"An operator moves the control to on or off and every process reads it."))
		return findings, nil
	}
	if !controlKnown {
		return findings, nil
	}

	// §24.1: the imbalance at every step of the window while the mover is on
	values := map[string]float64{}
	for _, tag := range []string{"imbalance_min", "imbalance_points", "empty_points", "on_points", "mode_points", "load_points", "expected_points"} {
		values[tag], _ = frame.scalar(tag)
	}
	expected := values["expected_points"]
	windowObserved := fmt.Sprintf("window=%s step=%s expected_steps=%s mode_steps=%s on_steps=%s load_complete_steps=%s judged_steps=%s empty_host_steps=%s",
		connectMoveDuration(settings.ImbalanceWindow), connectMoveDuration(settings.SustainStep),
		connectMoveNumber(expected), connectMoveNumber(values["mode_points"]), connectMoveNumber(values["on_points"]),
		connectMoveNumber(values["load_points"]), connectMoveNumber(values["imbalance_points"]), connectMoveNumber(values["empty_points"]))
	windowTicks := map[string]float64{}
	frame.each("ticks_window", func(labelValues []string, ticks float64) { windowTicks[labelValues[0]] = ticks })
	failedPublishes := []string{}
	frame.each("publish_failed", func(labelValues []string, failed float64) {
		if 0 < failed {
			failedPublishes = append(failedPublishes, labelValues[0]+":"+connectMoveNumber(math.Round(failed)))
		}
	})
	loadIncomplete := func(cause string) finding {
		incomplete := moverFinding(target, "move-host-load-incomplete")
		incomplete.symptom = "The host imbalance cannot be judged over the window: " + cause
		incomplete.mechanism = "A host whose load publishes fail drops out of the load table and so out of the ratio, and a host with no connect process has no series (SIGNALS.md §24.1). A step without every host's load, or without the mode gauge, cannot count toward the sustained ratio."
		incomplete.observed = windowObserved + " failed_load_publishes=" + connectMoveNames(failedPublishes, settings.MaxListed) + " " + observed
		incomplete.action = "Read urnetwork_connect_move_load_publishes_total{outcome=\"failed\"} by host and the Redis path of the hosts it names; for a host with no load series, check that its connect processes run and push."
		incomplete.verify = "Every load host publishes at every step of a full window."
		return incomplete
	}
	switch {
	case values["mode_points"] < expected:
		findings = append(findings, loadIncomplete("the mode gauge is missing at some step"))
		return findings, nil
	case values["on_points"] < expected:
		findings = append(findings, moverState(target, "mover-warming",
			"The connect mover is on, but not yet for the whole imbalance window",
			"The imbalance rule needs the mover on at every step of its window (SIGNALS.md §24.1); about 10k moves take about 85 minutes, so the ratio falls over hours, not minutes.",
			windowObserved+" "+observed,
			"None. The rule is judged once the mover has been on for the whole window.",
			"The mover stays on for a full window."))
		return findings, nil
	case values["load_points"] < expected:
		findings = append(findings, loadIncomplete("some step lacks a host's load"))
		return findings, nil
	}
	imbalanced := values["imbalance_points"] == expected && settings.ImbalanceRatio < values["imbalance_min"]
	empty := values["empty_points"] == expected
	if !imbalanced && !empty {
		if values["imbalance_points"]+values["empty_points"] < expected {
			findings = append(findings, loadIncomplete("some step has no ratio"))
			return findings, nil
		}
		findings = append(findings, healthyFinding(self.id(), tierWarn, "move-host-imbalance", target))
		return findings, nil
	}

	// the least loaded host now, its load at the window's start, and its
	// arrivals over the window
	minimumHost, minimumLoad, maximumHost, maximumLoad := "", math.Inf(1), "", -1.0
	frame.each("load", func(labelValues []string, load float64) {
		if load < minimumLoad {
			minimumHost, minimumLoad = labelValues[0], load
		}
		if maximumLoad < load {
			maximumHost, maximumLoad = labelValues[0], load
		}
	})
	startLoad, startKnown := frame.value("load_start", minimumHost)
	arrivals, _ := frame.value("arrivals", minimumHost)
	fillingArrivals := settings.FillingMinArrivalsPerMinute * settings.ImbalanceWindow.Minutes()
	filling := 0 < minimumLoad && (!startKnown || startLoad < minimumLoad) && fillingArrivals <= arrivals
	imbalanceObserved := windowObserved + fmt.Sprintf(" ratio_min=%s max_host=%s max_load=%s min_host=%s min_load=%s min_host_start_load=%s min_host_arrivals=%s filling_arrivals=%s failed_load_publishes=%s ",
		connectMoveNumber(values["imbalance_min"]), maximumHost, connectMoveNumber(maximumLoad), minimumHost, connectMoveNumber(minimumLoad),
		map[bool]string{true: connectMoveNumber(startLoad), false: "absent"}[startKnown], connectMoveNumber(math.Round(arrivals)), connectMoveNumber(fillingArrivals),
		connectMoveNames(failedPublishes, settings.MaxListed)) + moverTickMix(windowTicks, "imbalance_window_")
	if filling {
		findings = append(findings, moverState(target, "move-host-filling",
			"The least loaded connect host is filling under the target cap",
			"A new or emptied host starts at no load, so the ratio stays high while it fills at the arrival cap of 40 moves a minute (SIGNALS.md §24.1). Its load rose over the window and moves arrive there, so the imbalance is the mover working, not failing.",
			imbalanceObserved,
			"None while the host fills. If it stops rising, the imbalance rule judges the next full window.",
			"The ratio falls under the band, or the host's load stops rising and the imbalance rule judges it."))
		return findings, nil
	}
	imbalance := moverFinding(target, "move-host-imbalance")
	imbalance.sustain = 1
	imbalance.symptom = fmt.Sprintf("Connect hosts stayed imbalanced with the mover on for %s: max over min normalized load stayed above %s", connectMoveDuration(settings.ImbalanceWindow), connectMoveNumber(settings.ImbalanceRatio))
	if empty && !imbalanced {
		imbalance.symptom = fmt.Sprintf("A connect host stayed empty with the mover on for %s", connectMoveDuration(settings.ImbalanceWindow))
	}
	imbalance.mechanism = "Load is residents per unit of capacity, cores times move_weight. The mover sends idle, capable residents from hosts above the band to hosts below the mean, within the per-minute budget; a ratio that stays high at every step of the window means moves are not reaching the light hosts, or not fast enough."
	imbalance.observed = imbalanceObserved
	imbalance.context = "A host mid-deploy reads light for minutes because draining processes leave the load; it cannot hold the ratio high for the whole window alone. A host the control evacuates is left out. A new host filling under the target cap is reported as filling instead."
	imbalance.action = "Read the window's tick outcomes above: no_target means no host below the mean is under the 0.75 CPU guard (or its addresses' breakers are open, §24.6); db_paused means the database guard (§24.5); budget_refused means the budget is the bound; store_error means Redis (§3); in_band means the band is wider than the imbalance. Do not raise the budget without a plan for the database and the target hosts."
	imbalance.verify = "The ratio falls to or below the band at some step of a full window with the mover on."
	imbalance.playbook = "SIGNALS.md §24.1"
	return append(findings, imbalance), nil
}

// The instant imbalance the load gauges show, for an off fleet's state.
func moverInstantImbalance(frame *connectMoveFrame, settings *ConnectMoveSettings) string {
	minimumLoad, maximumLoad, hosts, emptyHosts := math.Inf(1), 0.0, 0, 0
	frame.each("load", func(_ []string, load float64) {
		hosts++
		if load == 0 {
			emptyHosts++
			return
		}
		minimumLoad, maximumLoad = math.Min(minimumLoad, load), math.Max(maximumLoad, load)
	})
	ratio := "unknown"
	if 0 < maximumLoad && !math.IsInf(minimumLoad, 0) {
		ratio = connectMoveNumber(maximumLoad / minimumLoad)
	}
	return fmt.Sprintf("load_hosts=%d empty_load_hosts=%d instant_ratio=%s ratio_band=%s", hosts, emptyHosts, ratio, connectMoveNumber(settings.ImbalanceRatio))
}

// A dry run's would-sends by source host beside each host's excess.
func moverDryRun(frame *connectMoveFrame, settings *ConnectMoveSettings) string {
	wouldSends, excesses := []string{}, []string{}
	frame.each("would_send", func(labelValues []string, sends float64) {
		wouldSends = append(wouldSends, labelValues[0]+":"+connectMoveNumber(math.Round(sends)))
	})
	frame.each("excess", func(labelValues []string, excess float64) {
		excesses = append(excesses, labelValues[0]+":"+connectMoveNumber(math.Round(excess)))
	})
	return "window=" + connectMoveDuration(settings.TickWindow) + " would_send_by_source=" + connectMoveNames(wouldSends, settings.MaxListed) + " host_excess=" + connectMoveNames(excesses, settings.MaxListed)
}
