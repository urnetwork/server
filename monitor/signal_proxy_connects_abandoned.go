package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §26.14 maps to signal_proxy_connects_abandoned.go and
// signal_proxy_connects_abandoned_test.go. A proxy process counts an HTTP
// CONNECT as abandoned (urnetwork_proxy_ingress_events_total
// {protocol="http",reason="connect_client_gone"}) when the client drops it
// while the proxy is still dialing the target through the hosted device, or
// when the proxy shuts down under it. A slow first CONNECT makes people and
// their tools give up: from 2026-11-14 abandoned CONNECTs rose from 1-10/s
// per host to 100-400/s, and to 750-990/s on 11-18 (proxy/P90SPEED.md §8).
// Bots that open CONNECTs and drop them raise the count too, so the alert is
// read with setup time and mint refusals (§26.12). The reference is the
// host's own median of the rate over the previous 24 hours at 15-minute
// steps, offset 30 minutes so the rise does not pull its own reference up,
// and the rate must also pass a floor. A rate at or under the floor reads
// within the band whatever its reference; above it, a zero reference is a
// rise from nothing and a missing one is unknown. A sum over fewer blocks can
// only be lower, so a held band stands while a block is missing; a clear
// needs every placed block.
func NewProxyConnectsAbandonedSignal() Signal {
	return NewProxyConnectsAbandonedSignalWithSettings(DefaultProxyConnectsAbandonedSettings())
}

// The signal with explicit thresholds.
func NewProxyConnectsAbandonedSignalWithSettings(settings *ProxyConnectsAbandonedSettings) Signal {
	return &signalAdapter{
		number: "26.14", key: "proxy-connects-abandoned", name: "Proxy CONNECTs abandoned against the previous day",
		probe: proxyConnectsAbandonedProbe{settings: settings},
	}
}

const proxyConnectsAbandonedProbeId = "mimir/proxy-connects-abandoned"

