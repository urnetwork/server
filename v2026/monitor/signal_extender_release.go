package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §28.1 maps to signal_extender_release.go and
// signal_extender_release_test.go. Whether the extender records the operator
// signs still reach clients, judged at the outcome of each channel rather than
// at its publisher (connect/EXTENDER.md C5, C6, R3):
//
//   - gossip: the share of the online extenders whose record the gossip
//     service released in the trailing day. A record lives a day and the drip
//     re-releases each before it expires, so a healthy share is about one.
//     It falls whatever stopped the release: a dead drain, a drip that queues
//     nothing, a stats collector that stopped seeing the queue;
//   - the activation bootstrap: stored activations. Each answers its extender
//     with its own fresh record and eight random peers, which since gossip
//     stalled on 2026-11-06 were the only fresh records an extender held;
//   - gated release: the answers to release requests that released
//     something, while clients ask.
//
// The publishers' own failures are §27.2's (extender-publishing), the public
// dns records §28.2's, the seed bundle §27.1's, and whether extenders still
// recognise each other §2.19c's stale-directories condition.
//
// Every part is one fleet-wide value; no extender, address or account reaches
// an alert.
func NewExtenderReleaseSignal() Signal {
	return NewExtenderReleaseSignalWithSettings(DefaultExtenderReleaseSettings())
}

// The signal with explicit thresholds.
func NewExtenderReleaseSignalWithSettings(settings *ExtenderReleaseSettings) Signal {
	return &signalAdapter{
		number: "28.1", key: "extender-release", name: "Extender release to clients",
		probe: extenderReleaseProbe{settings: settings},
	}
}

const extenderReleaseProbeId = "mimir/extender-release"

