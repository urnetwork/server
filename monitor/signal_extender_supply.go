package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §28.5 maps to signal_extender_supply.go and
// signal_extender_supply_test.go. How many extenders there are, and how fast
// the directory loses them (factury/research/extender-health-2026-11-11.md
// §1.1, §4 item 5):
//
//   - a fall: the online extenders well under their peak of the trailing day.
//     On 2026-11-01 the uptime probe's own failures (157 of 199 checks)
//     revoked about 136 of 307 extenders in half a day, and nothing reported
//     it;
//   - churn: more extenders revoked than created in the trailing day, by a
//     margin. From 11-02 to 11-11 about 20 were revoked and 11 created a day,
//     75% of the revoked within 72 hours of their creation, so the fleet
//     shrank while the online count moved slowly.
//
// The prober's common-mode guard and its false-revocation counters are
// fix/extender-churn-1111's; this signal reads the directory's own counts,
// which hold whatever revoked an extender. Every part is one fleet-wide value.
func NewExtenderSupplySignal() Signal {
	return NewExtenderSupplySignalWithSettings(DefaultExtenderSupplySettings())
}

// The signal with explicit thresholds.
func NewExtenderSupplySignalWithSettings(settings *ExtenderSupplySettings) Signal {
	return &signalAdapter{
		number: "28.5", key: "extender-supply", name: "Extender supply and churn",
		probe: extenderSupplyProbe{settings: settings},
	}
}

const extenderSupplyProbeId = "mimir/extender-supply"

