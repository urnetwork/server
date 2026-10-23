package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §24.2 (move-outcomes), which also owns §24.6: whether the moves
// the mover sends arrive, and which target hosts fail them or hold their
// breakers open (connect/MOVE.md §5.1, §5.5, §5.9).
//
// The success ratio is arrivals by intent at the targets over the mover's sends
// at the sources, both of advisory and compulsory kind, over an hour, held
// below the floor at every step of two hours. A target host's failure share is
// the client reports that its replacement failed over the mover's sends to it,
// held above a quarter for an hour. A drain's moves point at the draining
// host's own addresses and their failures count against that host with no kind
// label, so every capable drain move of the host in the window is taken off its
// failures first: the share only alerts on failures a drain cannot explain.
//
// No sends is not health: a fleet without the mover, with the mover off or in
// dry run, or with nothing to move has no ratio to judge, and reports a
// configured-off standing state every run instead (never posted, never
// healthy). The breaker rule is judged whenever the fleet is covered, since a
// host whose every address is held open receives no sends at all.
func NewMoveOutcomesSignal() Signal {
	return NewMoveOutcomesSignalWithSettings(DefaultConnectMoveSettings())
}

// The signal with explicit thresholds; settings must not be nil.
func NewMoveOutcomesSignalWithSettings(settings *ConnectMoveSettings) Signal {
	return &signalAdapter{
		number: "24.2", key: "move-outcomes", name: "Connect move success, target failures and breakers",
		probe: moveOutcomesProbe{settings: settings},
	}
}

// Carries the thresholds; the probe holds no state between runs.
type moveOutcomesProbe struct {
	settings *ConnectMoveSettings
}

// The alert id of every finding.
const moveOutcomesProbeId = "mimir/connect-move-outcomes"

func (moveOutcomesProbe) id() string                  { return moveOutcomesProbeId }
func (moveOutcomesProbe) tier() string                { return tierWarn }
func (self moveOutcomesProbe) cadence() time.Duration { return self.settings.Cadence }

// The tagged series of one move-outcomes query, by tag.
var moveOutcomesShape = connectMoveShape{
	"start":                   connectMoveRawProcessLabels,
	"start_time":              connectMoveRawProcessLabels,
	"mode_max":                {},
	"mode_series":             {},
	"sent":                    {},
	"arrived":                 {},
	"would_send":              {},
	"sent_by_target":          {"target_host"},
	"arrived_by_target":       {"target_host"},
	"failed_by_reason":        {"reason", "target_host"},
	"declined_by_reason":      {"reason"},
	"dropped_by_cause":        {"cause"},
	"expired_by_kind":         {"kind"},
	"success_max":             {},
	"success_points":          {},
	"sends_points":            {},
	"slot_points":             {},
	"expected_success_points": {},
	"failure_min":             {"target_host"},
	"failure_points":          {"target_host"},
	"expected_failure_points": {},
	"breaker_held":            {"target_host", "interface", "family"},
	"breaker_held_points":     {"target_host", "interface", "family"},
	"expected_breaker_points": {},
	"breaker_misshapen":       {},
}

// The report labels of controller/resident_move_result.go: decline and
// failure reasons, and the causes of a dropped report.
var (
	moveOutcomeReasons = []string{"other", "extender_carrier", "dns_carrier", "no_family_target", "memory_budget", "recent_move", "target_failed_recently", "mobile_background_or_metered", "move_in_progress", "connect_timeout", "dial_error", "tls_error", "auth_error", "unknown"}
	moveOutcomeCauses  = []string{"invalid_move_id", "invalid_result", "no_intent", "foreign_move"}
	moveOutcomeKinds   = []string{"drain", "advisory", "compulsory"}
)

