package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.10 maps to signal_contract_reply_collapse.go and
// signal_contract_reply_collapse_test.go. The hosted proxy's contract replies
// collapse when its creations stop returning: in incident B they fell to
// 20-28/s against a two-hour median of 205/s within 13 minutes of onset. A
// day-earlier or seven-day reference does not fit this series (its replies
// grew from about 45/s to 250-420/s in a week), so the reference is the proxy's own median
// over the previous two hours, offset ten minutes so the collapse does not
// pull its own reference down. The probe reads the ratio at each minute of its
// hold. Without replies in its reference there is nothing to collapse from,
// so a zero median reads within the band.
func NewContractReplyCollapseSignal() Signal {
	return NewContractReplyCollapseSignalWithSettings(DefaultContractReplyCollapseSettings())
}

// The signal with explicit thresholds.
func NewContractReplyCollapseSignalWithSettings(settings *ContractReplyCollapseSettings) Signal {
	return &signalAdapter{
		number: "26.10", key: "contract-reply-collapse", name: "Proxy contract replies against their previous two hours",
		probe: contractReplyCollapseProbe{settings: settings},
	}
}

const contractReplyCollapseProbeId = "mimir/contract-reply-collapse"

// The thresholds and bounds of §26.10.
type ContractReplyCollapseSettings struct {
	// the service whose replies are read
	Service string
	// the rate window of each evaluation
	Window time.Duration
	// the reference: the median of the reply rate over BaselineSpan at
	// BaselineStep, ending BaselineOffset before each evaluation
	BaselineSpan   time.Duration
	BaselineStep   time.Duration
	BaselineOffset time.Duration
	// replies under PageRatio of the reference for Hold page
	PageRatio float64
	Hold      time.Duration
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.10.
func DefaultContractReplyCollapseSettings() *ContractReplyCollapseSettings {
	return &ContractReplyCollapseSettings{
		Service:           "proxy",
		Window:            5 * time.Minute,
		BaselineSpan:      2 * time.Hour,
		BaselineStep:      5 * time.Minute,
		BaselineOffset:    10 * time.Minute,
		PageRatio:         0.25,
		Hold:              10 * time.Minute,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *ContractReplyCollapseSettings) valid() bool {
	wholeSeconds := func(duration time.Duration) bool { return duration >= time.Second && duration%time.Second == 0 }
	return self != nil && self.Service != "" && wholeSeconds(self.Window) && wholeSeconds(self.BaselineSpan) &&
		wholeSeconds(self.BaselineStep) && self.BaselineStep <= self.BaselineSpan && 0 <= self.BaselineOffset && self.BaselineOffset%time.Second == 0 &&
		0 < self.PageRatio && self.PageRatio < 1 && self.StormReadSettings.valid(self.Hold)
}

type contractReplyCollapseProbe struct {
	settings *ContractReplyCollapseSettings
}

func (contractReplyCollapseProbe) id() string   { return contractReplyCollapseProbeId }
func (contractReplyCollapseProbe) tier() string { return tierPage }

func (self contractReplyCollapseProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self contractReplyCollapseProbe) effectiveSettings() *ContractReplyCollapseSettings {
	if self.settings == nil {
		return DefaultContractReplyCollapseSettings()
	}
	return self.settings
}

// §26.10's expression, and its numerator and reference alone.
func contractReplyCollapseQuery(environment string, settings *ContractReplyCollapseSettings) string {
	replies := "sum(rate(urnetwork_contract_creation_completed_total{env=" + strconv.Quote(environment) + ",service=" + strconv.Quote(settings.Service) + `,outcome="contract_reply"}[` + stormDuration(settings.Window) + "]))"
	reference := "quantile_over_time(0.5, " + replies + "[" + stormDuration(settings.BaselineSpan) + ":" + stormDuration(settings.BaselineStep) + "] offset " + stormDuration(settings.BaselineOffset) + ")"
	return strings.Join([]string{
		fleetPart("ratio", replies+" / "+reference),
		fleetPart("replies", replies),
		fleetPart("reference", reference),
	}, " or ")
}

var contractReplyCollapseShapes = map[string]stormShape{
	"ratio":     {},
	"replies":   {},
	"reference": {},
}

func (self contractReplyCollapseProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/contract-reply-collapse"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(contractReplyCollapseProbeId, "contract-reply-collapse-unobservable", unobservableTarget, "§26.10", reasons, gaps, 8)
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, contractReplyCollapseQuery(env.cfg.env, settings), contractReplyCollapseShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{unobservable([]string{stormReadReason(err)}, nil)}, nil
	}

	ratioAt := func(step int64) stormReading {
		if ratio := history.reading("ratio", "", step); ratio.present {
			return ratio
		}
		// no replies in the reference: nothing to collapse from
		if reference := history.reading("reference", "", step); reference.present && reference.value == 0 {
			return stormValue(1)
		}
		return stormReading{}
	}
	hold := stormHoldOf(history.last(settings.holdSteps(settings.Hold)), stormLevel{threshold: settings.PageRatio, under: true}, ratioAt)
	reasons := history.invalidReasons()
	if !hold.complete {
		reasons = append(reasons, "evaluation_missing")
	}
	target := env.cfg.env + "/" + settings.Service
	findings := []finding{}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, []string{settings.Service + "[missing]"}))
	} else {
		findings = append(findings, healthyFinding(contractReplyCollapseProbeId, tierWarn, "contract-reply-collapse-unobservable", unobservableTarget))
	}
	switch {
	case hold.held:
		findings = append(findings, finding{
			probeId: contractReplyCollapseProbeId, tier: tierPage, class: "contract-reply-collapse", target: target, sustain: 1,
			symptom:   fmt.Sprintf("The %s's contract replies stayed under %.0f%% of their median over the previous %s at every evaluation for %s", settings.Service, 100*settings.PageRatio, settings.BaselineSpan, settings.Hold),
			mechanism: "A reply is a contract creation that returned a contract. When creations stop returning, replies collapse while the stuck calls are still in flight. In incident B (2026-10-26) the hosted proxy's replies fell to 27.7/s against a two-hour median of 205/s by 14:56Z, 13 minutes after onset.",
			baseline:  fmt.Sprintf("Replies near their own recent median; the hosted proxy grew from about 45 replies/s on 2026-10-20 to 250-420/s on the morning of 10-26. PAGE when replies stay under %.0f%% of the median of the previous %s (at %s steps, offset %s) at every one-minute evaluation for %s.", 100*settings.PageRatio, settings.BaselineSpan, settings.BaselineStep, settings.BaselineOffset, settings.Hold),
			observed:  fmt.Sprintf("ratio_now=%s replies_per_second_now=%s reference_replies_per_second=%s evaluation_unix=%d", stormNumber(ratioAt(history.at()), "%.3f"), stormNumber(history.reading("replies", "", history.at()), "%.1f"), stormNumber(history.reading("reference", "", history.at()), "%.1f"), history.at()),
			evidence:  fmt.Sprintf("One bounded Mimir range read of sum(rate(urnetwork_contract_creation_completed_total{service=%q,outcome=\"contract_reply\"}[%s])) over its quantile_over_time(0.5, ...[%s:%s] offset %s), through a services host's loopback Mimir.", settings.Service, stormDuration(settings.Window), stormDuration(settings.BaselineSpan), stormDuration(settings.BaselineStep), stormDuration(settings.BaselineOffset)),
			context:   "A proxy rollout that resets devices cuts replies too (14:22Z on 10-26, 240/s to 35-55/s as live devices fell from 22.7k to 16.8k); read rollout-guard. The median absorbs a collapse within two hours, so the stuck-creation (§26.4) and cancellation (§25.1) entries carry a long storm, and a decline slower than two hours needs §25.1's seasonal floor.",
			action:    "As §25.1: read rollout-guard, the stuck-creation entry (§26.4), the release-error and Redis entries (§26.6-§26.8), and hold rollouts before choosing a repair.",
			verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads replies at or above %.0f%% of their reference.", settings.Hold, 100*settings.PageRatio),
			playbook:  "SIGNALS.md §26.10 and §25.1",
		})
	case hold.clear:
		findings = append(findings, healthyFinding(contractReplyCollapseProbeId, tierPage, "contract-reply-collapse", target))
	default:
		findings = append(findings, stormClearHold(contractReplyCollapseProbeId, "contract-reply-collapse", target))
	}
	return findings, nil
}
