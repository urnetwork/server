package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §27.1 maps to signal_extender_seed_mirrors.go and
// signal_extender_seed_mirrors_test.go. The public extender seed bundle
// (connect/EXTENDER-UPDATE2.md §2) is how a client recovers verified reach when
// DNS bootstrap and the control route are blocked, so it must be fresh where a
// client can fetch it: the taskworker's seed chain publishes it to the origin
// store and checks every configured mirror route, and exports what each
// served (server/taskworker/work/extender_seed_metrics.go). This probe judges
// those series: a route not serving the stored bundle for an hour is stale, a
// bundle within its expiry margin is expiring, and a sequence below what the
// same source served before is a rollback, which every client refuses.
//
// The series name a mirror only by its configured name and a route by dns,
// ipv4 or ipv6; no address or host reaches an alert. The chain reports its
// state only while extender.yml enables the seed block; off is reported as
// the configured state it is.
func NewExtenderSeedMirrorsSignal() Signal {
	return NewExtenderSeedMirrorsSignalWithSettings(DefaultExtenderSeedMirrorsSettings())
}

// The signal with explicit thresholds.
func NewExtenderSeedMirrorsSignalWithSettings(settings *ExtenderSeedMirrorsSettings) Signal {
	return &signalAdapter{
		number: "27.1", key: "extender-seed-mirrors", name: "Extender seed mirrors",
		probe: extenderSeedMirrorsProbe{settings: settings},
	}
}

const extenderSeedMirrorsProbeId = "mimir/extender-seed-mirrors"