// One union query. Sends, arrivals and expiries come from the connect
// service's desired slots; reports arrive on every control path (connect, api,
// proxy, taskworker), so their counters carry only the environment.
func moveOutcomesQuery(environment string, settings *ConnectMoveSettings, scope *connectMoveScope) string {
	selector := "{" + scope.selector(environment) + "}"
	with := func(matchers string) string { return "{" + scope.selector(environment) + "," + matchers + "}" }
	reports := "{env=" + strconv.Quote(environment) + "}"
	window := connectMoveDuration(settings.SuccessWindow)
	arrivedSelector := with(`kind=~"advisory|compulsory"`)
	sent := "sum(increase(urnetwork_connect_moves_sent_total" + selector + "[" + window + "]))"
	arrived := "sum(increase(urnetwork_connect_moves_arrived_total" + arrivedSelector + "[" + window + "]))"
	minimumSends := strconv.Itoa(settings.MinSends)
	slotsComplete := "(count(group by (host, block) (process_start_time_seconds" + selector + ")) == " + strconv.Itoa(len(scope.hosts)*len(scope.blocks)) + ")"
	success := "(((" + arrived + ") or vector(0)) / (" + sent + " >= " + minimumSends + ") and on() " + slotsComplete + ")"
	targetSent := "sum by (target_host) (increase(urnetwork_connect_moves_sent_total" + selector + "[" + window + "]))"
	targetFailed := "sum by (target_host) (increase(urnetwork_connect_moves_failed_total" + reports + "[" + window + "]))"
	drainMoves := `sum by (target_host) (label_replace(increase(urnetwork_connect_drain_moves_sent_total` + with(`capable="true"`) + "[" + window + `]),"target_host","$1","host","(.*)"))`
	moverFailed := "clamp_min((" + targetFailed + " - on(target_host) " + drainMoves + ") or " + targetFailed + ", 0)"
	failure := "(((" + moverFailed + ") or (" + targetSent + " * 0)) / (" + targetSent + " >= " + minimumSends + ") and on() " + slotsComplete + ")"
	breakers := "max by (target_host, interface, family) (urnetwork_connect_move_target_breaker_open" + with(`target_host!="",interface!="",family=~"4|6"`) + ")"
	sustained := func(function string, expression string, length time.Duration, resolution time.Duration) string {
		return function + "(" + expression + "[" + connectMoveDuration(length) + ":" + connectMoveDuration(resolution) + "])"
	}
	parts := []string{
		connectMoveTagged("start", "process_start_time_seconds"+selector),
		connectMoveTagged("start_time", "timestamp(process_start_time_seconds"+selector+")"),
		connectMoveTagged("mode_max", "max(urnetwork_connect_mover_mode"+selector+")"),
		connectMoveTagged("mode_series", "count(urnetwork_connect_mover_mode"+selector+")"),
		connectMoveTagged("sent", sent),
		connectMoveTagged("arrived", arrived),
		connectMoveTagged("would_send", "sum(increase(urnetwork_connect_moves_would_send_total"+selector+"["+window+"]))"),
		connectMoveTagged("sent_by_target", targetSent),
		connectMoveTagged("arrived_by_target", "sum by (target_host) (increase(urnetwork_connect_moves_arrived_total"+arrivedSelector+"["+window+"]))"),
		connectMoveTagged("failed_by_reason", "sum by (reason, target_host) (increase(urnetwork_connect_moves_failed_total"+reports+"["+window+"]))"),
		connectMoveTagged("declined_by_reason", "sum by (reason) (increase(urnetwork_connect_moves_declined_total"+reports+"["+window+"]))"),
		connectMoveTagged("dropped_by_cause", "sum by (cause) (increase(urnetwork_connect_move_results_dropped_total"+reports+"["+window+"]))"),
		connectMoveTagged("expired_by_kind", "sum by (kind) (increase(urnetwork_connect_moves_expired_total"+selector+"["+window+"]))"),
		connectMoveTagged("success_max", sustained("max_over_time", success, settings.SuccessSustain, settings.SustainStep)),
		connectMoveTagged("success_points", sustained("count_over_time", success, settings.SuccessSustain, settings.SustainStep)),
		connectMoveTagged("sends_points", sustained("count_over_time", "("+sent+" >= "+minimumSends+")", settings.SuccessSustain, settings.SustainStep)),
		connectMoveTagged("slot_points", sustained("count_over_time", slotsComplete, settings.SuccessSustain, settings.SustainStep)),
		connectMoveTagged("expected_success_points", sustained("count_over_time", "vector(1)", settings.SuccessSustain, settings.SustainStep)),
		connectMoveTagged("failure_min", sustained("min_over_time", failure, settings.TargetFailureSustain, settings.SustainStep)),
		connectMoveTagged("failure_points", sustained("count_over_time", failure, settings.TargetFailureSustain, settings.SustainStep)),
		connectMoveTagged("expected_failure_points", sustained("count_over_time", "vector(1)", settings.TargetFailureSustain, settings.SustainStep)),
		connectMoveTagged("breaker_held", sustained("min_over_time", breakers, settings.BreakerHold, settings.BreakerStep)),
		connectMoveTagged("breaker_held_points", sustained("count_over_time", breakers, settings.BreakerHold, settings.BreakerStep)),
		connectMoveTagged("expected_breaker_points", sustained("count_over_time", "vector(1)", settings.BreakerHold, settings.BreakerStep)),
		connectMoveTagged("breaker_misshapen", "count(urnetwork_connect_move_target_breaker_open"+selector+" unless urnetwork_connect_move_target_breaker_open"+with(`target_host!="",interface!="",family=~"4|6"`)+")"),
	}
	return strings.Join(parts, " or ")
}

