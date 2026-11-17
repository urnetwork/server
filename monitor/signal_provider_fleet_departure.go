package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §28.3 maps to signal_provider_fleet_departure.go and
// signal_provider_fleet_departure_test.go. A provider fleet that goes offline
// together takes its connect connections, its gossip links and its pings with
// it, and from the server those look like our own failures. On 2026-11-11 at
// 10:35Z about 12.5k devices in Vietnam on one custom build left within 30
// seconds: gossip links fell from 15.6k to 3.2k and connect lost 8% of its
// clients, and the first reading was a gossip outage
// (factury/research/gossip-drop-2026-11-11.md). The same fleet had left once
// before, on 11-02 at 13:52Z.
//
// This probe reports such a departure as what it is: a country whose online
// providers fell by more than half of their trailing peak, and by at least a
// thousand, held long enough to span two refreshes of the stats collector.
// Beside it, it reports the fleet's total, connect's clients, the gossip
// links and the builds that lost the most providers over the same window, so
// the reader sees the whole departure in one alert.
//
// Countries are ISO codes and builds the app version labels the stats gauge
// already exports; no device, network or address reaches an alert.
func NewProviderFleetDepartureSignal() Signal {
	return NewProviderFleetDepartureSignalWithSettings(DefaultProviderFleetDepartureSettings())
}

// The signal with explicit thresholds.
func NewProviderFleetDepartureSignalWithSettings(settings *ProviderFleetDepartureSettings) Signal {
	return &signalAdapter{
		number: "28.3", key: "provider-fleet-departure", name: "Provider fleet departure",
		probe: providerFleetDepartureProbe{settings: settings},
	}
}

const providerFleetDepartureProbeId = "mimir/provider-fleet-departure"

// The thresholds and bounds of §28.3.
type ProviderFleetDepartureSettings struct {
	// a country's online providers are measured against their peak over
	// PeakWindow
	PeakWindow time.Duration
	// a departure is at least MinLoss providers and more than MinLossShare of
	// the country's peak, at every evaluation of Hold
	MinLoss      float64
	MinLossShare float64
	Hold         time.Duration
	// the builds that lost the most providers, listed beside a departure
	TopBuilds int
	// the most countries one alert lists
	MaxListed int
	// the one history read
	StormReadSettings
}

// The defaults of SIGNALS.md §28.3.
func DefaultProviderFleetDepartureSettings() *ProviderFleetDepartureSettings {
	read := DefaultStormReadSettings()
	read.Cadence = 5 * time.Minute
	// about 120 countries and a few builds over 45 minutes
	read.MaxRows = 16384
	read.MaxResponseBytes = 4 * 1024 * 1024
	return &ProviderFleetDepartureSettings{
		PeakWindow:        3 * time.Hour,
		MinLoss:           1000,
		MinLossShare:      0.5,
		Hold:              45 * time.Minute,
		TopBuilds:         3,
		MaxListed:         8,
		StormReadSettings: read,
	}
}

func (self *ProviderFleetDepartureSettings) valid() bool {
	return self != nil && self.PeakWindow >= time.Minute && self.PeakWindow%time.Minute == 0 &&
		0 < self.MinLoss && 0 < self.MinLossShare && self.MinLossShare < 1 &&
		0 < self.TopBuilds && 0 < self.MaxListed &&
		self.StormReadSettings.valid(self.Hold)
}

type providerFleetDepartureProbe struct {
	settings *ProviderFleetDepartureSettings
}

func (providerFleetDepartureProbe) id() string   { return providerFleetDepartureProbeId }
func (providerFleetDepartureProbe) tier() string { return tierWarn }

func (self providerFleetDepartureProbe) cadence() time.Duration {
	return self.effectiveSettings().schedule()
}

func (self providerFleetDepartureProbe) effectiveSettings() *ProviderFleetDepartureSettings {
	if self.settings == nil {
		return DefaultProviderFleetDepartureSettings()
	}
	return self.settings
}

// A labeled gauge against its own peak over the window, keyed by label: the
// current value, with a key the gauge no longer exports read as zero while the
// gauge exports any key at all, and the peak.
func providerFleetNowAndPeak(metric string, by string, window string) (string, string) {
	peak := "max by (" + by + ") (max_over_time(" + metric + window + "))"
	now := "(max by (" + by + ") (" + metric + ") or 0 * " + peak + ") and on () count(" + metric + ")"
	return now, peak
}