// The thresholds and bounds of §27.1.
type ExtenderSeedMirrorsSettings struct {
	// a bundle with less validity left than this at every evaluation of
	// ExpiredHold is expiring
	ExpiryMargin time.Duration
	ExpiredHold  time.Duration
	// a route not serving the stored bundle at every evaluation of StaleHold
	// is stale
	StaleHold time.Duration
	// a sequence below its own peak over RollbackWindow at every evaluation
	// of RollbackHold is a rollback
	RollbackHold   time.Duration
	RollbackWindow time.Duration
	// the most routes one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §27.1.
func DefaultExtenderSeedMirrorsSettings() *ExtenderSeedMirrorsSettings {
	read := DefaultStormReadSettings()
	read.Cadence = 5 * time.Minute
	// sixteen mirrors of three routes, five parts each, an hour of minutes
	read.MaxRows = 16384
	read.MaxResponseBytes = 4 * 1024 * 1024
	return &ExtenderSeedMirrorsSettings{
		ExpiryMargin:      6 * time.Hour,
		ExpiredHold:       10 * time.Minute,
		StaleHold:         60 * time.Minute,
		RollbackHold:      2 * time.Minute,
		RollbackWindow:    6 * time.Hour,
		MaxListed:         8,
		StormReadSettings: read,
	}
}

func (self *ExtenderSeedMirrorsSettings) valid() bool {
	return self != nil && 0 < self.ExpiryMargin && self.RollbackWindow >= time.Minute && self.RollbackWindow%time.Minute == 0 &&
		0 < self.MaxListed && self.StormReadSettings.valid(self.ExpiredHold, self.StaleHold, self.RollbackHold)
}

type extenderSeedMirrorsProbe struct {
	settings *ExtenderSeedMirrorsSettings
}

func (extenderSeedMirrorsProbe) id() string   { return extenderSeedMirrorsProbeId }
func (extenderSeedMirrorsProbe) tier() string { return tierWarn }

func (self extenderSeedMirrorsProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self extenderSeedMirrorsProbe) effectiveSettings() *ExtenderSeedMirrorsSettings {
	if self.settings == nil {
		return DefaultExtenderSeedMirrorsSettings()
	}
	return self.settings
}

// The longest hold, which is how many evaluations one read covers.
func (self *ExtenderSeedMirrorsSettings) readSteps() int {
	return self.holdSteps(max(self.ExpiredHold, self.StaleHold, self.RollbackHold))
}

// §27.1's expressions, each tagged with its part.
func extenderSeedMirrorsQuery(environment string, settings *ExtenderSeedMirrorsSettings) string {
	env := "{env=" + strconv.Quote(environment) + "}"
	window := "[" + stormDuration(settings.RollbackWindow) + "]"
	byRoute := "max by (mirror, route) "
	return strings.Join([]string{
		fleetPart("enabled", "max(urnetwork_taskworker_extender_seed_enabled"+env+")"),
		fleetPart("published", "max(urnetwork_taskworker_extender_seed_published_sequence"+env+")"),
		fleetPart("published_peak", "max(max_over_time(urnetwork_taskworker_extender_seed_published_sequence"+env+window+"))"),
		fleetPart("published_issued", "max(urnetwork_taskworker_extender_seed_published_issued_timestamp_seconds"+env+")"),
		fleetPart("published_expires", "max(urnetwork_taskworker_extender_seed_published_expires_timestamp_seconds"+env+")"),
		fleetPart("up", byRoute+"(urnetwork_taskworker_extender_seed_mirror_up"+env+")"),
		fleetPart("sequence", byRoute+"(urnetwork_taskworker_extender_seed_mirror_sequence"+env+")"),
		fleetPart("peak", byRoute+"(max_over_time(urnetwork_taskworker_extender_seed_mirror_sequence"+env+window+"))"),
		fleetPart("issued", byRoute+"(urnetwork_taskworker_extender_seed_mirror_issued_timestamp_seconds"+env+")"),
		fleetPart("expires", byRoute+"(urnetwork_taskworker_extender_seed_mirror_expires_timestamp_seconds"+env+")"),
	}, " or ")
}

var extenderSeedMirrorsShapes = map[string]stormShape{
	"enabled":           {},
	"published":         {},
	"published_peak":    {},
	"published_issued":  {},
	"published_expires": {},
	"up":                {keys: []string{"mirror", "route"}},
	"sequence":          {keys: []string{"mirror", "route"}},
	"peak":              {keys: []string{"mirror", "route"}},
	"issued":            {keys: []string{"mirror", "route"}},
	"expires":           {keys: []string{"mirror", "route"}},
}

// The parts of the stored bundle and of a mirror route.
type extenderSeedSource struct {
	// the target's name after the env: origin, or mirror/route
	name     string
	origin   bool
	key      string
	sequence string
	peak     string
	issued   string
	expires  string
}

func (self extenderSeedMirrorsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	stateTarget := env.cfg.env + "/extender-seed"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(extenderSeedMirrorsProbeId, "extender-seed-mirrors-unobservable", stateTarget, "§27.1", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, extenderSeedMirrorsQuery(env.cfg.env, settings), extenderSeedMirrorsShapes, settings.readSteps(), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}
	reasons := history.invalidReasons()

	enabled := history.reading("enabled", "", history.at())
	switch {
	case !enabled.present:
		// no taskworker exported the chain's state in the last half hour:
		// a build that predates it, or a chain that stopped running
		return []finding{unobservable(append(reasons, "seed_state_missing"), nil)}, nil
	case enabled.value == 0:
		state := finding{
			probeId: extenderSeedMirrorsProbeId, tier: tierWarn, class: "extender-seed-off", target: stateTarget,
			sustain: 1, configuredOff: true,
			symptom:   "The public extender seed bundle is off: nothing is published and no mirror is checked",
			mechanism: "The taskworker's seed chain reads the seed block of extender.yml every pass and does nothing while it is not enabled, which is the shipped default (connect/EXTENDER-UPDATE2.md §2). Mirrors keep whatever they last copied until it expires; a client refuses an expired bundle.",
			observed:  fmt.Sprintf("seed_enabled=0 evaluation_unix=%d", history.at()),
			action:    "None while the owner keeps the seed off. To start it, follow xops/main/ansible/EXTENDER-SEED-MIRRORS.md.",
			verify:    "The seed block is enabled and the chain reports seed_enabled=1.",
			playbook:  "SIGNALS.md §27.1",
		}
		return []finding{state}, nil
	}

	sources := []extenderSeedSource{{
		name: "origin", origin: true,
		sequence: "published", peak: "published_peak", issued: "published_issued", expires: "published_expires",
	}}
	routeKeys := map[string]bool{}
	for _, part := range []string{"up", "sequence", "peak", "issued", "expires"} {
		for _, key := range history.keys(part) {
			routeKeys[key] = true
		}
	}
	keys := []string{}
	for key := range routeKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sources = append(sources, extenderSeedSource{
			name: key, key: key,
			sequence: "sequence", peak: "peak", issued: "issued", expires: "expires",
		})
	}

	findings := []finding{}
	missing := []string{}
	for _, source := range sources {
		target := stateTarget + "/" + source.name
		reading := func(part string, step int64) stormReading {
			return history.reading(part, source.key, step)
		}

		// stale: not serving the stored bundle
		staleAt := func(step int64) stormReading {
			if source.origin {
				// nothing verified is stored while the chain runs enabled
				if !history.reading("enabled", "", step).present {
					return stormReading{}
				}
				if !reading(source.issued, step).present {
					return stormValue(1)
				}
				return stormValue(0)
			}
			up := reading("up", step)
			switch {
			case !up.present:
				return stormReading{}
			case up.value == 0:
				return stormValue(1)
			}
			issued, published := reading(source.issued, step), history.reading("published_issued", "", step)
			if !issued.present || !published.present {
				return stormReading{}
			}
			if issued.value < published.value {
				return stormValue(1)
			}
			return stormValue(0)
		}
		stale := stormHoldOf(history.last(settings.holdSteps(settings.StaleHold)), stormLevel{threshold: 0.5}, staleAt)

		// expiring: validity left under the margin
		expiresAt := func(step int64) stormReading {
			expires := reading(source.expires, step)
			if !expires.present {
				return stormReading{}
			}
			return stormValue(expires.value - float64(step))
		}
		expiring := stormHoldOf(history.last(settings.holdSteps(settings.ExpiredHold)), stormLevel{threshold: settings.ExpiryMargin.Seconds(), under: true}, expiresAt)

		// rollback: a sequence under its own recent peak
		rollbackAt := func(step int64) stormReading {
			sequence, peak := reading(source.sequence, step), reading(source.peak, step)
			if !sequence.present || !peak.present {
				return stormReading{}
			}
			return stormValue(peak.value - sequence.value)
		}
		rollback := stormHoldOf(history.last(settings.holdSteps(settings.RollbackHold)), stormLevel{threshold: 0}, rollbackAt)

		at := history.at()
		observed := fmt.Sprintf("source=%s served_sequence=%s origin_sequence=%s served_issued_unix=%s origin_issued_unix=%s validity_left_seconds=%s route_up=%s evaluation_unix=%d",
			source.name,
			stormNumber(reading(source.sequence, at), "%.0f"),
			stormNumber(history.reading("published", "", at), "%.0f"),
			stormNumber(reading(source.issued, at), "%.0f"),
			stormNumber(history.reading("published_issued", "", at), "%.0f"),
			stormNumber(expiresAt(at), "%.0f"),
			extenderSeedRouteUp(source, history, at),
			at,
		)

		judge := func(class string, hold stormHold, incident func() finding) {
			switch {
			case hold.held:
				findings = append(findings, incident())
			case hold.clear:
				findings = append(findings, healthyFinding(extenderSeedMirrorsProbeId, tierWarn, class, target))
			default:
				findings = append(findings, stormClearHold(extenderSeedMirrorsProbeId, class, target))
			}
		}
		judge("extender-seed-mirror-stale", stale, func() finding {
			return finding{
				probeId: extenderSeedMirrorsProbeId, tier: tierWarn, class: "extender-seed-mirror-stale", target: target, sustain: 1,
				symptom:   fmt.Sprintf("Seed source %s has not served the stored seed bundle at any evaluation for %s", source.name, settings.StaleHold),
				mechanism: "A mirror route copies the origin's bundle within minutes of each publish; one that is unreachable, serves a bundle that does not verify, or keeps an older bundle stops a cold client from recovering through it. For the origin, the chain runs enabled but no verified bundle is stored. Clients try their other mirrors, so one stale route degrades bootstrap rather than ending it.",
				baseline:  fmt.Sprintf("Every route serves the stored bundle within minutes of each publish; the origin stores a verified bundle within one pass. WARN after %s.", settings.StaleHold),
				observed:  observed,
				evidence:  "One bounded Mimir range read of the seed chain's per-route gauges (urnetwork_taskworker_extender_seed_mirror_*) and the stored bundle's (urnetwork_taskworker_extender_seed_published_*), through a services host's loopback Mimir. Routes are named by mirror name and family only.",
				context:   "A mirror is labeled by its configured name and a route by dns, ipv4 or ipv6; the url and the dial addresses are in extender.yml's seed block. The fetch outcome of each check is urnetwork_taskworker_extender_seed_mirror_checks_total.",
				action:    "Read the route's check outcomes on the extender seed dashboard. unreachable or tls: the mirror host, its certificate or its address; http_status: the mirror's web server; signature or foreign_network: the mirror serves another bundle; an older sequence: the mirror's sync from the origin (xops/main/ansible/EXTENDER-SEED-MIRRORS.md). For the origin: the publisher's outcomes.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the route up with the stored bundle's issue time.", settings.StaleHold),
				playbook:  "SIGNALS.md §27.1",
			}
		})
		judge("extender-seed-mirror-expired", expiring, func() finding {
			return finding{
				probeId: extenderSeedMirrorsProbeId, tier: tierWarn, class: "extender-seed-mirror-expired", target: target, sustain: 1,
				symptom:   fmt.Sprintf("Seed source %s serves a bundle with less than %s of validity left at every evaluation for %s", source.name, settings.ExpiryMargin, settings.ExpiredHold),
				mechanism: "A bundle is valid for its records' lifetime, a day by default, and the publisher replaces it every six hours, so a healthy source always has at least eighteen hours left. Under the margin, the publisher has missed its refreshes or the mirror stopped copying; at zero every client refuses the bundle as expired, with a five-minute clock allowance.",
				baseline:  fmt.Sprintf("At least the record lifetime less six hours of validity on every source. WARN under %s for %s.", settings.ExpiryMargin, settings.ExpiredHold),
				observed:  observed,
				evidence:  "One bounded Mimir range read of the seed chain's expiry gauges, through a services host's loopback Mimir.",
				context:   "Expiry is reported for any bundle that verifies, fresh or not, so an expired source is told apart from a broken one. A bundle embedded in an application release expires the same way and is not a permanent escape hatch.",
				action:    "If the origin is expiring too, read the publisher's outcomes (store, signing, self-check) and restore the seed chain. If only a mirror is, restore its sync from the origin.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads more than %s of validity left.", settings.ExpiredHold, settings.ExpiryMargin),
				playbook:  "SIGNALS.md §27.1",
			}
		})
		judge("extender-seed-mirror-rollback", rollback, func() finding {
			return finding{
				probeId: extenderSeedMirrorsProbeId, tier: tierWarn, class: "extender-seed-mirror-rollback", target: target, sustain: 1,
				symptom:   fmt.Sprintf("Seed source %s serves a sequence below the highest it served in the last %s", source.name, settings.RollbackWindow),
				mechanism: "Seed sequences only rise: the publisher raises each past every stored and claimed sequence, and a client persists the highest sequence it accepted and refuses anything lower. A source that went back is serving an older bundle, from a restored backup, a cache or a mirror node that missed updates, which every client that saw the newer one refuses.",
				baseline:  fmt.Sprintf("No source's sequence ever falls. WARN when it is below its %s peak at every evaluation for %s.", settings.RollbackWindow, settings.RollbackHold),
				observed:  observed,
				evidence:  fmt.Sprintf("One bounded Mimir range read of the seed chain's sequence gauges and max_over_time of each over %s.", settings.RollbackWindow),
				context:   "The peak is per source: a mirror that never served the newer bundle is stale, not rolled back. Two taskworkers' fresh observations are merged by max, which can delay a rollback's report by up to half an hour.",
				action:    "Find what replaced the source's bundle with an older one and restore the newest; a mirror's sync refuses to install a lower sequence, so a rolled-back mirror points at its storage or its cache. For the origin, the blob store's latest.bin was replaced.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the source at or above its peak.", settings.RollbackHold),
				playbook:  "SIGNALS.md §27.1",
			}
		})
		if !stale.complete && !source.origin {
			missing = append(missing, source.name+"[missing]")
		}
	}
	if 0 < len(missing) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, missing))
	} else {
		findings = append(findings, healthyFinding(extenderSeedMirrorsProbeId, tierWarn, "extender-seed-mirrors-unobservable", stateTarget))
	}
	return findings, nil
}

// A route's up state at an evaluation; the origin has none.
func extenderSeedRouteUp(source extenderSeedSource, history *stormHistory, at int64) string {
	if source.origin {
		return "n/a"
	}
	return stormNumber(history.reading("up", source.key, at), "%.0f")
}