// The finding shared by every class of this probe.
func moveOutcomesFinding(target string, class string) finding {
	return finding{
		probeId: moveOutcomesProbeId, tier: tierWarn, class: class, target: target, sustain: 2,
		baseline: "With the mover sending, arrivals by intent over sends stay at or above 0.5 at some step of every two hours, no target host's replacement failures exceed a quarter of its sends for an hour, and no host has more than one address whose breaker is held open (SIGNALS.md §24.2, §24.6).",
		evidence: "One Mimir instant query through a services host's loopback Mimir. Only inventory host names, fixed reasons, causes and kinds, and counts leave the source; process instance nonces and response bodies do not.",
		context:  "Clients decline advisory moves by design (memory budget, a recent move, a target that failed, mobile background or metered network), so the ratio sits below 1. A move the budget refused was never sent and is in neither count; an unrelated reconnect that lands on the target consumes the intent and counts as an arrival, which is the placement wanted. Arrivals lag sends by up to the 3 minute intent window. Reports are best effort and sent once, so a client that loses its control path reports nothing.",
		playbook: "SIGNALS.md §24.2",
	}
}

func moveOutcomesTelemetryIncomplete(target string, observed string) finding {
	incomplete := moveOutcomesFinding(target, "move-outcomes-telemetry-incomplete")
	incomplete.symptom = "The connect move outcomes cannot be judged over their windows"
	incomplete.mechanism = "A desired slot without a fresh current process, a step of the window without every slot, a breaker series without the target host, interface and family the probe reads, or an unreadable Mimir answer leaves the ratios unknown: a process that stops publishing removes its sends or arrivals from the sums, which a ratio would read as success or failure."
	incomplete.observed = observed
	incomplete.action = "Restore each desired slot's stats push and the services host's loopback Mimir path. As built, the target breaker gauge's own host label collides with the stats pusher's host grouping label: the first breaker read in a process makes its every later push fail, so its slot goes stale (SIGNALS.md §24.6)."
	incomplete.verify = "Every desired slot is current at every step of the windows, for two runs."
	return incomplete
}