// The thresholds and bounds of §28.1.
type ExtenderReleaseSettings struct {
	// a pool of fewer online extenders than this is not judged
	MinOnlineExtenders int
	// gossip: the extenders released in the trailing day under
	// CoverageMinShare of the online ones at every evaluation of CoverageHold
	CoverageMinShare float64
	CoverageHold     time.Duration
	// activation: no stored activation in the trailing ActivationWindow at
	// every evaluation of ActivationHold
	ActivationWindow time.Duration
	ActivationHold   time.Duration
	// gated release: at least ReleaseMinAnswers answers a client could have
	// been served by in the trailing ReleaseWindow, under ReleaseMinOkShare of
	// them releasing anything, at every evaluation of ReleaseHold
	ReleaseWindow     time.Duration
	ReleaseMinAnswers float64
	ReleaseMinOkShare float64
	ReleaseHold       time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §28.1.
func DefaultExtenderReleaseSettings() *ExtenderReleaseSettings {
	read := DefaultStormReadSettings()
	read.Cadence = 5 * time.Minute
	return &ExtenderReleaseSettings{
		MinOnlineExtenders: 10,
		CoverageMinShare:   0.5,
		CoverageHold:       60 * time.Minute,
		ActivationWindow:   time.Hour,
		ActivationHold:     30 * time.Minute,
		ReleaseWindow:      time.Hour,
		ReleaseMinAnswers:  10,
		ReleaseMinOkShare:  0.5,
		ReleaseHold:        30 * time.Minute,
		MaxListed:          8,
		StormReadSettings:  read,
	}
}

func (self *ExtenderReleaseSettings) valid() bool {
	windowValid := func(window time.Duration) bool {
		return window >= time.Minute && window%time.Minute == 0
	}
	return self != nil && 0 < self.MinOnlineExtenders &&
		0 < self.CoverageMinShare && self.CoverageMinShare <= 1 &&
		windowValid(self.ActivationWindow) && windowValid(self.ReleaseWindow) &&
		0 < self.ReleaseMinAnswers && 0 < self.ReleaseMinOkShare && self.ReleaseMinOkShare <= 1 &&
		0 < self.MaxListed &&
		self.StormReadSettings.valid(self.CoverageHold, self.ActivationHold, self.ReleaseHold)
}

type extenderReleaseProbe struct {
	settings *ExtenderReleaseSettings
}

func (extenderReleaseProbe) id() string   { return extenderReleaseProbeId }
func (extenderReleaseProbe) tier() string { return tierWarn }

func (self extenderReleaseProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self extenderReleaseProbe) effectiveSettings() *ExtenderReleaseSettings {
	if self.settings == nil {
		return DefaultExtenderReleaseSettings()
	}
	return self.settings
}

// The longest hold, which is how many evaluations one read covers.
func (self *ExtenderReleaseSettings) readSteps() int {
	return self.holdSteps(max(self.CoverageHold, self.ActivationHold, self.ReleaseHold))
}

// The release outcomes a client could have been served by: rate limits,
// spent epochs and probation refuse a requester by design (connect/EXTENDER.md
// R3), so they are no failure of the channel.
const extenderReleaseByDesignRefusals = "rate_limited|epoch_spent|probation"

// §28.1's expressions, each tagged with its part.
func extenderReleaseQuery(environment string, settings *ExtenderReleaseSettings) string {
	env := "env=" + strconv.Quote(environment)
	activationWindow := "[" + stormDuration(settings.ActivationWindow) + "]"
	releaseWindow := "[" + stormDuration(settings.ReleaseWindow) + "]"
	return strings.Join([]string{
		fleetPart("online", "max(urnetwork_stats_online_extenders{"+env+"})"),
		fleetPart("gossip_extenders", "max(urnetwork_stats_extender_gossip_extenders_24h{"+env+`,kind="record"})`),
		fleetPart("gossip_records", "max(urnetwork_stats_extender_gossip_released_24h{"+env+`,kind="record"})`),
		fleetPart("dns_extenders", "max(urnetwork_taskworker_extender_dns_extenders{"+env+"})"),
		fleetPart("activated", "sum(increase(urnetwork_extender_activations_total{"+env+`,stage="store",outcome="activated"}`+activationWindow+"))"),
		fleetPart("activation_posts", "sum(increase(urnetwork_extender_activations_total{"+env+"}"+activationWindow+"))"),
		fleetPart("release_ok", "sum(increase(urnetwork_extender_releases_total{"+env+`,outcome="ok"}`+releaseWindow+"))"),
		fleetPart("release_answers", "sum(increase(urnetwork_extender_releases_total{"+env+",outcome!~"+strconv.Quote(extenderReleaseByDesignRefusals)+"}"+releaseWindow+"))"),
	}, " or ")
}

var extenderReleaseShapes = map[string]stormShape{
	"online":           {},
	"gossip_extenders": {},
	"gossip_records":   {},
	"dns_extenders":    {},
	"activated":        {},
	"activation_posts": {},
	"release_ok":       {},
	"release_answers":  {},
}

func (self extenderReleaseProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	stateTarget := env.cfg.env + "/extender-release"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(extenderReleaseProbeId, "extender-release-unobservable", stateTarget, "§28.1", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, extenderReleaseQuery(env.cfg.env, settings), extenderReleaseShapes, settings.readSteps(), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}
	reasons := history.invalidReasons()
	if !history.answered("online", "") {
		// no taskworker's stats collector exported the online pool, so
		// neither the coverage nor the activations can be judged against it
		return []finding{unobservable(append(reasons, "extender_state_missing"), nil)}, nil
	}
	at := history.at()
	findings := []finding{}
	gaps := []string{}
	reading := func(part string, step int64) stormReading {
		return history.reading(part, "", step)
	}
	judge := func(class string, target string, hold stormHold, incident func() finding) {
		switch {
		case hold.held:
			findings = append(findings, incident())
		case hold.clear:
			findings = append(findings, healthyFinding(extenderReleaseProbeId, tierWarn, class, target))
		default:
			findings = append(findings, stormClearHold(extenderReleaseProbeId, class, target))
		}
	}
	// a pool too small to judge reads as within every level
	smallPool := func(step int64) (bool, bool) {
		online := reading("online", step)
		if !online.present {
			return false, false
		}
		return online.value < float64(settings.MinOnlineExtenders), true
	}

	// gossip: the online extenders with a record released in the day
	coverageAt := func(step int64) stormReading {
		small, known := smallPool(step)
		released := reading("gossip_extenders", step)
		switch {
		case !known || !released.present:
			return stormReading{}
		case small:
			return stormValue(1)
		}
		return stormValue(released.value / reading("online", step).value)
	}
	coverage := stormHoldOf(history.last(settings.holdSteps(settings.CoverageHold)), stormLevel{threshold: settings.CoverageMinShare, under: true}, coverageAt)
	if !coverage.complete {
		gaps = append(gaps, "gossip[missing]")
	}
	gossipTarget := stateTarget + "/gossip"
	judge("extender-release-coverage-low", gossipTarget, coverage, func() finding {
		return finding{
			probeId: extenderReleaseProbeId, tier: tierWarn, class: "extender-release-coverage-low", target: gossipTarget, sustain: 1,
			symptom:   fmt.Sprintf("Gossip released a record for under %.0f%% of the online extenders in the trailing day, at every evaluation for %s", 100*settings.CoverageMinShare, settings.CoverageHold),
			mechanism: "A signed extender record lives 24 hours, and the drip re-releases each online extender's record through the gossip service before it expires, so about every online extender has a record released in any day. An extender with none has an expired record in every directory gossip fills: clients stop dialing it, and other extenders refuse its pings as an unknown pinger (§2.19c). On 2026-11-06 the drain died in a Main brownout and the share fell to zero by the next morning; nothing alerted for five days.",
			baseline:  fmt.Sprintf("About one: 1.0-1.15 from 2026-10-30 to 11-06, with revoked extenders still counted for the day. WARN under %.2f for %s, with at least %d online extenders.", settings.CoverageMinShare, settings.CoverageHold, settings.MinOnlineExtenders),
			observed: fmt.Sprintf("online_extenders=%s gossip_extenders_24h=%s gossip_records_24h=%s coverage=%s dns_extenders=%s evaluation_unix=%d",
				stormNumber(reading("online", at), "%.0f"),
				stormNumber(reading("gossip_extenders", at), "%.0f"),
				stormNumber(reading("gossip_records", at), "%.0f"),
				stormNumber(coverageAt(at), "%.2f"),
				stormNumber(reading("dns_extenders", at), "%.0f"),
				at,
			),
			evidence: "One bounded Mimir range read of urnetwork_stats_extender_gossip_extenders_24h{kind=\"record\"} and urnetwork_stats_online_extenders, which every taskworker's stats collector computes from network_extender_publish and network_extender on the replica, merged by max.",
			context:  "This is the outcome of the gossip channel, not its publisher: extender-gossip-stalled (§27.2) sees a queue that does not drain within minutes; this sees any cause, a drip that queues nothing included, a day later. dns_extenders is the extenders whose addresses the dns tick last landed (what it wanted, on a taskworker that predates §27.2's fix).",
			action:   "Read §27.2 first: extender-gossip-stalled means the gossip replicas are not draining (their log for `[gossip]drain pass failed` and `[gossip]not ready`). If the queue is empty and nothing is released, the drip is not queueing: read the taskworker's extender publish chain. Then read §2.19c's stale-directories condition and §28.2 for the other channels.",
			verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads at least %.2f of the online extenders released in the day.", settings.CoverageHold, settings.CoverageMinShare),
			playbook: "SIGNALS.md §28.1",
		}
	})

	// the activation bootstrap: stored activations
	activatedAt := func(step int64) stormReading {
		small, known := smallPool(step)
		activated := reading("activated", step)
		switch {
		case !known || !activated.present:
			return stormReading{}
		case small:
			return stormValue(1)
		}
		return activated
	}
	activations := stormHoldOf(history.last(settings.holdSteps(settings.ActivationHold)), stormLevel{threshold: 0.5, under: true}, activatedAt)
	if !activations.complete {
		gaps = append(gaps, "activation[missing]")
	}
	activationTarget := stateTarget + "/activation"
	judge("extender-activations-stopped", activationTarget, activations, func() finding {
		return finding{
			probeId: extenderReleaseProbeId, tier: tierWarn, class: "extender-activations-stopped", target: activationTarget, sustain: 1,
			symptom:   fmt.Sprintf("No extender activation was stored in any trailing %s, at every evaluation for %s", settings.ActivationWindow, settings.ActivationHold),
			mechanism: "Extenders activate several times a day each (about 4.4 on 2026-11-11), and every stored activation answers its extender with its own fresh record and eight random peers. With none stored, no new extender joins, no extender renews the record it holds, and the bootstrap sample that kept extender directories partly fresh through the 2026-11-06 gossip stall stops too.",
			baseline:  fmt.Sprintf("At least 7 stored activations in every trailing hour from 2026-11-03 to 11-11 (about 39 an hour). WARN at none in a trailing %s for %s, with at least %d online extenders.", settings.ActivationWindow, settings.ActivationHold, settings.MinOnlineExtenders),
			observed: fmt.Sprintf("stored_activations_%s=%s activation_posts_%s=%s online_extenders=%s evaluation_unix=%d",
				stormDuration(settings.ActivationWindow), stormNumber(reading("activated", at), "%.1f"),
				stormDuration(settings.ActivationWindow), stormNumber(reading("activation_posts", at), "%.1f"),
				stormNumber(reading("online", at), "%.0f"),
				at,
			),
			evidence: fmt.Sprintf("One bounded Mimir range read of increase(urnetwork_extender_activations_total[%s]) summed over the api processes, stage store, outcome activated, and over every stage.", stormDuration(settings.ActivationWindow)),
			context:  "activation_posts counts every answer of POST /network/extender-activate by any stage. Posts with none stored point at the api's checks (config, budget, tcp, forward, store); no posts at all point at the extenders or the path to the api.",
			action:   "Read the extenders dashboard's activation outcomes by stage. config error: the operator's extender configuration; tcp or forward errors and timeouts from every account at once: the api's outbound probe path; store errors: Main; budget or busy: the activation budgets. No posts at all: the api route and the lb, then the released extender builds.",
			verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads a stored activation in its trailing %s.", settings.ActivationHold, settings.ActivationWindow),
			playbook: "SIGNALS.md §28.1",
		}
	})

	// gated release: answers that released something, while clients ask
	releaseAt := func(step int64) stormReading {
		answers, ok := reading("release_answers", step), reading("release_ok", step)
		switch {
		case !answers.present || !ok.present:
			return stormReading{}
		case answers.value < settings.ReleaseMinAnswers:
			// nobody asked, so the channel failed nobody
			return stormValue(1)
		}
		return stormValue(ok.value / answers.value)
	}
	release := stormHoldOf(history.last(settings.holdSteps(settings.ReleaseHold)), stormLevel{threshold: settings.ReleaseMinOkShare, under: true}, releaseAt)
	if !release.complete {
		gaps = append(gaps, "gated[missing]")
	}
	gatedTarget := stateTarget + "/gated"
	judge("extender-gated-release-failing", gatedTarget, release, func() finding {
		return finding{
			probeId: extenderReleaseProbeId, tier: tierWarn, class: "extender-gated-release-failing", target: gatedTarget, sustain: 1,
			symptom:   fmt.Sprintf("Under %.0f%% of the gated release requests in the trailing %s released a record, at every evaluation for %s", 100*settings.ReleaseMinOkShare, settings.ReleaseWindow, settings.ReleaseHold),
			mechanism: "A client that cannot reach the open channels asks the api for gated records (POST /network/extender-release, connect/EXTENDER.md R3). An answer that releases nothing, because no gated record is eligible, every eligible one is at its country's limit, the records need a capability the client did not declare, or the ledger is unavailable, leaves that client with no extender it can dial.",
			baseline:  fmt.Sprintf("No requests at all through 2026-11-11: the gated tier had no members. WARN when at least %.0f answers that could have released anything in the trailing %s released under %.2f of the time, for %s. Rate limits, spent epochs and probation refuse a requester by design and are left out.", settings.ReleaseMinAnswers, settings.ReleaseWindow, settings.ReleaseMinOkShare, settings.ReleaseHold),
			observed: fmt.Sprintf("release_ok_%s=%s release_answers_%s=%s ok_share=%s evaluation_unix=%d",
				stormDuration(settings.ReleaseWindow), stormNumber(reading("release_ok", at), "%.1f"),
				stormDuration(settings.ReleaseWindow), stormNumber(reading("release_answers", at), "%.1f"),
				stormNumber(releaseAt(at), "%.2f"),
				at,
			),
			evidence: fmt.Sprintf("One bounded Mimir range read of increase(urnetwork_extender_releases_total[%s]) summed over the api processes, by outcome.", stormDuration(settings.ReleaseWindow)),
			context:  "The outcomes are empty (nothing eligible), country_capacity, incompatible, unavailable and ok; urnetwork_extender_release_country_outcomes_total splits them by the caller's country.",
			action:   "Read the release outcomes on the extenders dashboard. empty: the gated tier has no eligible member, so add gated extenders or stop clients asking; country_capacity: the per-country limit; incompatible: the clients' declared capabilities against the records' requirements; unavailable: the api's extender configuration and the release ledger on Main.",
			verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads at least %.2f of the answers releasing a record, or fewer than %.0f answers.", settings.ReleaseHold, settings.ReleaseMinOkShare, settings.ReleaseMinAnswers),
			playbook: "SIGNALS.md §28.1",
		}
	})

	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(extenderReleaseProbeId, tierWarn, "extender-release-unobservable", stateTarget))
	}
	return findings, nil
}