// §28.3's expressions, each tagged with its part.
func providerFleetDepartureQuery(environment string, settings *ProviderFleetDepartureSettings) string {
	env := "env=" + strconv.Quote(environment)
	window := "[" + stormDuration(settings.PeakWindow) + "]"
	subquery := "[" + stormDuration(settings.PeakWindow) + ":" + stormDuration(settings.Step) + "]"
	countryNow, countryPeak := providerFleetNowAndPeak("urnetwork_stats_online_providers_by_country{"+env+"}", "country_code", window)
	versions := "urnetwork_stats_device_versions{" + env + `,population="provider"}`
	versionNow, versionPeak := providerFleetNowAndPeak(versions, "version", window)
	connect := "sum(max by (host, block) (urnetwork_connect_connected_clients{" + env + "}))"
	gossip := "sum(max by (host) (process_open_fds{" + env + `,service="gossip"}))`
	return strings.Join([]string{
		fleetPart("country_now", countryNow),
		fleetPart("country_peak", countryPeak),
		fleetPart("build_loss", "topk("+strconv.Itoa(settings.TopBuilds)+", "+versionPeak+" - ("+versionNow+"))"),
		fleetPart("providers_now", "max(urnetwork_stats_online_providers{"+env+"})"),
		fleetPart("providers_peak", "max(max_over_time(urnetwork_stats_online_providers{"+env+"}"+window+"))"),
		fleetPart("connect_now", connect),
		fleetPart("connect_peak", "max_over_time("+connect+subquery+")"),
		fleetPart("gossip_now", gossip),
		fleetPart("gossip_peak", "max_over_time("+gossip+subquery+")"),
	}, " or ")
}

var providerFleetDepartureShapes = map[string]stormShape{
	"country_now":    {keys: []string{"country_code"}},
	"country_peak":   {keys: []string{"country_code"}},
	"build_loss":     {keys: []string{"version"}},
	"providers_now":  {},
	"providers_peak": {},
	"connect_now":    {},
	"connect_peak":   {},
	"gossip_now":     {},
	"gossip_peak":    {},
}

// One country's loss at an evaluation.
type providerFleetCountryLoss struct {
	countryCode string
	now         float64
	peak        float64
}

func (self providerFleetCountryLoss) loss() float64 {
	return self.peak - self.now
}

func (self providerFleetCountryLoss) share() float64 {
	if self.peak <= 0 {
		return 0
	}
	return self.loss() / self.peak
}

func (self providerFleetDepartureProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	target := env.cfg.env + "/provider-fleet"
	unobservable := func(reasons []string, gaps []string) finding {
		return stormUnobservable(providerFleetDepartureProbeId, "provider-fleet-departure-unobservable", target, "§28.3", reasons, gaps, max(1, settings.MaxListed))
	}
	if !settings.valid() {
		return []finding{unobservable([]string{"invalid_policy"}, nil)}, nil
	}
	history, err := readStormHistory(ctx, env, providerFleetDepartureQuery(env.cfg.env, settings), providerFleetDepartureShapes, settings.holdSteps(settings.Hold), settings.StormReadSettings)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return []finding{stormReadFailureFinding(unobservable([]string{stormReadReason(err)}, nil), err)}, nil
	}
	reasons := history.invalidReasons()
	countries := history.keys("country_peak")
	if len(countries) == 0 {
		// no taskworker's stats collector exported the providers by country
		return []finding{unobservable(append(reasons, "provider_state_missing"), nil)}, nil
	}

	// the countries that lost at least MinLoss at a step, or a gap when the
	// gauge exported no country at that step
	lossesAt := func(step int64) ([]providerFleetCountryLoss, bool) {
		losses := []providerFleetCountryLoss{}
		for _, countryCode := range countries {
			peak := history.reading("country_peak", countryCode, step)
			if !peak.present {
				continue
			}
			now := history.reading("country_now", countryCode, step)
			if !now.present {
				return nil, false
			}
			loss := providerFleetCountryLoss{countryCode: countryCode, now: now.value, peak: peak.value}
			if settings.MinLoss <= loss.loss() {
				losses = append(losses, loss)
			}
		}
		return losses, true
	}
	// the largest share any country lost
	lossShareAt := func(step int64) stormReading {
		losses, known := lossesAt(step)
		if !known {
			return stormReading{}
		}
		largest := 0.0
		for _, loss := range losses {
			largest = max(largest, loss.share())
		}
		return stormValue(largest)
	}
	departure := stormHoldOf(history.last(settings.holdSteps(settings.Hold)), stormLevel{threshold: settings.MinLossShare}, lossShareAt)

	findings := []finding{}
	switch {
	case departure.held:
		findings = append(findings, self.departureFinding(settings, history, target, lossesAt))
	case departure.clear:
		findings = append(findings, healthyFinding(providerFleetDepartureProbeId, tierWarn, "provider-fleet-departure", target))
	default:
		findings = append(findings, stormClearHold(providerFleetDepartureProbeId, "provider-fleet-departure", target))
	}
	if !departure.complete {
		reasons = append(reasons, "evaluation_missing")
	}
	if 0 < len(reasons) {
		findings = append(findings, unobservable(reasons, nil))
	} else {
		findings = append(findings, healthyFinding(providerFleetDepartureProbeId, tierWarn, "provider-fleet-departure-unobservable", target))
	}
	return findings, nil
}