func (self moveOutcomesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	target := env.cfg.env + "/connect-moves"
	if !settings.valid() {
		return []finding{moveOutcomesTelemetryIncomplete(target, "visibility_reason=settings_invalid")}, nil
	}
	scope, reason := newConnectMoveScope(env, settings)
	if reason != "" {
		return []finding{moveOutcomesTelemetryIncomplete(target, "visibility_reason="+reason)}, nil
	}
	at := env.now().Add(-settings.EvaluationLag).UTC().Truncate(time.Second)
	frame, reason, err := readConnectMoveFrame(ctx, env, settings, scope, moveOutcomesQuery(env.cfg.env, settings, scope), at, moveOutcomesShape)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return []finding{moveOutcomesTelemetryIncomplete(target, "visibility_reason="+reason)}, nil
	}
	census, reason := newConnectMoveCensus(frame, env.cfg.env, scope, settings)
	if reason != "" {
		return []finding{moveOutcomesTelemetryIncomplete(target, "visibility_reason="+reason)}, nil
	}
	known := func(values []string, allowed []string) bool {
		for _, value := range values {
			found := false
			for _, candidate := range allowed {
				found = found || value == candidate
			}
			if !found {
				return false
			}
		}
		return true
	}
	invalid := ""
	frame.each("failed_by_reason", func(values []string, _ float64) {
		if !known(values[:1], moveOutcomeReasons) {
			invalid = "unknown_report_reason"
		}
	})
	frame.each("declined_by_reason", func(values []string, _ float64) {
		if !known(values, moveOutcomeReasons) {
			invalid = "unknown_report_reason"
		}
	})
	frame.each("dropped_by_cause", func(values []string, _ float64) {
		if !known(values, moveOutcomeCauses) {
			invalid = "unknown_report_cause"
		}
	})
	frame.each("expired_by_kind", func(values []string, _ float64) {
		if !known(values, moveOutcomeKinds) {
			invalid = "unknown_move_kind"
		}
	})
	if invalid != "" {
		return []finding{moveOutcomesTelemetryIncomplete(target, "visibility_reason="+invalid)}, nil
	}

	desiredHosts := map[string]bool{}
	for _, name := range scope.hosts {
		desiredHosts[name] = true
	}
	scalar := func(tag string) float64 {
		value, _ := frame.scalar(tag)
		return value
	}
	modeSeries, deployed := frame.scalar("mode_series")
	deployed = deployed && 0 < modeSeries
	misshapen := scalar("breaker_misshapen")
	complete := census.complete() && misshapen == 0
	observed := census.coverage() + fmt.Sprintf(" mover_series=%s mode_max=%s window=%s sent=%s arrived=%s would_send=%s breaker_misshapen_series=%s",
		connectMoveNumber(modeSeries), connectMoveNumber(scalar("mode_max")), connectMoveDuration(settings.SuccessWindow),
		connectMoveNumber(math.Round(scalar("sent"))), connectMoveNumber(math.Round(scalar("arrived"))), connectMoveNumber(math.Round(scalar("would_send"))),
		connectMoveNumber(misshapen))
	findings := []finding{}
	if complete {
		findings = append(findings, healthyFinding(self.id(), tierWarn, "move-outcomes-telemetry-incomplete", target))
	} else {
		findings = append(findings, moveOutcomesTelemetryIncomplete(target, observed))
		return findings, nil
	}
	dormant := func(reason string, mechanism string) finding {
		state := moveOutcomesFinding(target, "move-outcomes-dormant")
		state.sustain, state.configuredOff = 1, true
		state.symptom = "The connect move success ratio has nothing to judge: " + strings.ReplaceAll(reason, "_", " ")
		state.mechanism = mechanism
		state.observed = "dormant_reason=" + reason + " " + observed
		state.action = "None while the mover is not sending. The ratio is judged once the mover sends at least " + strconv.Itoa(settings.MinSends) + " moves in every window step of the sustain."
		state.verify = "The mover sends enough moves at every step of a full window."
		return state
	}
	if !deployed {
		return append(findings, dormant("not_deployed", "No connect process exports the mover's mode, so no build that sends moves runs; there are no sends, arrivals or reports to judge, which is not health.")), nil
	}

	// §24.6 breakers: judged whether or not the mover sends
	expectedBreaker := scalar("expected_breaker_points")
	hostOpenAddresses := map[string][]string{}
	frame.each("breaker_held", func(values []string, held float64) {
		points, _ := frame.value("breaker_held_points", values...)
		if held == 1 && points == expectedBreaker && desiredHosts[values[0]] {
			hostOpenAddresses[values[0]] = append(hostOpenAddresses[values[0]], values[1]+"/ipv"+values[2])
		}
	})
	for _, hostName := range scope.hosts {
		hostTarget := target + "/" + hostName
		openAddresses := hostOpenAddresses[hostName]
		if len(openAddresses) < settings.BreakerMinOpenAddresses {
			findings = append(findings, healthyFinding(self.id(), tierWarn, "move-target-breakers-open", hostTarget))
			continue
		}
		breakers := moveOutcomesFinding(hostTarget, "move-target-breakers-open")
		breakers.sustain = 1
		breakers.symptom = fmt.Sprintf("%d move target addresses of %s have held their breakers open for %s", len(openAddresses), hostName, connectMoveDuration(settings.BreakerHold))
		breakers.mechanism = "A target address's breaker opens when its failure reports over sends exceed 0.2 with at least 20 sends, and stays open for 10 minutes; the mover skips an open address. More than one address of a host held open points at the host or its path rather than one interface."
		breakers.observed = "target_host=" + hostName + " open_addresses=" + connectMoveNames(openAddresses, settings.MaxListed) + " hold=" + connectMoveDuration(settings.BreakerHold) + " " + observed
		breakers.action = "Read urnetwork_connect_moves_failed_total{target_host} by reason: connect_timeout and dial_error point at the path to the target, tls_error at its certificates, auth_error at the platform. Check the host's public interfaces and §16 and §18 for its addresses."
		breakers.verify = "At most one address of the host reads open over a full hold."
		breakers.playbook = "SIGNALS.md §24.6"
		findings = append(findings, breakers)
	}

	sent := scalar("sent")
	if sent <= 0 {
		switch {
		case 0 < scalar("would_send"):
			findings = append(findings, dormant("dry_run", "Dry run reserves budget and counts the moves it would send, but sends nothing; its would-sends have no arrivals to compare (SIGNALS.md §24.2)."))
		case scalar("mode_max") == 0:
			findings = append(findings, dormant("mover_off", "Every process reads mode 0: the mover sends nothing, so arrivals over sends is undefined. This is the shipped default, not health."))
		default:
			findings = append(findings, dormant("no_sends", "The mover is on but sent nothing in the window: every host is within the band or none has candidates. Arrivals over zero sends is undefined, not health."))
		}
		return findings, nil
	}

	// per-target context, then §24.6 failure shares
	targetSends, targetArrivals, targetFailures := map[string]float64{}, map[string]float64{}, map[string]float64{}
	frame.each("sent_by_target", func(values []string, value float64) { targetSends[values[0]] = value })
	frame.each("arrived_by_target", func(values []string, value float64) { targetArrivals[values[0]] = value })
	reasonFailures, declineReasons, dropCauses, expiryKinds := []string{}, []string{}, []string{}, []string{}
	frame.each("failed_by_reason", func(values []string, value float64) {
		targetFailures[values[1]] += value
		if 0 < value {
			reasonFailures = append(reasonFailures, values[1]+"/"+values[0]+":"+connectMoveNumber(math.Round(value)))
		}
	})
	frame.each("declined_by_reason", func(values []string, value float64) {
		if 0 < value {
			declineReasons = append(declineReasons, values[0]+":"+connectMoveNumber(math.Round(value)))
		}
	})
	frame.each("dropped_by_cause", func(values []string, value float64) {
		if 0 < value {
			dropCauses = append(dropCauses, values[0]+":"+connectMoveNumber(math.Round(value)))
		}
	})
	frame.each("expired_by_kind", func(values []string, value float64) {
		if 0 < value {
			expiryKinds = append(expiryKinds, values[0]+":"+connectMoveNumber(math.Round(value)))
		}
	})
	targets := []string{}
	for _, hostName := range scope.hosts {
		if sends := targetSends[hostName]; 0 < sends {
			targets = append(targets, fmt.Sprintf("%s:%s/%s", hostName, connectMoveNumber(math.Round(targetArrivals[hostName])), connectMoveNumber(math.Round(sends))))
		}
	}
	foreignTargets := 0
	for hostName := range targetSends {
		if !desiredHosts[hostName] {
			foreignTargets++
		}
	}
	resultsObserved := " arrived_over_sent_by_target=" + connectMoveNames(targets, settings.MaxListed) +
		" foreign_targets=" + strconv.Itoa(foreignTargets) +
		" failed_by_target_reason=" + connectMoveNames(reasonFailures, settings.MaxListed) +
		" declined_by_reason=" + connectMoveNames(declineReasons, settings.MaxListed) +
		" dropped_by_cause=" + connectMoveNames(dropCauses, settings.MaxListed) +
		" expired_by_kind=" + connectMoveNames(expiryKinds, settings.MaxListed)
	expectedFailure := scalar("expected_failure_points")
	for _, hostName := range scope.hosts {
		hostTarget := target + "/" + hostName
		share, judged := frame.value("failure_min", hostName)
		points, _ := frame.value("failure_points", hostName)
		if !judged || points != expectedFailure {
			continue
		}
		if share <= settings.TargetFailureShare {
			findings = append(findings, healthyFinding(self.id(), tierWarn, "move-target-failures", hostTarget))
			continue
		}
		failures := moveOutcomesFinding(hostTarget, "move-target-failures")
		failures.sustain = 1
		failures.symptom = fmt.Sprintf("Moves to %s failed their replacement for more than %.0f%% of its sends for %s", hostName, 100*settings.TargetFailureShare, connectMoveDuration(settings.TargetFailureSustain))
		failures.mechanism = "A client reports a move whose replacement transport failed, counted against the target host; a failure counts only the carrier its old transport used, so a network that blocks one carrier is not blamed on the target. Every capable drain move of the host in the window was taken off its failures first, so a deploy's drain alone cannot raise this."
		failures.observed = fmt.Sprintf("target_host=%s failure_share_min=%s sends=%s failures=%s", hostName, connectMoveNumber(share), connectMoveNumber(math.Round(targetSends[hostName])), connectMoveNumber(math.Round(targetFailures[hostName]))) + resultsObserved + " " + observed
		failures.action = "Read the reasons above for this host: connect_timeout and dial_error point at the path to the target, tls_error at its certificates, auth_error at the platform. Check the breaker gauge for its addresses and §16, §18 for its public interfaces."
		failures.verify = "The host's failure share falls to or below a quarter at some step of a full hour."
		failures.playbook = "SIGNALS.md §24.6"
		findings = append(findings, failures)
	}

	// §24.2: arrivals over sends at every step of the sustain
	expectedSuccess := scalar("expected_success_points")
	windowObserved := fmt.Sprintf(" sustain=%s step=%s expected_steps=%s slot_steps=%s sends_steps=%s judged_steps=%s success_max=%s",
		connectMoveDuration(settings.SuccessSustain), connectMoveDuration(settings.SustainStep), connectMoveNumber(expectedSuccess),
		connectMoveNumber(scalar("slot_points")), connectMoveNumber(scalar("sends_points")), connectMoveNumber(scalar("success_points")), connectMoveNumber(scalar("success_max")))
	switch {
	case scalar("slot_points") < expectedSuccess:
		return append(findings, moveOutcomesTelemetryIncomplete(target, observed+windowObserved)), nil
	case scalar("sends_points") < expectedSuccess:
		return append(findings, dormant("below_minimum_sends", "The mover sends, but some step of the sustain had fewer than the minimum sends in its window, and a ratio of so few moves is noise, not a measurement.")), nil
	case scalar("success_points") < expectedSuccess:
		return append(findings, moveOutcomesTelemetryIncomplete(target, observed+windowObserved)), nil
	}
	if settings.SuccessRatioFloor <= scalar("success_max") {
		return append(findings, healthyFinding(self.id(), tierWarn, "move-success-low", target)), nil
	}
	low := moveOutcomesFinding(target, "move-success-low")
	low.sustain = 1
	low.symptom = fmt.Sprintf("Fewer than %.0f%% of the mover's sends arrived, at every step of %s", 100*settings.SuccessRatioFloor, connectMoveDuration(settings.SuccessSustain))
	low.mechanism = "Arrivals are residents taken over by intent at the target; sends are the mover's balance, arrival, evacuation and emergency moves at the source. Declines are expected, but most moves failing to arrive means clients decline or fail them, or arrive somewhere the intent does not match."
	low.observed = "success_ratio_max=" + connectMoveNumber(scalar("success_max")) + windowObserved + resultsObserved + " " + observed
	low.action = "Localize by target host above, then by the failure reasons (§24.6), the decline reasons and the breaker gauge. Declines by memory_budget, recent_move, move_in_progress and mobile_background_or_metered are the client's rules working; a steady foreign_move or invalid_move_id drop is a client reporting a move it was not sent."
	low.verify = "Arrivals over sends reach the floor at some step of a full sustain."
	return append(findings, low), nil
}