// The thresholds and bounds of §26.14.
type ProxyConnectsAbandonedSettings struct {
	// the service whose CONNECTs are read
	Service string
	// the rate window of each evaluation
	Window time.Duration
	// the reference: the median of the host's rate over BaselineSpan at
	// BaselineStep, ending BaselineOffset before each evaluation
	BaselineSpan   time.Duration
	BaselineStep   time.Duration
	BaselineOffset time.Duration
	// a host's rate above RiseRatio times its reference and above MinRate
	// per second for Hold warns
	RiseRatio float64
	MinRate   float64
	Hold      time.Duration
	// the most gaps one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §26.14.
func DefaultProxyConnectsAbandonedSettings() *ProxyConnectsAbandonedSettings {
	return &ProxyConnectsAbandonedSettings{
		Service:           "proxy",
		Window:            15 * time.Minute,
		BaselineSpan:      24 * time.Hour,
		BaselineStep:      15 * time.Minute,
		BaselineOffset:    30 * time.Minute,
		RiseRatio:         3,
		MinRate:           50,
		Hold:              15 * time.Minute,
		MaxListed:         8,
		StormReadSettings: DefaultStormReadSettings(),
	}
}

func (self *ProxyConnectsAbandonedSettings) valid() bool {
	wholeSeconds := func(duration time.Duration) bool { return duration >= time.Second && duration%time.Second == 0 }
	return self != nil && self.Service != "" && wholeSeconds(self.Window) && wholeSeconds(self.BaselineSpan) &&
		wholeSeconds(self.BaselineStep) && self.BaselineStep <= self.BaselineSpan && 0 <= self.BaselineOffset && self.BaselineOffset%time.Second == 0 &&
		1 < self.RiseRatio && 0 <= self.MinRate && 0 < self.MaxListed && self.StormReadSettings.valid(self.Hold)
}

type proxyConnectsAbandonedProbe struct {
	settings *ProxyConnectsAbandonedSettings
}

func (proxyConnectsAbandonedProbe) id() string   { return proxyConnectsAbandonedProbeId }
func (proxyConnectsAbandonedProbe) tier() string { return tierWarn }

func (self proxyConnectsAbandonedProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self proxyConnectsAbandonedProbe) effectiveSettings() *ProxyConnectsAbandonedSettings {
	if self.settings == nil {
		return DefaultProxyConnectsAbandonedSettings()
	}
	return self.settings
}

// §26.14's expression, its reference, and the blocks whose rates it sums.
func proxyConnectsAbandonedQuery(environment string, settings *ProxyConnectsAbandonedSettings) string {
	rate := "rate(urnetwork_proxy_ingress_events_total{env=" + strconv.Quote(environment) + ",service=" + strconv.Quote(settings.Service) + `,protocol="http",reason="connect_client_gone"}[` + stormDuration(settings.Window) + "])"
	abandoned := "sum by (host) (" + rate + ")"
	reference := "quantile_over_time(0.5, " + abandoned + "[" + stormDuration(settings.BaselineSpan) + ":" + stormDuration(settings.BaselineStep) + "] offset " + stormDuration(settings.BaselineOffset) + ")"
	return strings.Join([]string{
		fleetPart("abandoned", abandoned),
		fleetPart("reference", reference),
		fleetPart("slots", "count by (host, block) ("+rate+")"),
	}, " or ")
}

var proxyConnectsAbandonedShapes = map[string]stormShape{
	"abandoned": {keys: []string{"host"}},
	"reference": {keys: []string{"host"}},
	"slots":     {keys: []string{"host", "block"}},
}

func (self proxyConnectsAbandonedProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	unobservableTarget := env.cfg.env + "/proxy-connects-abandoned"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(proxyConnectsAbandonedProbeId, "proxy-connects-abandoned-unobservable", unobservableTarget, "§26.14", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, proxyConnectsAbandonedQuery(env.cfg.env, settings), proxyConnectsAbandonedShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}

	placed, placementGaps := stormPlacedHostBlocks(env, settings.Service)
	reasons := history.invalidReasons()
	if len(placed) == 0 {
		reasons = append(reasons, "placement_missing")
	}
	if 0 < len(placementGaps) {
		reasons = append(reasons, "placement_incomplete")
	}
	level := stormLevel{threshold: settings.RiseRatio}
	steps := history.last(settings.holdSteps(settings.Hold))
	gaps := []string{}
	findings := []finding{}
	for _, name := range stormJudgedHosts(placed, history, "abandoned", "slots") {
		// the band's reading is the rise over the reference once the rate
		// passes the floor; a sum over fewer blocks stands only beyond
		riseAt := func(step int64) stormReading {
			rate := history.reading("abandoned", name, step)
			if !rate.present {
				return stormReading{}
			}
			complete := len(stormAbsentBlocks(history, "slots", name, placed[name], step)) == 0
			if rate.value <= settings.MinRate {
				if complete {
					return stormValue(0)
				}
				return stormReading{}
			}
			reference := history.reading("reference", name, step)
			if !reference.present {
				return stormReading{}
			}
			rise := math.Inf(1)
			if 0 < reference.value {
				rise = rate.value / reference.value
			}
			if complete || level.beyond(rise) {
				return stormValue(rise)
			}
			return stormReading{}
		}
		hold := stormHoldOf(steps, level, riseAt)
		absentBools := map[string]bool{}
		for _, step := range steps {
			for _, key := range stormAbsentBlocks(history, "slots", name, placed[name], step) {
				absentBools[key] = true
			}
		}
		for key := range absentBools {
			gaps = append(gaps, key+"[missing]")
		}
		if !hold.complete && len(absentBools) == 0 {
			gaps = append(gaps, name+"[missing]")
		}
		switch {
		case hold.held:
			reference := history.reading("reference", name, history.at())
			findings = append(findings, finding{
				probeId: proxyConnectsAbandonedProbeId, tier: tierWarn, class: "proxy-connects-abandoned", target: name, sustain: 1,
				symptom:   fmt.Sprintf("Clients abandoned more than %g CONNECTs a second on proxy host %s, more than %g times its median of the previous %s, at every evaluation for %s", settings.MinRate, name, settings.RiseRatio, stormDuration(settings.BaselineSpan), settings.Hold),
				mechanism: "A proxy process counts an HTTP CONNECT as abandoned (connect_client_gone) when the client drops it while the proxy is still dialing the target through the hosted device, or when the proxy shuts down under it. A first CONNECT waits until one of its window's exits answers, which takes seconds to tens of seconds once the window's mints are refused (§26.12), so people and their tools give up and retry. From 2026-11-14 abandoned CONNECTs rose from 1-10/s per host to 100-400/s on 11-16 and 11-17, and to 750-990/s on the morning of 11-18 (proxy/P90SPEED.md §8).",
				baseline:  fmt.Sprintf("Per host from 2026-11-07 to the load ramp at 13:51Z on 11-14: median 2.3/s, peak 20/s. WARN when a host's %s rate passes both %g/s and %g times its median over the previous %s (at %s steps, offset %s) at every one-minute evaluation for %s.", stormDuration(settings.Window), settings.MinRate, settings.RiseRatio, stormDuration(settings.BaselineSpan), stormDuration(settings.BaselineStep), stormDuration(settings.BaselineOffset), settings.Hold),
				observed:  fmt.Sprintf("host=%s abandoned_per_second_now=%s reference_per_second=%s rise_now=%s rise_level=%g floor_per_second=%g hold=%s evaluation_unix=%d", name, stormNumber(history.reading("abandoned", name, history.at()), "%.1f"), stormNumber(reference, "%.1f"), stormNumber(riseAt(history.at()), "%.2f"), settings.RiseRatio, settings.MinRate, settings.Hold, history.at()),
				evidence:  fmt.Sprintf("One bounded Mimir range read of sum by (host) (rate(urnetwork_proxy_ingress_events_total{service=%q,protocol=\"http\",reason=\"connect_client_gone\"}[%s])) and its quantile_over_time(0.5, ...[%s:%s] offset %s), with the blocks that answered, through a services host's loopback Mimir.", settings.Service, stormDuration(settings.Window), stormDuration(settings.BaselineSpan), stormDuration(settings.BaselineStep), stormDuration(settings.BaselineOffset)),
				context:   "Bots that open CONNECTs and drop them raise this count as well as people who give up, so read it with setup time (the proxy-test runner) and mint refusals (§26.12) before calling it what customers see. The median absorbs a rise within about a day: the 100-400/s of 11-16 and 11-17 became the reference, and the rule cleared once it had (11-16 09:25Z on fireside, 13:03Z on crisp). A proxy restart counts the CONNECTs it cuts, but its burst is short against the 15-minute rate.",
				action:    "Read the mint-refusal (§26.12) and heap-limit (§26.13) entries for the host, and the proxy-test runner's setup time. If they agree, hosted proxies are slow to set up: restore the host's headroom (P90SPEED.md §9, decision 1). If they are quiet, look for a client population that opens CONNECTs and drops them before treating this as an outage.",
				verify:    fmt.Sprintf("Every evaluation of a complete %s hold reads the host at or under %g/s or at or under %g times its reference.", settings.Hold, settings.MinRate, settings.RiseRatio),
				playbook:  "SIGNALS.md §26.14 and proxy/P90SPEED.md §8",
			})
		case hold.clear:
			findings = append(findings, healthyFinding(proxyConnectsAbandonedProbeId, tierWarn, "proxy-connects-abandoned", name))
		default:
			findings = append(findings, stormClearHold(proxyConnectsAbandonedProbeId, "proxy-connects-abandoned", name))
		}
	}
	sort.Strings(gaps)
	if 0 < len(gaps) {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, append(placementGaps, gaps...)))
	} else {
		findings = append(findings, healthyFinding(proxyConnectsAbandonedProbeId, tierWarn, "proxy-connects-abandoned-unobservable", unobservableTarget))
	}
	return findings, nil
}