func (self providerFleetDepartureProbe) departureFinding(
	settings *ProviderFleetDepartureSettings,
	history *stormHistory,
	target string,
	lossesAt func(step int64) ([]providerFleetCountryLoss, bool),
) finding {
	at := history.at()
	losses, _ := lossesAt(at)
	departed := []providerFleetCountryLoss{}
	for _, loss := range losses {
		if settings.MinLossShare < loss.share() {
			departed = append(departed, loss)
		}
	}
	sort.Slice(departed, func(i, j int) bool {
		if departed[i].loss() != departed[j].loss() {
			return departed[i].loss() > departed[j].loss()
		}
		return departed[i].countryCode < departed[j].countryCode
	})
	listedCountries := []string{}
	for _, loss := range departed {
		listedCountries = append(listedCountries, fmt.Sprintf("%s:%.0f->%.0f(-%.0f%%)", loss.countryCode, loss.peak, loss.now, 100*loss.share()))
	}

	type buildLoss struct {
		version string
		loss    float64
	}
	builds := []buildLoss{}
	for _, version := range history.keys("build_loss") {
		reading := history.reading("build_loss", version, at)
		if reading.present && 0 < reading.value {
			builds = append(builds, buildLoss{version: version, loss: reading.value})
		}
	}
	sort.Slice(builds, func(i, j int) bool {
		if builds[i].loss != builds[j].loss {
			return builds[i].loss > builds[j].loss
		}
		return builds[i].version < builds[j].version
	})
	listedBuilds := []string{}
	for _, build := range builds {
		listedBuilds = append(listedBuilds, fmt.Sprintf("%s:-%.0f", build.version, build.loss))
	}

	pair := func(part string) string {
		return stormNumber(history.reading(part+"_now", "", at), "%.0f") + "/" + stormNumber(history.reading(part+"_peak", "", at), "%.0f")
	}
	return finding{
		probeId: providerFleetDepartureProbeId, tier: tierWarn, class: "provider-fleet-departure", target: target, sustain: 1,
		symptom:   fmt.Sprintf("A provider fleet went offline: %s lost more than %.0f%% of its online providers, at least %.0f, against its %s peak, at every evaluation for %s", stormListed(departedCodes(departed), settings.MaxListed), 100*settings.MinLossShare, settings.MinLoss, settings.PeakWindow, settings.Hold),
		mechanism: "Providers that go offline together close their connect control connections and their gossip links and stop pinging and calling the api, all at once. From the server that reads as a connect, gossip or ping failure. A fleet that is most of the gossip mesh takes the mesh with it: on 2026-11-11 a 12.5k-device fleet in Vietnam left at 10:35Z and gossip links fell five-fold. No server change is involved, and none brings the fleet back.",
		baseline:  fmt.Sprintf("No country loses more than half of its providers within %s; the largest swings from 2026-10-28 to 11-11 were whole fleets: Vietnam on 11-02 (-67%%) and 11-11 (-98%%). WARN when a country loses more than %.0f%% and at least %.0f against its %s peak for %s, which spans two refreshes of the stats collector.", settings.PeakWindow, 100*settings.MinLossShare, settings.MinLoss, settings.PeakWindow, settings.Hold),
		observed: fmt.Sprintf("countries=%s online_providers=%s connect_clients=%s gossip_open_fds=%s builds_lost=%s evaluation_unix=%d",
			stormListed(listedCountries, settings.MaxListed),
			pair("providers"),
			pair("connect"),
			pair("gossip"),
			stormListed(listedBuilds, settings.TopBuilds),
			at,
		),
		evidence: fmt.Sprintf("One bounded Mimir range read of urnetwork_stats_online_providers_by_country against its %s peak, with urnetwork_stats_online_providers, urnetwork_connect_connected_clients, the gossip processes' open fds and urnetwork_stats_device_versions{population=\"provider\"} over the same window (each now/peak).", settings.PeakWindow),
		context:  "The stats collector refreshes the provider counts every 30-40 minutes and lags the departure by as much, while connect and gossip see it at once. A build that lost providers while the total held is an upgrade (its devices moved to another build), not a departure. A fleet spread thinly over many countries, or a large country's fleet that is under half of it (the US fleets of 5-20k on build 2026.7.2 that cycle off and on), is not caught here.",
		action:   "Treat connect, gossip and ping drops in the same window as this fleet's, not as a server fault, unless a server change lines up with the drop (process starts, a config release, a lb reload). The builds listed name the fleet; an operator who runs it can be reached through support. Check §28.4's gossip mesh: a fleet that returns brings its gossip links back, and more members than the replicas' watermarks restarts the trim loop.",
		verify:   fmt.Sprintf("Every evaluation of a complete %s hold reads no country more than %.0f%% under its %s peak by %.0f or more: the fleet returned, or the peak window has passed the departure.", settings.Hold, 100*settings.MinLossShare, settings.PeakWindow, settings.MinLoss),
		playbook: "SIGNALS.md §28.3",
	}
}

func departedCodes(losses []providerFleetCountryLoss) []string {
	codes := []string{}
	for _, loss := range losses {
		codes = append(codes, loss.countryCode)
	}
	return codes
}
