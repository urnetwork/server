package monitor

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §28.6 maps to signal_extender_webrtc_relay.go and
// signal_extender_webrtc_relay_test.go. Whether the exchange's half of the
// webrtc extender carrier's signaling relay (connect/EXTENDER.md S8) gets
// answers from the extender clients it relays the operator's offers to:
//
//   - no answer: the share of the offers the exchanges took in the trailing
//     hour that their client did not answer before the offer's deadline, or
//     answered with a failure. The prober offers only to extenders whose
//     builds serve the carrier, so a high share is the clients' answerer or
//     the relay path broken, and the webrtc tests of those extenders fail;
//   - busy: the share the exchanges refused because they were already
//     relaying as many offers as they take at once (MaxConcurrentRelays),
//     which the prober's own bounds keep far from.
//
// Both read urnetwork_extender_webrtc_relay_offers_total, which every connect
// process exports with each outcome at zero from its start, summed over the
// processes by outcome. Too few offers to judge is a quiet relay, not an
// unknown one. A relay that takes no offer at all counts nothing, so it is
// quiet here too; the prober's own webrtc failures show it. Every part is one
// fleet-wide value; no client, extender or process reaches an alert.
func NewExtenderWebRtcRelaySignal() Signal {
	return NewExtenderWebRtcRelaySignalWithSettings(DefaultExtenderWebRtcRelaySettings())
}

// The signal with explicit thresholds.
func NewExtenderWebRtcRelaySignalWithSettings(settings *ExtenderWebRtcRelaySettings) Signal {
	return &signalAdapter{
		number: "28.6", key: "extender-webrtc-relay", name: "Extender webrtc relay",
		probe: extenderWebRtcRelayProbe{settings: settings},
	}
}

const extenderWebRtcRelayProbeId = "mimir/extender-webrtc-relay"

