package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The Online URL ratio (egress_online_success.go, owner considering it
// 2026-10-15), pure: the decisions with the ratio off and on, its country
// exemption, the kill switch, provider.yml, the published guard, the counts
// policy key, provider status and the published score.

// One provider of the ratio tests' population: its accepted URL outcomes in
// the evidence window (none while total is zero), its published country, its
// common gate failures, and its decision with the ratio off, which is
// today's.
type onlineUrlRatioTestProvider struct {
	name              string
	countryCode       string
	ok                int
	total             int
	arinNonQuality    bool
	reliabilityFailed bool
	arinRisk          bool
	tlsFailed         bool
	want              onlineUrlRatioTestDecision
}

// What a decision does with the provider's buckets.
type onlineUrlRatioTestDecision struct {
	reason       string
	hardExcluded bool
	quality      bool
	speed        bool
	online       bool
	counted      bool
}

func onlineUrlRatioTestBuckets(decision providerEgressDecision) onlineUrlRatioTestDecision {
	return onlineUrlRatioTestDecision{
		reason:       decision.reason,
		hardExcluded: decision.hardExcluded,
		quality:      decision.quality,
		speed:        decision.speed,
		online:       decision.online,
		counted:      decision.counted,
	}
}

// in Quality, Speed and Online
var onlineUrlRatioTestNative = onlineUrlRatioTestDecision{quality: true, speed: true, online: true, counted: true}

// out of every bucket and the counts, no hard exclusion
func onlineUrlRatioTestOut(reason string) onlineUrlRatioTestDecision {
	return onlineUrlRatioTestDecision{reason: reason}
}

// in Online and the counts only
func onlineUrlRatioTestOnlineOnly(reason string) onlineUrlRatioTestDecision {
	return onlineUrlRatioTestDecision{reason: reason, online: true, counted: true}
}

// out of everything
func onlineUrlRatioTestHard(reason string) onlineUrlRatioTestDecision {
	return onlineUrlRatioTestDecision{reason: reason, hardExcluded: true}
}

// The population: us, where the probe works (9 of 12 measured providers have
// a success), and vn, where it fails (1 of 5), so the URL-success rule exempts
// vn. Each provider's want is its decision with the ratio off.
func onlineUrlRatioTestPopulation() []*onlineUrlRatioTestProvider {
	return []*onlineUrlRatioTestProvider{
		{name: "us 60 of 60", countryCode: "us", ok: 60, total: 60, want: onlineUrlRatioTestNative},
		{name: "us 4 of 5", countryCode: "us", ok: 4, total: 5, want: onlineUrlRatioTestNative},
		{name: "us 8 of 10", countryCode: "us", ok: 8, total: 10, want: onlineUrlRatioTestNative},
		{name: "us 19 of 24", countryCode: "us", ok: 19, total: 24, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		{name: "us 7 of 10", countryCode: "us", ok: 7, total: 10, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		{name: "us 3 of 5", countryCode: "us", ok: 3, total: 5, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		{name: "us 1 of 2", countryCode: "us", ok: 1, total: 2, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		{name: "us 0 of 5", countryCode: "us", ok: 0, total: 5, want: onlineUrlRatioTestOut(ProviderExcludedNoSuccess)},
		{name: "us 0 of 2", countryCode: "us", ok: 0, total: 2, want: onlineUrlRatioTestOut(ProviderExcludedNoSuccess)},
		{name: "us 0 of 1", countryCode: "us", ok: 0, total: 1, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		// one run is no native verdict (egress_native_min_runs.go)
		{name: "us 1 of 1", countryCode: "us", ok: 1, total: 1, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedUnprobed)},
		{name: "us unprobed", countryCode: "us", want: onlineUrlRatioTestOnlineOnly(ProviderExcludedUnprobed)},
		{name: "us arin non-quality 5 of 5", countryCode: "us", ok: 5, total: 5, arinNonQuality: true, want: onlineUrlRatioTestDecision{reason: ProviderExcludedArinNonQuality, speed: true, online: true, counted: true}},
		{name: "us unreliable 5 of 5", countryCode: "us", ok: 5, total: 5, reliabilityFailed: true, want: onlineUrlRatioTestHard(ProviderExcludedReliability)},
		{name: "us arin risk 3 of 5", countryCode: "us", ok: 3, total: 5, arinRisk: true, want: onlineUrlRatioTestHard(ProviderExcludedArinRisk)},
		{name: "us tls 0 of 5", countryCode: "us", ok: 0, total: 5, tlsFailed: true, want: onlineUrlRatioTestHard(ProviderExcludedTls)},
		{name: "vn 3 of 5", countryCode: "vn", ok: 3, total: 5, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		{name: "vn 0 of 5 a", countryCode: "vn", ok: 0, total: 5, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		{name: "vn 0 of 5 b", countryCode: "vn", ok: 0, total: 5, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		{name: "vn 0 of 5 c", countryCode: "vn", ok: 0, total: 5, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
		{name: "vn 0 of 5 d", countryCode: "vn", ok: 0, total: 5, want: onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth)},
	}
}

// A fleet pass's filter over the population under settings, with the
// country guard counted over it, and the client id of each provider by name.
func onlineUrlRatioTestFilter(providers []*onlineUrlRatioTestProvider, settings *EgressIndexSettings) (providerCountFilter, map[string]server.Id) {
	filter := providerCountFilter{
		now:                     onlineSuccessTestNow,
		arinRisk:                map[server.Id]bool{},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		tlsAuthenticationFailed: map[server.Id]bool{},
		countryCodes:            map[server.Id]string{},
		healthCounts:            map[server.Id]ProviderEgressHealthCounts{},
	}
	nameClientIds := map[string]server.Id{}
	clientIdCountryCodes := map[server.Id]string{}
	for _, provider := range providers {
		clientId := server.NewId()
		nameClientIds[provider.name] = clientId
		clientIdCountryCodes[clientId] = provider.countryCode
		if 0 < provider.total {
			filter.healthCounts[clientId] = onlineSuccessTestCounts(provider.ok, provider.total)
		}
		if provider.arinNonQuality {
			filter.arinNonQuality[clientId] = true
		}
		if provider.reliabilityFailed {
			filter.reliabilityFailed[clientId] = true
		}
		if provider.arinRisk {
			filter.arinRisk[clientId] = true
		}
		if provider.tlsFailed {
			filter.tlsAuthenticationFailed[clientId] = true
		}
	}
	filter.onlineSuccessBaseline = newProviderOnlineSuccessBaseline(filter, clientIdCountryCodes, settings, nil)
	return filter, nameClientIds
}

// The settings provider.yml gives.
func onlineUrlRatioTestSettings(config string) *EgressIndexSettings {
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
	defer pop()
	return egressIndexSettings()
}

// Fails for every provider whose decision under settings differs from its
// want, or from the override for its name.
func assertOnlineUrlRatioTestDecisions(t *testing.T, label string, settings *EgressIndexSettings, overrides map[string]onlineUrlRatioTestDecision) {
	t.Helper()
	providers := onlineUrlRatioTestPopulation()
	filter, nameClientIds := onlineUrlRatioTestFilter(providers, settings)
	for _, provider := range providers {
		want := provider.want
		if override, ok := overrides[provider.name]; ok {
			want = override
		}
		decision := onlineUrlRatioTestBuckets(onlineSuccessTestDecide(filter, nameClientIds[provider.name], provider.countryCode, settings))
		if decision != want {
			t.Errorf("%s, %s: decision %+v, want %+v", label, provider.name, decision, want)
		}
	}
}

// With the ratio off, the default, every decision is today's: the golden
// table is the decisions of the code before the ratio, and provider.yml that
// sets the ratio to zero, or only the exemption switch, changes none of them.
func TestOnlineUrlRatioOffKeepsEveryDecision(t *testing.T) {
	for _, c := range []struct {
		label    string
		settings *EgressIndexSettings
	}{
		{label: "defaults", settings: DefaultEgressIndexSettings()},
		{label: "empty provider.yml", settings: onlineUrlRatioTestSettings("")},
		{label: "ratio 0", settings: onlineUrlRatioTestSettings("online_url_success_ratio: 0\n")},
		{label: "ratio 0 without the exemption", settings: onlineUrlRatioTestSettings("online_url_success_ratio: 0\nonline_url_ratio_country_exemption: false\n")},
		{label: "exemption switch alone", settings: onlineUrlRatioTestSettings("online_url_ratio_country_exemption: false\n")},
	} {
		assertOnlineUrlRatioTestDecisions(t, c.label, c.settings, nil)
	}
}

// At 0.8 with the country exemption (variant c), a provider measured at
// least twice with a success but below 0.8 -- 3 of 5, 7 of 10, 1 of 2 and
// 19 of 24 -- leaves every bucket and the counts for low_success, with no
// hard exclusion. 4 of 5 and 8 of 10 are at the ratio and stay in Online and
// Speed. Unprobed providers and those with a single run keep Online, and a
// provider without a success stays no_success.
func TestOnlineUrlRatioTakesProvidersBelowItOutOfEveryBucket(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	settings.OnlineUrlSuccessRatio = 0.8
	assertOnlineUrlRatioTestDecisions(t, "ratio 0.8", settings, map[string]onlineUrlRatioTestDecision{
		"us 19 of 24": onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
		"us 7 of 10":  onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
		"us 3 of 5":   onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
		"us 1 of 2":   onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
	})

	// none of them is a common hard exclusion, which would stop probes and
	// explicit requests
	providers := onlineUrlRatioTestPopulation()
	filter, nameClientIds := onlineUrlRatioTestFilter(providers, settings)
	for _, name := range []string{"us 19 of 24", "us 7 of 10", "us 3 of 5", "us 1 of 2"} {
		if filter.hasHardEgressFailure(nameClientIds[name]) {
			t.Errorf("%s: the ratio became a common hard exclusion", name)
		}
	}
	// the boundary is exact: 19 of 24 is 0.7917, and 4 of 5 is 0.8
	connect.AssertEqual(t, providerBelowOnlineUrlRatio(onlineSuccessTestCounts(19, 24), EgressIndex{Evidence: true}, 0.8), true)
	connect.AssertEqual(t, providerBelowOnlineUrlRatio(onlineSuccessTestCounts(4, 5), EgressIndex{Evidence: true}, 0.8), false)
	connect.AssertEqual(t, providerBelowOnlineUrlRatio(onlineSuccessTestCounts(8, 10), EgressIndex{Evidence: true}, 0.8), false)
	// without a success, a single run or current evidence a provider is never below it
	connect.AssertEqual(t, providerBelowOnlineUrlRatio(onlineSuccessTestCounts(0, 5), EgressIndex{Evidence: true}, 0.8), false)
	connect.AssertEqual(t, providerBelowOnlineUrlRatio(onlineSuccessTestCounts(0, 1), EgressIndex{Evidence: true}, 0.8), false)
	connect.AssertEqual(t, providerBelowOnlineUrlRatio(onlineSuccessTestCounts(3, 5), EgressIndex{}, 0.8), false)
}

// The published score of a provider below the ratio is not online and in no
// native mode; with the ratio off the same provider is online.
func TestOnlineUrlRatioRankedScoreIsNotOnline(t *testing.T) {
	for _, c := range []struct {
		ratio  float64
		online bool
	}{
		{ratio: 0.8, online: false},
		{ratio: 0, online: true},
	} {
		settings := DefaultEgressIndexSettings()
		settings.OnlineUrlSuccessRatio = c.ratio
		providers := onlineUrlRatioTestPopulation()
		filter, nameClientIds := onlineUrlRatioTestFilter(providers, settings)
		clientId := nameClientIds["us 3 of 5"]
		decision := onlineSuccessTestDecide(filter, clientId, "us", settings)
		score := onlineSuccessTestRankedScore(filter, clientId, decision, settings)
		if score.Online != c.online || score.PassesMinimums[RankModeQuality] || score.PassesMinimums[RankModeSpeed] {
			t.Errorf("ratio %v: 3 of 5 published online=%t native=%v, want online=%t and no native mode", c.ratio, score.Online, score.PassesMinimums, c.online)
		}
		// a provider at the ratio stays online and native
		passing := onlineSuccessTestRankedScore(filter, nameClientIds["us 4 of 5"], onlineSuccessTestDecide(filter, nameClientIds["us 4 of 5"], "us", settings), settings)
		if !passing.Online || !passing.PassesMinimums[RankModeQuality] || !passing.PassesMinimums[RankModeSpeed] {
			t.Errorf("ratio %v: 4 of 5 published online=%t native=%v", c.ratio, passing.Online, passing.PassesMinimums)
		}
	}
}

// The country exemption. vn, where 1 of 5 measured providers has a success,
// is exempt. With the exemption (variant c) it keeps its 3 of 5 provider, and
// those without a success, in Online. Without it (variant a) the ratio
// applies in vn too: the 3 of 5 provider is low_success, and those without a
// success, below every ratio, are no_success. us, above the floor, loses its
// providers below the ratio either way. The guard keeps the exemption's
// counts and says what the exemption keeps.
func TestOnlineUrlRatioCountryExemption(t *testing.T) {
	belowRatio := map[string]onlineUrlRatioTestDecision{
		"us 19 of 24": onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
		"us 7 of 10":  onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
		"us 3 of 5":   onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
		"us 1 of 2":   onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
	}
	withExemption := onlineUrlRatioTestSettings("online_url_success_ratio: 0.8\n")
	connect.AssertEqual(t, withExemption.OnlineUrlRatioCountryExemption, true)
	assertOnlineUrlRatioTestDecisions(t, "with the exemption", withExemption, belowRatio)

	withoutExemption := onlineUrlRatioTestSettings("online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: false\n")
	everyCountry := map[string]onlineUrlRatioTestDecision{
		"vn 3 of 5":   onlineUrlRatioTestOut(ProviderExcludedLowSuccess),
		"vn 0 of 5 a": onlineUrlRatioTestOut(ProviderExcludedNoSuccess),
		"vn 0 of 5 b": onlineUrlRatioTestOut(ProviderExcludedNoSuccess),
		"vn 0 of 5 c": onlineUrlRatioTestOut(ProviderExcludedNoSuccess),
		"vn 0 of 5 d": onlineUrlRatioTestOut(ProviderExcludedNoSuccess),
	}
	for name, decision := range belowRatio {
		everyCountry[name] = decision
	}
	assertOnlineUrlRatioTestDecisions(t, "without the exemption", withoutExemption, everyCountry)

	// the guard: vn is exempt either way, and what its exemption keeps
	for _, c := range []struct {
		label    string
		settings *EgressIndexSettings
		kept     int
		summary  string
	}{
		{label: "ratio off", settings: DefaultEgressIndexSettings(), kept: 4, summary: "1 countries exempt keeping 4 providers [vn 1/5 kept 4]"},
		{label: "with the exemption", settings: withExemption, kept: 5, summary: "online url ratio 0.8 outside the exempt countries, 1 countries exempt keeping 5 providers [vn 1/5 kept 5]"},
		{label: "without the exemption", settings: withoutExemption, kept: 0, summary: "online url ratio 0.8 in every country, 1 countries exempt keeping 0 providers [vn 1/5 kept 0]"},
	} {
		filter, _ := onlineUrlRatioTestFilter(onlineUrlRatioTestPopulation(), c.settings)
		baseline := filter.onlineSuccessBaseline
		connect.AssertEqual(t, baseline.countryCodeCounts["vn"].Exempt, true)
		exemptCountries := baseline.exemptCountries()
		if len(exemptCountries) != 1 || exemptCountries[0].CountryCode != "vn" || exemptCountries[0].Kept != c.kept {
			t.Errorf("%s: exempt countries %+v, want vn keeping %d", c.label, exemptCountries, c.kept)
		}
		if summary := baseline.logSummary(); !strings.Contains(summary, c.summary) {
			t.Errorf("%s: log summary %q is missing %q", c.label, summary, c.summary)
		}
	}
	// with the ratio off the line is the one before the ratio
	if summary := (func() string {
		filter, _ := onlineUrlRatioTestFilter(onlineUrlRatioTestPopulation(), DefaultEgressIndexSettings())
		return filter.onlineSuccessBaseline.logSummary()
	})(); strings.Contains(summary, "online url ratio") {
		t.Errorf("log summary %q names the ratio while it is off", summary)
	}
}

// The guard counts the providers below the ratio, and decides each country on
// the share of its measured providers with a success, never on the share at
// the ratio. In xx two of three measured providers are below 0.8: a guard on
// the share at the ratio (1 of 3) would exempt xx under the 0.36 floor, but
// all three have a success, so xx is not exempt and its providers below the
// ratio leave Online.
func TestOnlineUrlRatioCountryGuardStaysOnTheSuccessShare(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	settings.OnlineUrlSuccessRatio = 0.8
	low, low2, passing, dead, warming := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	filter := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{
			low:     onlineSuccessTestCounts(1, 5),
			low2:    onlineSuccessTestCounts(3, 5),
			passing: onlineSuccessTestCounts(5, 5),
		},
		map[server.Id]string{low: "xx", low2: "xx", passing: "xx"},
		settings,
	)
	connect.AssertEqual(t, filter.onlineSuccessBaseline.countryCodeCounts["xx"], providerOnlineSuccessCount{Measured: 3, Succeeded: 3, LowSuccess: 2})
	connect.AssertEqual(t, onlineSuccessTestDecide(filter, low, "xx", settings).reason, ProviderExcludedLowSuccess)
	connect.AssertEqual(t, onlineSuccessTestDecide(filter, low2, "xx", settings).reason, ProviderExcludedLowSuccess)

	// the counts beside no_success, and none below a ratio that is off
	us := map[server.Id]ProviderEgressHealthCounts{
		low:     onlineSuccessTestCounts(3, 5),
		passing: onlineSuccessTestCounts(4, 5),
		dead:    onlineSuccessTestCounts(0, 5),
		warming: onlineSuccessTestCounts(1, 1),
	}
	usCountryCodes := map[server.Id]string{low: "us", passing: "us", dead: "us", warming: "us"}
	connect.AssertEqual(t, onlineSuccessTestFilter(us, usCountryCodes, settings).onlineSuccessBaseline.countryCodeCounts["us"], providerOnlineSuccessCount{Measured: 4, Succeeded: 3, NoSuccess: 1, LowSuccess: 1})
	connect.AssertEqual(t, onlineSuccessTestFilter(us, usCountryCodes, DefaultEgressIndexSettings()).onlineSuccessBaseline.countryCodeCounts["us"], providerOnlineSuccessCount{Measured: 4, Succeeded: 3, NoSuccess: 1})
}

// The kill switch turns the ratio off with the rule: every provider below it,
// or without a success, is in Online again in every country.
func TestOnlineUrlRatioKillSwitch(t *testing.T) {
	disabled := onlineUrlRatioTestSettings("online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: false\nonline_requires_url_success: false\n")
	connect.AssertEqual(t, disabled.OnlineUrlSuccessRatio, 0.8)
	connect.AssertEqual(t, disabled.OnlineRequiresUrlSuccess, false)
	assertOnlineUrlRatioTestDecisions(t, "kill switch", disabled, map[string]onlineUrlRatioTestDecision{
		"us 0 of 5": onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth),
		"us 0 of 2": onlineUrlRatioTestOnlineOnly(ProviderExcludedHealth),
	})
	filter, _ := onlineUrlRatioTestFilter(onlineUrlRatioTestPopulation(), disabled)
	connect.AssertEqual(t, strings.HasPrefix(filter.onlineSuccessBaseline.logSummary(), "disabled"), true)
}

// provider.yml sets the ratio and its exemption switch, each on its own key.
// A ratio that cannot be read, or one outside [0, 1], is off; an exemption
// switch that cannot be read keeps the exemption. Neither resets a
// neighbouring key, and a file that is not yaml disables the rule and leaves
// the ratio off.
func TestEgressIndexSettingsOnlineUrlRatio(t *testing.T) {
	defaults := DefaultEgressIndexSettings()
	connect.AssertEqual(t, defaults.OnlineUrlSuccessRatio, float64(0))
	connect.AssertEqual(t, defaults.OnlineUrlRatioCountryExemption, true)

	for _, c := range []struct {
		config    string
		ratio     float64
		exemption bool
	}{
		{config: "online_url_success_ratio: 0.8\n", ratio: 0.8, exemption: true},
		{config: "online_url_success_ratio: 1\n", ratio: 1, exemption: true},
		{config: "online_url_success_ratio: 0\n", ratio: 0, exemption: true},
		{config: "online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: false\n", ratio: 0.8, exemption: false},
		{config: "online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: true\n", ratio: 0.8, exemption: true},
		// unreadable or out of range: off
		{config: "online_url_success_ratio: 80%\n", ratio: 0, exemption: true},
		{config: "online_url_success_ratio: \"0.8\"\n", ratio: 0, exemption: true},
		{config: "online_url_success_ratio: 1.5\n", ratio: 0, exemption: true},
		{config: "online_url_success_ratio: -0.1\n", ratio: 0, exemption: true},
		{config: "online_url_success_ratio: .nan\n", ratio: 0, exemption: true},
		{config: "online_url_success_ratio: 0.8\nonline_url_success_ratio: 0.8\n", ratio: 0, exemption: true},
		// an unreadable exemption switch keeps the exemption
		{config: "online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: nope\n", ratio: 0.8, exemption: true},
		{config: "online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: 0\n", ratio: 0.8, exemption: true},
		{config: "online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: false\nonline_url_ratio_country_exemption: false\n", ratio: 0.8, exemption: true},
	} {
		// the neighbours of both keys keep their values whatever these are
		settings := onlineUrlRatioTestSettings(c.config + "online_success_country_floor: 0.5\nonline_success_country_floor_margin: 0.05\negress_index:\n  native_reader_enabled: true\n  max_failure_index: 9\n")
		if settings.OnlineUrlSuccessRatio != c.ratio || settings.OnlineUrlRatioCountryExemption != c.exemption {
			t.Errorf("%q read as ratio %v exemption %t, want ratio %v exemption %t", c.config, settings.OnlineUrlSuccessRatio, settings.OnlineUrlRatioCountryExemption, c.ratio, c.exemption)
		}
		if !settings.OnlineRequiresUrlSuccess || settings.OnlineSuccessCountryFloor != 0.5 || settings.OnlineSuccessCountryFloorMargin != 0.05 || !settings.NativeReaderEnabled || settings.MaxFailureIndex != 9 {
			t.Errorf("%q reset a neighbouring key: rule %t floor %v margin %v native_reader_enabled %t max_failure_index %d", c.config, settings.OnlineRequiresUrlSuccess, settings.OnlineSuccessCountryFloor, settings.OnlineSuccessCountryFloorMargin, settings.NativeReaderEnabled, settings.MaxFailureIndex)
		}
	}

	// a malformed neighbour leaves the ratio as given
	neighbour := onlineUrlRatioTestSettings("online_success_country_floor: 36%\nonline_url_success_ratio: 0.8\negress_index:\n  max_failure_index: \"nine\"\n")
	connect.AssertEqual(t, neighbour.OnlineUrlSuccessRatio, 0.8)
	connect.AssertEqual(t, neighbour.OnlineSuccessCountryFloor, defaults.OnlineSuccessCountryFloor)
	connect.AssertEqual(t, neighbour.MaxFailureIndex, defaults.MaxFailureIndex)

	// a tab in the indentation makes the file unreadable: the defaults, the
	// rule disabled and the ratio off
	tabbed := onlineUrlRatioTestSettings("egress_index:\n\tnative_reader_enabled: true\nonline_url_success_ratio: 0.8\n")
	connect.AssertEqual(t, tabbed.OnlineUrlSuccessRatio, float64(0))
	connect.AssertEqual(t, tabbed.OnlineUrlRatioCountryExemption, true)
	connect.AssertEqual(t, tabbed.OnlineRequiresUrlSuccess, false)
	connect.AssertEqual(t, tabbed.NativeReaderEnabled, false)
}

// The dashboard snapshot key carries the ratio and its exemption switch, so
// workers that count under different ratios never share a snapshot, and the
// ratio off reads the same however provider.yml says so.
func TestProviderEgressCountsPolicyKeyCarriesTheOnlineUrlRatio(t *testing.T) {
	key := func(config string) string {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
		defer pop()
		return ProviderEgressCountsPolicyKey(0)
	}
	off := key("")
	ratio := key("online_url_success_ratio: 0.8\n")
	if ratio == off {
		t.Fatal("the ratio reused the snapshot key of the ratio off")
	}
	if key("online_url_success_ratio: 0.7\n") == ratio {
		t.Fatal("another ratio reused the snapshot key")
	}
	if key("online_url_success_ratio: 0.8\nonline_url_ratio_country_exemption: false\n") == ratio {
		t.Fatal("the ratio without the country exemption reused the snapshot key")
	}
	connect.AssertEqual(t, key("online_url_success_ratio: 0\n"), off)
	connect.AssertEqual(t, key("online_url_success_ratio: 80%\n"), off)
}

// A guard with the ratio on is published as version 3, with the ratio, the
// exemption switch and each country's providers below the ratio, and a
// reader reads back exactly what the export applied. A version 3 document
// without any of those is not trusted.
func TestProviderOnlineSuccessBaselineDocumentVersion3RoundTrips(t *testing.T) {
	baseline := onlineSuccessTestBaseline(0.36, map[string]providerOnlineSuccessCount{
		"us": {Measured: 10, Succeeded: 8, NoSuccess: 2, LowSuccess: 3},
		"vn": {Measured: 10, Succeeded: 1, NoSuccess: 9, LowSuccess: 1},
	})
	baseline.margin = 0.03
	baseline.urlSuccessRatio = 0.8
	baseline.urlRatioCountryExemption = false
	publishTime := onlineSuccessTestNow.Add(5 * time.Minute)
	value, err := encodeProviderOnlineSuccessBaseline(baseline, publishTime)
	connect.AssertEqual(t, err, nil)
	var fields map[string]any
	connect.AssertEqual(t, json.Unmarshal(value, &fields), nil)
	connect.AssertEqual(t, fields["version"], float64(3))
	connect.AssertEqual(t, fields["url_success_ratio"], 0.8)
	connect.AssertEqual(t, fields["url_ratio_country_exemption"], false)

	decoded, err := decodeProviderOnlineSuccessBaseline(value, publishTime.Add(time.Minute))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, decoded.countryCodeCounts, baseline.countryCodeCounts)
	connect.AssertEqual(t, decoded.enabled, true)
	connect.AssertEqual(t, decoded.floor, 0.36)
	connect.AssertEqual(t, decoded.margin, 0.03)
	connect.AssertEqual(t, decoded.urlSuccessRatio, 0.8)
	connect.AssertEqual(t, decoded.urlRatioCountryExemption, false)
	// the reader applies the ratio where the export did: everywhere, here
	connect.AssertEqual(t, decoded.urlRatioAppliesIn("vn"), true)
	connect.AssertEqual(t, decoded.noSuccessAppliesIn("vn"), true)
	decoded.urlRatioCountryExemption = true
	connect.AssertEqual(t, decoded.urlRatioAppliesIn("vn"), false)
	connect.AssertEqual(t, decoded.urlRatioAppliesIn("us"), true)

	// a version 3 document written by hand with every field is trusted, so
	// each one below is refused for its own defect
	document := func(ratio string, exemption string, lowSuccess string) string {
		return fmt.Sprintf(`{"version":3,"compute_time":"2026-10-13T21:00:00Z","publish_time":"2026-10-13T21:00:00Z","enabled":true,"floor":0.36,"margin":0.03,"countries":{"us":{"measured":4,"succeeded":2,"no_success":2,"exempt":false%s}}%s%s}`, lowSuccess, ratio, exemption)
	}
	_, err = decodeProviderOnlineSuccessBaseline([]byte(document(`,"url_success_ratio":0.8`, `,"url_ratio_country_exemption":true`, `,"low_success":1`)), onlineSuccessTestNow)
	connect.AssertEqual(t, err, nil)
	for name, untrusted := range map[string]string{
		"no ratio":                 document(``, `,"url_ratio_country_exemption":true`, `,"low_success":1`),
		"ratio out of range":       document(`,"url_success_ratio":1.5`, `,"url_ratio_country_exemption":true`, `,"low_success":1`),
		"negative ratio":           document(`,"url_success_ratio":-0.1`, `,"url_ratio_country_exemption":true`, `,"low_success":1`),
		"no exemption switch":      document(`,"url_success_ratio":0.8`, ``, `,"low_success":1`),
		"no count below the ratio": document(`,"url_success_ratio":0.8`, `,"url_ratio_country_exemption":true`, ``),
		"more below than succeed":  document(`,"url_success_ratio":0.8`, `,"url_ratio_country_exemption":true`, `,"low_success":3`),
		"negative below the ratio": document(`,"url_success_ratio":0.8`, `,"url_ratio_country_exemption":true`, `,"low_success":-1`),
	} {
		if _, err := decodeProviderOnlineSuccessBaseline([]byte(untrusted), onlineSuccessTestNow); err == nil {
			t.Errorf("%s: a reader accepted an untrustworthy version 3 guard", name)
		}
	}
}

// With the ratio off the export publishes version 2, byte for byte the
// document the code before the ratio publishes, so a reader of either code
// trusts it during a rollout or after a rollback. A version 2 guard, from
// either code, reads as the rule with the ratio off and is no reason to log:
// the rollout state of the ratio.
func TestProviderOnlineSuccessBaselineVersion2ReadsAsTheRatioOff(t *testing.T) {
	baseline := onlineSuccessTestBaseline(0.36, map[string]providerOnlineSuccessCount{
		"us": {Measured: 2, Succeeded: 1, NoSuccess: 1},
	})
	baseline.margin = 0.03
	value, err := encodeProviderOnlineSuccessBaseline(baseline, onlineSuccessTestNow.Add(5*time.Minute))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(value), `{"version":2,"compute_time":"2026-10-13T21:00:00Z","publish_time":"2026-10-13T21:05:00Z","enabled":true,"floor":0.36,"margin":0.03,"countries":{"us":{"measured":2,"succeeded":1,"no_success":1,"exempt":false}}}`)

	// as the code before the ratio wrote it, with the margin holding vn
	previous, err := decodeProviderOnlineSuccessBaseline([]byte(`{"version":2,"compute_time":"2026-10-13T20:55:00Z","publish_time":"2026-10-13T20:58:00Z","enabled":true,"floor":0.36,"margin":0.03,"countries":{"us":{"measured":2,"succeeded":1,"no_success":1,"exempt":false},"vn":{"measured":25,"succeeded":9,"no_success":16,"exempt":true}}}`), onlineSuccessTestNow)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, previous.urlSuccessRatio, float64(0))
	connect.AssertEqual(t, previous.urlRatioCountryExemption, true)
	connect.AssertEqual(t, previous.urlRatioAppliesIn("us"), false)
	connect.AssertEqual(t, previous.appliesIn("us"), true)
	connect.AssertEqual(t, previous.exemptCountryCodes(), map[string]bool{"vn": true})
	// its verdicts are the previous state the margin holds: vn at 9 of 25,
	// the floor, stays exempt
	filter := onlineSuccessTestCountry("vn", 25, 9)
	connect.AssertEqual(t, newProviderOnlineSuccessBaseline(filter, onlineSuccessTestCountryCodes(filter, "vn"), DefaultEgressIndexSettings(), previous).exempt("vn"), true)
}

// A provider below the ratio is told why and what to fix, and lists no mode
// weight, since it is in no pool. With the ratio off it is online-only for
// its failing ratio.
func TestProviderStatusExplainsLowUrlSuccess(t *testing.T) {
	low, healthy := server.NewId(), server.NewId()
	countryCode := "us"
	facts := &providerStatusFacts{
		clientId:                 low,
		networkId:                server.NewId(),
		providePublic:            true,
		hasRollup:                true,
		connected:                true,
		valid:                    true,
		minRelativeLatencyMillis: 10,
		maxBytesPerSecond:        100 * 1024 * 1024,
		hasLatencyTest:           true,
		hasSpeedTest:             true,
		publishedCountryCode:     &countryCode,
		lookbacks:                providerStatusNeutralLookbacks(),
	}
	minimums := providerReliabilityMinimums()
	clientIdCounts := map[server.Id]ProviderEgressHealthCounts{low: onlineSuccessTestCounts(3, 5), healthy: onlineSuccessTestCounts(5, 5)}
	clientIdCountryCodes := map[server.Id]string{low: "us", healthy: "us"}

	settings := DefaultEgressIndexSettings()
	settings.OnlineUrlSuccessRatio = 0.8
	filter := onlineSuccessTestFilter(clientIdCounts, clientIdCountryCodes, settings)
	evaluation := evaluateProviderStatus(facts, filter, settings, false, minimums)
	connect.AssertEqual(t, evaluation.reason, ProviderStatusReasonEgressLowSuccess)
	status := newProviderStatus(facts, evaluation, filter, settings, minimums, onlineSuccessTestNow)
	connect.AssertEqual(t, status.Reason, "egress_low_success")
	if !strings.Contains(status.ReasonText, "failed to load") || !strings.Contains(status.ReasonText, "aren't offered") || !strings.Contains(status.ReasonText, "firewall") {
		t.Fatalf("reason text %q does not say what happened and what to fix", status.ReasonText)
	}
	connect.AssertEqual(t, status.Admission.Egress, ProviderEgressFail)
	for _, number := range status.Ranking {
		if number.Name == ProviderStatusNumberWeightQuality || number.Name == ProviderStatusNumberWeightSpeed {
			t.Fatalf("a provider out of every pool lists the mode weight %s", number.Name)
		}
	}

	off := onlineSuccessTestFilter(clientIdCounts, clientIdCountryCodes, DefaultEgressIndexSettings())
	connect.AssertEqual(t, evaluateProviderStatus(facts, off, DefaultEgressIndexSettings(), false, minimums).reason, ProviderStatusReasonEgressFailing)
}

// A status follows the ratio the score export published, never the reader's
// own provider.yml: a version 3 guard at 0.8 holds a 3 of 5 provider below
// it whatever the reader sets, and a version 2 guard, the ratio off, holds
// it nowhere.
func TestProviderStatusFollowsThePublishedUrlRatio(t *testing.T) {
	low := server.NewId()
	countryCode := "us"
	facts := &providerStatusFacts{
		clientId:                 low,
		networkId:                server.NewId(),
		providePublic:            true,
		hasRollup:                true,
		connected:                true,
		valid:                    true,
		minRelativeLatencyMillis: 10,
		maxBytesPerSecond:        100 * 1024 * 1024,
		hasLatencyTest:           true,
		hasSpeedTest:             true,
		publishedCountryCode:     &countryCode,
		lookbacks:                providerStatusNeutralLookbacks(),
	}
	filter := providerCountFilter{
		now:                     onlineSuccessTestNow,
		arinRisk:                map[server.Id]bool{},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		tlsAuthenticationFailed: map[server.Id]bool{},
		countryCodes:            map[server.Id]string{},
		healthCounts:            map[server.Id]ProviderEgressHealthCounts{low: onlineSuccessTestCounts(3, 5)},
	}
	minimums := providerReliabilityMinimums()
	published := func(document string) *providerOnlineSuccessBaseline {
		baseline, err := decodeProviderOnlineSuccessBaseline([]byte(document), onlineSuccessTestNow)
		connect.AssertEqual(t, err, nil)
		return baseline
	}

	filter.onlineSuccessBaseline = published(`{"version":3,"compute_time":"2026-10-13T20:55:00Z","publish_time":"2026-10-13T20:58:00Z","enabled":true,"floor":0.36,"margin":0.03,"countries":{"us":{"measured":2,"succeeded":2,"no_success":0,"exempt":false,"low_success":1}},"url_success_ratio":0.8,"url_ratio_country_exemption":true}`)
	for _, config := range []string{"", "online_url_success_ratio: 0\n", "online_requires_url_success: false\n"} {
		if reason := evaluateProviderStatus(facts, filter, onlineUrlRatioTestSettings(config), false, minimums).reason; reason != ProviderStatusReasonEgressLowSuccess {
			t.Errorf("export at 0.8, reader provider.yml %q: reason %s, want %s", config, reason, ProviderStatusReasonEgressLowSuccess)
		}
	}
	filter.onlineSuccessBaseline = published(`{"version":2,"compute_time":"2026-10-13T20:55:00Z","publish_time":"2026-10-13T20:58:00Z","enabled":true,"floor":0.36,"margin":0.03,"countries":{"us":{"measured":2,"succeeded":2,"no_success":0,"exempt":false}}}`)
	if reason := evaluateProviderStatus(facts, filter, onlineUrlRatioTestSettings("online_url_success_ratio: 0.8\n"), false, minimums).reason; reason != ProviderStatusReasonEgressFailing {
		t.Errorf("export with the ratio off, reader at 0.8: reason %s, want %s", reason, ProviderStatusReasonEgressFailing)
	}
}