// The thresholds and bounds of §28.5.
type ExtenderSupplySettings struct {
	// falling: the online extenders more than FallShare under their peak over
	// PeakWindow, and at least FallMinLoss under it, at every evaluation of
	// FallHold
	PeakWindow  time.Duration
	FallShare   float64
	FallMinLoss float64
	FallHold    time.Duration
	// churn: more than ChurnExcess more extenders revoked than created in the
	// trailing day, at every evaluation of ChurnHold
	ChurnExcess float64
	ChurnHold   time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §28.5.
func DefaultExtenderSupplySettings() *ExtenderSupplySettings {
	read := DefaultStormReadSettings()
	read.Cadence = 5 * time.Minute
	return &ExtenderSupplySettings{
		PeakWindow:        24 * time.Hour,
		FallShare:         0.25,
		FallMinLoss:       20,
		FallHold:          60 * time.Minute,
		ChurnExcess:       30,
		ChurnHold:         120 * time.Minute,
		MaxListed:         8,
		StormReadSettings: read,
	}
}

func (self *ExtenderSupplySettings) valid() bool {
	return self != nil && self.PeakWindow >= time.Minute && self.PeakWindow%time.Minute == 0 &&
		0 < self.FallShare && self.FallShare < 1 && 0 < self.FallMinLoss && 0 < self.ChurnExcess &&
		0 < self.MaxListed && self.StormReadSettings.valid(self.FallHold, self.ChurnHold)
}

type extenderSupplyProbe struct {
	settings *ExtenderSupplySettings
}

func (extenderSupplyProbe) id() string   { return extenderSupplyProbeId }
func (extenderSupplyProbe) tier() string { return tierWarn }

func (self extenderSupplyProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self extenderSupplyProbe) effectiveSettings() *ExtenderSupplySettings {
	if self.settings == nil {
		return DefaultExtenderSupplySettings()
	}
	return self.settings
}

// The longest hold, which is how many evaluations one read covers.
func (self *ExtenderSupplySettings) readSteps() int {
	return self.holdSteps(max(self.FallHold, self.ChurnHold))
}

// §28.5's expressions, each tagged with its part.
func extenderSupplyQuery(environment string, settings *ExtenderSupplySettings) string {
	env := "{env=" + strconv.Quote(environment) + "}"
	window := "[" + stormDuration(settings.PeakWindow) + "]"
	return strings.Join([]string{
		fleetPart("online", "max(urnetwork_stats_online_extenders"+env+")"),
		fleetPart("online_peak", "max(max_over_time(urnetwork_stats_online_extenders"+env+window+"))"),
		fleetPart("created", "max(urnetwork_stats_extenders_created_24h"+env+")"),
		fleetPart("revoked", "max(urnetwork_stats_extenders_revoked_24h"+env+")"),
		fleetPart("activated", "sum(increase(urnetwork_extender_activations_total{env="+strconv.Quote(environment)+`,stage="store",outcome="activated"}[24h]))`),
	}, " or ")
}

var extenderSupplyShapes = map[string]stormShape{
	"online":      {},
	"online_peak": {},
	"created":     {},
	"revoked":     {},
	"activated":   {},
}

func (self extenderSupplyProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	supplyTarget := env.cfg.env + "/extender-supply"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(extenderSupplyProbeId, "extender-supply-unobservable", supplyTarget, "§28.5", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, extenderSupplyQuery(env.cfg.env, settings), extenderSupplyShapes, settings.readSteps(), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}
	reasons := history.invalidReasons()
	gaps := []string{}
	at := history.at()
	findings := []finding{}
	reading := func(part string, step int64) stormReading {
		return history.reading(part, "", step)
	}
	judge := func(class string, target string, hold stormHold, incident func() finding) {
		switch {
		case hold.held:
			findings = append(findings, incident())
		case hold.clear:
			findings = append(findings, healthyFinding(extenderSupplyProbeId, tierWarn, class, target))
		default:
			findings = append(findings, stormClearHold(extenderSupplyProbeId, class, target))
		}
	}
	churnObserved := func() string {
		return fmt.Sprintf("online_extenders=%s peak_%s=%s created_24h=%s revoked_24h=%s stored_activations_24h=%s evaluation_unix=%d",
			stormNumber(reading("online", at), "%.0f"),
			stormDuration(settings.PeakWindow), stormNumber(reading("online_peak", at), "%.0f"),
			stormNumber(reading("created", at), "%.0f"),
			stormNumber(reading("revoked", at), "%.0f"),
			stormNumber(reading("activated", at), "%.0f"),
			at,
		)
	}

	if !history.answered("online", "") {
		// no taskworker's stats collector exported the online pool
		reasons = append(reasons, "extender_state_missing")
	} else {
		fallAt := func(step int64) stormReading {
			online, peak := reading("online", step), reading("online_peak", step)
			switch {
			case !online.present || !peak.present:
				return stormReading{}
			case peak.value-online.value < settings.FallMinLoss || peak.value <= 0:
				return stormValue(0)
			}
			return stormValue((peak.value - online.value) / peak.value)
		}
		fall := stormHoldOf(history.last(settings.holdSteps(settings.FallHold)), stormLevel{threshold: settings.FallShare}, fallAt)
		if !fall.complete {
			gaps = append(gaps, "online[missing]")
		}
		judge("extender-supply-falling", supplyTarget, fall, func() finding {
			return finding{
				probeId: extenderSupplyProbeId, tier: tierWarn, class: "extender-supply-falling", target: supplyTarget, sustain: 1,
				symptom:   fmt.Sprintf("Online extenders are more than %.0f%% under their %s peak, by at least %.0f, at every evaluation for %s", 100*settings.FallShare, settings.PeakWindow, settings.FallMinLoss, settings.FallHold),
				mechanism: "An extender is online while one of its addresses is active, and six consecutive failed uptime probes deactivate an address; only a new activation brings a revoked extender back. A fall this fast is many extenders failing their probes at once: the prober itself (on 2026-11-01, 157 of 199 checks failed from the prober's side and about 136 of 307 extenders were revoked in half a day), a network event on the extenders' side, or a release that broke the extender role.",
				baseline:  fmt.Sprintf("157-214 online from 2026-11-02 to 11-11, moving a few a day. WARN more than %.0f%% and %.0f under the %s peak for %s.", 100*settings.FallShare, settings.FallMinLoss, settings.PeakWindow, settings.FallHold),
				observed:  churnObserved(),
				evidence:  fmt.Sprintf("One bounded Mimir range read of urnetwork_stats_online_extenders against its maximum over %s, with the directory's created and revoked counts of the day.", settings.PeakWindow),
				context:   "Revoked extenders whose hosts still run re-activate within hours, so a prober fault shows here as a fall and then a recovery, and in the revoked count, which drops as they return.",
				action:    "Read the taskworker's extender probe sweeps (\"N ok\" lines of extender_probe_work.go) and, on a build with fix/extender-churn-1111, urnetwork_taskworker_extender_probe_passes_total{verdict=\"common_mode\"} and the control target: failures spread over every network at once are the prober's. Failures in a few networks are theirs. Then the activation outcomes (§28.1) for extenders coming back.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the online extenders within %.0f%% of their %s peak.", settings.FallHold, 100*settings.FallShare, settings.PeakWindow),
				playbook:  "SIGNALS.md §28.5",
			}
		})
	}

	if !history.answered("created", "") || !history.answered("revoked", "") {
		// a taskworker build that predates the supply gauges
		reasons = append(reasons, "churn_state_missing")
	} else {
		churnAt := func(step int64) stormReading {
			created, revoked := reading("created", step), reading("revoked", step)
			if !created.present || !revoked.present {
				return stormReading{}
			}
			return stormValue(revoked.value - created.value)
		}
		churn := stormHoldOf(history.last(settings.holdSteps(settings.ChurnHold)), stormLevel{threshold: settings.ChurnExcess}, churnAt)
		if !churn.complete {
			gaps = append(gaps, "churn[missing]")
		}
		churnTarget := env.cfg.env + "/extender-churn"
		judge("extender-churn-high", churnTarget, churn, func() finding {
			return finding{
				probeId: extenderSupplyProbeId, tier: tierWarn, class: "extender-churn-high", target: churnTarget, sustain: 1,
				symptom:   fmt.Sprintf("More than %.0f more extenders were revoked than created in the trailing day, at every evaluation for %s", settings.ChurnExcess, settings.ChurnHold),
				mechanism: "The directory gains an extender when a new key activates and loses one when its last address fails six uptime probes in a row. A revoked extender that activates again is no longer counted as revoked, so this is the supply the day lost for good. The excess of revocations over creations is the fleet shrinking whatever the online count's moment-to-moment level.",
				baseline:  fmt.Sprintf("About 20 revoked and 11 created a day from 2026-11-02 to 11-11, an excess of about 9. WARN at an excess above %.0f for %s.", settings.ChurnExcess, settings.ChurnHold),
				observed:  churnObserved(),
				evidence:  "One bounded Mimir range read of urnetwork_stats_extenders_created_24h and urnetwork_stats_extenders_revoked_24h, which the taskworkers' stats collectors count from network_extender, merged by max.",
				context:   "Released miners run the extender role on every slot with one key, and 75% of the revoked records of 11-02 to 11-11 died within 72 hours of creation, so a churning fleet can hold its online count while it cycles keys.",
				action:    "Read the extender probe outcomes as for extender-supply-falling. Revocations of extenders whose hosts answer again soon after are false (fix/extender-churn-1111's urnetwork_taskworker_extender_probe_restores_total counts them); revocations that stay are hosts that left.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads an excess of revocations at or under %.0f.", settings.ChurnHold, settings.ChurnExcess),
				playbook:  "SIGNALS.md §28.5",
			}
		})
	}

	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, gaps))
	} else {
		findings = append(findings, healthyFinding(extenderSupplyProbeId, tierWarn, "extender-supply-unobservable", supplyTarget))
	}
	return findings, nil
}