// The thresholds and bounds of §28.6.
type ExtenderWebRtcRelaySettings struct {
	// the trailing window the offers are counted over
	Window time.Duration
	// fewer offers than this in the window are too few to judge, and read as
	// within every level
	MinOffers float64
	// no answer: more than NoAnswerShare of the window's offers unanswered,
	// at every evaluation of NoAnswerHold
	NoAnswerShare float64
	NoAnswerHold  time.Duration
	// busy: more than BusyShare of the window's offers refused busy, at every
	// evaluation of BusyHold
	BusyShare float64
	BusyHold  time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §28.6. The relay has no production history, so
// these are first values, to revisit after its first week of data.
func DefaultExtenderWebRtcRelaySettings() *ExtenderWebRtcRelaySettings {
	read := DefaultStormReadSettings()
	read.Cadence = 5 * time.Minute
	return &ExtenderWebRtcRelaySettings{
		Window:            time.Hour,
		MinOffers:         20,
		NoAnswerShare:     0.5,
		NoAnswerHold:      30 * time.Minute,
		BusyShare:         0.05,
		BusyHold:          30 * time.Minute,
		MaxListed:         8,
		StormReadSettings: read,
	}
}

func (self *ExtenderWebRtcRelaySettings) valid() bool {
	return self != nil && self.Window >= time.Minute && self.Window%time.Minute == 0 &&
		0 < self.MinOffers && 0 < self.NoAnswerShare && self.NoAnswerShare < 1 &&
		0 < self.BusyShare && self.BusyShare < 1 && 0 < self.MaxListed &&
		self.StormReadSettings.valid(self.NoAnswerHold, self.BusyHold)
}

type extenderWebRtcRelayProbe struct {
	settings *ExtenderWebRtcRelaySettings
}

func (extenderWebRtcRelayProbe) id() string   { return extenderWebRtcRelayProbeId }
func (extenderWebRtcRelayProbe) tier() string { return tierWarn }

func (self extenderWebRtcRelayProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self extenderWebRtcRelayProbe) effectiveSettings() *ExtenderWebRtcRelaySettings {
	if self.settings == nil {
		return DefaultExtenderWebRtcRelaySettings()
	}
	return self.settings
}

// The longest hold, which is how many evaluations one read covers.
func (self *ExtenderWebRtcRelaySettings) readSteps() int {
	return self.holdSteps(max(self.NoAnswerHold, self.BusyHold))
}

// The outcomes connect counts each offer its residents took by (server
// connect/extender_webrtc_relay.go), in the order an alert lists them. A
// connect process exports each at zero from its start, so every one answers
// whenever the counter does.
var extenderWebRtcRelayOutcomes = []string{"answered", "no_answer", "expired", "rate_limited", "busy", "oversized", "malformed"}

// §28.6's expression, tagged with its part: the window's offers by outcome,
// summed over the connect processes.
func extenderWebRtcRelayQuery(environment string, settings *ExtenderWebRtcRelaySettings) string {
	return fleetPart("offers", "sum by (outcome) (increase(urnetwork_extender_webrtc_relay_offers_total{env="+strconv.Quote(environment)+"}["+stormDuration(settings.Window)+"]))")
}

var extenderWebRtcRelayShapes = map[string]stormShape{
	"offers": {keys: []string{"outcome"}},
}

func (self extenderWebRtcRelayProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	relayTarget := env.cfg.env + "/extender-webrtc-relay"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(extenderWebRtcRelayProbeId, "extender-webrtc-relay-unobservable", relayTarget, "§28.6", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, extenderWebRtcRelayQuery(env.cfg.env, settings), extenderWebRtcRelayShapes, settings.readSteps(), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}
	reasons := history.invalidReasons()
	answered := history.keys("offers")
	if len(answered) == 0 {
		// no connect process exported the relay's counter: a build that
		// predates S8, or none running, so neither share can be judged
		return []finding{unobservable(append(reasons, "relay_state_missing"), nil)}, nil
	}
	// every outcome connect exports, then any a newer build added, which
	// counts in the total too
	outcomes := slices.Clone(extenderWebRtcRelayOutcomes)
	for _, outcome := range answered {
		if !slices.Contains(outcomes, outcome) {
			outcomes = append(outcomes, outcome)
		}
	}
	// an outcome without a value at an evaluation is a gap in both shares,
	// since it would misstate the total
	gaps := []string{}
	for _, outcome := range outcomes {
		for _, step := range history.last(settings.readSteps()) {
			if !history.reading("offers", outcome, step).present {
				gaps = append(gaps, outcome+"[missing]")
				break
			}
		}
	}
	at := history.at()
	findings := []finding{}
	judge := func(class string, target string, hold stormHold, incident func() finding) {
		switch {
		case hold.held:
			findings = append(findings, incident())
		case hold.clear:
			findings = append(findings, healthyFinding(extenderWebRtcRelayProbeId, tierWarn, class, target))
		default:
			findings = append(findings, stormClearHold(extenderWebRtcRelayProbeId, class, target))
		}
	}
	// the window's offers at a step, every outcome
	totalAt := func(step int64) stormReading {
		total := 0.0
		for _, outcome := range outcomes {
			count := history.reading("offers", outcome, step)
			if !count.present {
				return stormReading{}
			}
			total += count.value
		}
		return stormValue(total)
	}
	// an outcome's share of the window's offers at a step
	ratioAt := func(outcome string, step int64) stormReading {
		total := totalAt(step)
		if !total.present || total.value <= 0 {
			return stormReading{}
		}
		return stormValue(history.reading("offers", outcome, step).value / total.value)
	}
	// the share a hold judges: too few offers read as within every level, a
	// relay that failed nobody in numbers worth judging
	shareAt := func(outcome string) func(step int64) stormReading {
		return func(step int64) stormReading {
			total := totalAt(step)
			switch {
			case !total.present:
				return stormReading{}
			case total.value < settings.MinOffers:
				return stormValue(0)
			}
			return ratioAt(outcome, step)
		}
	}
	// the window's offers by outcome at the evaluation instant, with both
	// shares; outcomes a newer connect build added are summed, unnamed
	relayObserved := func() string {
		fields := []string{"window=" + stormDuration(settings.Window), "offers=" + stormNumber(totalAt(at), "%.0f")}
		for _, outcome := range extenderWebRtcRelayOutcomes {
			fields = append(fields, outcome+"="+stormNumber(history.reading("offers", outcome, at), "%.0f"))
		}
		if len(extenderWebRtcRelayOutcomes) < len(outcomes) {
			other := stormValue(0)
			for _, outcome := range outcomes[len(extenderWebRtcRelayOutcomes):] {
				count := history.reading("offers", outcome, at)
				if !count.present {
					other = stormReading{}
					break
				}
				other.value += count.value
			}
			fields = append(fields, "other_outcomes="+stormNumber(other, "%.0f"))
		}
		return strings.Join(append(fields,
			"no_answer_share="+stormNumber(ratioAt("no_answer", at), "%.2f"),
			"busy_share="+stormNumber(ratioAt("busy", at), "%.2f"),
			fmt.Sprintf("evaluation_unix=%d", at),
		), " ")
	}
	evidence := fmt.Sprintf("One bounded Mimir range read of increase(urnetwork_extender_webrtc_relay_offers_total[%s]) summed over the connect processes by outcome; every connect process exports each outcome at zero from its start.", stormDuration(settings.Window))

	noAnswer := stormHoldOf(history.last(settings.holdSteps(settings.NoAnswerHold)), stormLevel{threshold: settings.NoAnswerShare}, shareAt("no_answer"))
	judge("extender-webrtc-relay-no-answer-high", relayTarget, noAnswer, func() finding {
		return finding{
			probeId: extenderWebRtcRelayProbeId, tier: tierWarn, class: "extender-webrtc-relay-no-answer-high", target: relayTarget, sustain: 1,
			symptom:   fmt.Sprintf("More than %.0f%% of the webrtc extender offers the exchanges took in the trailing %s went unanswered, with at least %.0f offers, at every evaluation for %s", 100*settings.NoAnswerShare, settings.Window, settings.MinOffers, settings.NoAnswerHold),
			mechanism: "The operator's prober (the api's activation probe and the taskworker's uptime probe) posts a webrtc offer to the extender client's Redis offer set. The connect process holding the client's resident takes it and relays it to the client as exchange signals, the client's extender answerer answers on the same exchange, and the resident posts the answer for the prober. The prober offers only to extenders whose builds serve the carrier, so an unanswered offer is an answerer that did not answer, or an offer or answer lost between the resident and the client. Each fails its probe's webrtc test: an activation only webrtc could pass is refused, and an address accepted on webrtc alone is deactivated after six failed uptime probes in a row.",
			baseline:  fmt.Sprintf("No history: the relay first runs with S8's connect build. Expected: nearly every offer answered, since the prober offers only to builds that serve the carrier. WARN above %.0f%% unanswered of at least %.0f offers in the trailing %s, at every evaluation for %s: first values, to revisit after the first week of data.", 100*settings.NoAnswerShare, settings.MinOffers, settings.Window, settings.NoAnswerHold),
			observed:  relayObserved(),
			evidence:  evidence,
			context:   "no_answer counts an offer whose client sent no answer before the offer's deadline, or whose answer failed. expired, rate_limited, busy, oversized and malformed are refusals before the client, counted in the total, so read extender-webrtc-relay-busy-high first. An extender whose build has no answerer does not offer the carrier, so the prober never sends it an offer; one whose network loses the exchange's signals still reads as no_answer.",
			action:    "Read the extenders dashboard's webrtc relay row beside the webrtc step of its activation panels, and when the share rose: with a connect deploy, the relay; with an sdk release, the answerer. Then check, in order: the sdk's extender answerer (connect net_extender_webrtc_signal.go) in the builds the prober offers to; the resident relay (server connect/extender_webrtc_relay.go), whose `[wr]<client> no answer` lines at log verbosity 1 name each failed exchange's error; and the exchange-to-client control path the offer travels as exchange signals, from the resident's internal client to the client's manager, which hands flagged offers to the answerer.",
			verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads at most %.0f%% of the offers in the trailing %s unanswered, or fewer than %.0f offers.", settings.NoAnswerHold, 100*settings.NoAnswerShare, settings.Window, settings.MinOffers),
			playbook:  "SIGNALS.md §28.6",
		}
	})

	busy := stormHoldOf(history.last(settings.holdSteps(settings.BusyHold)), stormLevel{threshold: settings.BusyShare}, shareAt("busy"))
	judge("extender-webrtc-relay-busy-high", relayTarget, busy, func() finding {
		return finding{
			probeId: extenderWebRtcRelayProbeId, tier: tierWarn, class: "extender-webrtc-relay-busy-high", target: relayTarget, sustain: 1,
			symptom:   fmt.Sprintf("More than %.0f%% of the webrtc extender offers the exchanges took in the trailing %s were refused busy, with at least %.0f offers, at every evaluation for %s", 100*settings.BusyShare, settings.Window, settings.MinOffers, settings.BusyHold),
			mechanism: "Each connect process relays at most MaxConcurrentRelays offers at once over all its residents (64 by default), each holding an exchange with its client for the seconds its answer takes. An offer past the bound is answered busy at once rather than queued, and its probe's webrtc test fails. The prober holds far fewer open: the uptime pass probes 16 addresses at a time per taskworker, and a client may have at most two offers pending. A lasting busy share is either the prober sending far more offers than that (a burst of activations: each api process probes up to 64 at once), or relay slots that stay held after their exchange ends.",
			baseline:  fmt.Sprintf("No history: the relay first runs with S8's connect build. Expected: about zero, since the prober holds far fewer offers open than the bound. WARN above %.0f%% busy of at least %.0f offers in the trailing %s, at every evaluation for %s: first values, to revisit after the first week of data.", 100*settings.BusyShare, settings.MinOffers, settings.Window, settings.BusyHold),
			observed:  relayObserved(),
			evidence:  evidence,
			context:   "busy is the relay's own bound, not the extenders'. A busy refusal fails its probe's webrtc test, so a lasting share refuses activations only webrtc could pass and, after six failed uptime probes in a row, deactivates addresses accepted on webrtc alone. The share is fleet-wide: one process refusing all its offers shows smaller here among many.",
			action: fmt.Sprintf("Find what fills the relay before raising MaxConcurrentRelays, which bounds how many exchanges one connect process holds open with its clients. Find the process: sum by (host, block) (increase(urnetwork_extender_webrtc_relay_offers_total{env=%s,outcome=\"busy\"}[%s])) against the same over every outcome. Many offers there: a prober sending more than it should (a burst of activations, or an uptime pass grown with the fleet), which the relay is right to refuse; slow the prober. Few offers with busy still high: slots held by exchanges that outlive their offer's deadline, a relay fault in server connect/extender_webrtc_relay.go. Raise the bound only for load an exchange should carry.",
				strconv.Quote(env.cfg.env), stormDuration(settings.Window)),
			verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads at most %.0f%% of the offers in the trailing %s refused busy, or fewer than %.0f offers.", settings.BusyHold, 100*settings.BusyShare, settings.Window, settings.MinOffers),
			playbook: "SIGNALS.md §28.6",
		}
	})

	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(extenderWebRtcRelayProbeId, tierWarn, "extender-webrtc-relay-unobservable", relayTarget))
	}
	return findings, nil
}
