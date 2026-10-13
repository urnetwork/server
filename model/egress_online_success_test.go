package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The URL-success rule (egress_online_success.go), pure: the facts a pass
// gathers from its loaded evidence, the decision, the published score, the
// country guard, the kill switch and the guard's published document.

// A fixed evaluation time, so no test reads the clock.
var onlineSuccessTestNow = time.Date(2026, 10, 13, 21, 0, 0, 0, time.UTC)

// `total` accepted measured URL runs within the last hour, `ok` of them
// successes.
func onlineSuccessTestCounts(ok int, total int) ProviderEgressHealthCounts {
	return ProviderEgressHealthCounts{
		MeasuredAt:      onlineSuccessTestNow.Add(-time.Minute),
		FirstMeasuredAt: onlineSuccessTestNow.Add(-time.Hour),
		OKCount:         ok,
		Total:           total,
	}
}

// A pass's loaded evidence for the given providers, with the country guard
// counted over clientIdCountryCodes, as a fleet pass counts its population.
func onlineSuccessTestFilter(
	clientIdCounts map[server.Id]ProviderEgressHealthCounts,
	clientIdCountryCodes map[server.Id]string,
	settings *EgressIndexSettings,
) providerCountFilter {
	filter := providerCountFilter{
		now:                     onlineSuccessTestNow,
		arinRisk:                map[server.Id]bool{},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		tlsAuthenticationFailed: map[server.Id]bool{},
		countryCodes:            map[server.Id]string{},
		healthCounts:            clientIdCounts,
	}
	filter.onlineSuccessBaseline = newProviderOnlineSuccessBaseline(filter, clientIdCountryCodes, settings)
	return filter
}

// The provider's decision as every pass makes it.
func onlineSuccessTestDecide(filter providerCountFilter, clientId server.Id, countryCode string, settings *EgressIndexSettings) providerEgressDecision {
	return decideProviderEgress(filter.egressFacts(clientId, &countryCode, nil, nil, settings), false)
}

// A fast, reliable provider's pool score, ranked with the decision the way
// UpdateClientScores ranks every score it publishes.
func onlineSuccessTestRankedScore(filter providerCountFilter, clientId server.Id, decision providerEgressDecision, settings *EgressIndexSettings) *ClientScore {
	lookbackClientScore := &ClientScore{
		ClientId:                     clientId,
		ReliabilityWeight:            1,
		IndependentReliabilityWeight: 1,
		MinRelativeLatencyMillis:     10,
		MaxBytesPerSecond:            100 * 1024 * 1024,
		HasLatencyTest:               true,
		HasSpeedTest:                 true,
		Scores:                       map[string]int{},
		Tiers:                        map[string]int{},
	}
	index := 0
	rankModeBases, rankModeMinimumBases := clientScoreRankModeBases(0, 0, &index)
	setClientScoreRanks(lookbackClientScore, rankModeBases, rankModeMinimumBases, 10, 100*1024*1024, true, true)
	clientScore := &ClientScore{
		ClientId:             clientId,
		LookbackClientScores: map[int]*ClientScore{0: lookbackClientScore},
	}
	rankClientScore(clientScore, decision, filter, settings, providerReliabilityMinimums())
	return clientScore
}

// A provider measured repeatedly without one success leaves Online and every
// bucket, the counts included, in a country where the probe works. It is no
// hard exclusion: probe admission, explicit client ids and force_minimum read
// only the common gates.
func TestOnlineUrlSuccessRuleExcludesDeadProvider(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	dead, healthy := server.NewId(), server.NewId()
	filter := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{
			dead:    onlineSuccessTestCounts(0, 5),
			healthy: onlineSuccessTestCounts(5, 5),
		},
		map[server.Id]string{dead: "us", healthy: "us"},
		settings,
	)

	decision := onlineSuccessTestDecide(filter, dead, "us", settings)
	if decision != (providerEgressDecision{reason: ProviderExcludedNoSuccess}) {
		t.Fatalf("dead provider decision %+v, want out of every bucket and the counts for no_success", decision)
	}
	if filter.hasHardEgressFailure(dead) {
		t.Fatal("the URL-success rule became a common hard exclusion, which stops probes and explicit requests")
	}
	deadScore := onlineSuccessTestRankedScore(filter, dead, decision, settings)
	if deadScore.Online || deadScore.PassesMinimums[RankModeQuality] || deadScore.PassesMinimums[RankModeSpeed] {
		t.Fatalf("dead provider published online=%t native=%v", deadScore.Online, deadScore.PassesMinimums)
	}

	healthyDecision := onlineSuccessTestDecide(filter, healthy, "us", settings)
	healthyScore := onlineSuccessTestRankedScore(filter, healthy, healthyDecision, settings)
	if !healthyScore.Online || !healthyScore.PassesMinimums[RankModeQuality] || !healthyScore.PassesMinimums[RankModeSpeed] {
		t.Fatalf("healthy provider published online=%t native=%v", healthyScore.Online, healthyScore.PassesMinimums)
	}

	// the smallest dead history the rule reads
	connect.AssertEqual(t, ProviderOnlineMinMeasuredUrlRuns, 2)
	filter.healthCounts[dead] = onlineSuccessTestCounts(0, ProviderOnlineMinMeasuredUrlRuns)
	connect.AssertEqual(t, onlineSuccessTestDecide(filter, dead, "us", settings).reason, ProviderExcludedNoSuccess)
}

// An unprobed provider and one with a single failed run keep the benefit of
// the doubt: online, counted, with a neutral or measured URL weight, in a
// country where the rule excludes a dead provider beside them.
func TestOnlineUrlSuccessRuleKeepsUnprobedAndWarmingProviders(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	dead, healthy, healthy2, warming, unprobed := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	filter := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{
			dead:     onlineSuccessTestCounts(0, 5),
			healthy:  onlineSuccessTestCounts(5, 5),
			healthy2: onlineSuccessTestCounts(4, 5),
			warming:  onlineSuccessTestCounts(0, 1),
		},
		map[server.Id]string{dead: "us", healthy: "us", healthy2: "us", warming: "us", unprobed: "us"},
		settings,
	)
	// the warming run counts as measured: two of four have a success
	connect.AssertEqual(t, filter.onlineSuccessBaseline.countryCodeCounts["us"], providerOnlineSuccessCount{Measured: 4, Succeeded: 2, NoSuccess: 1})
	connect.AssertEqual(t, onlineSuccessTestDecide(filter, dead, "us", settings).reason, ProviderExcludedNoSuccess)

	for name, c := range map[string]struct {
		clientId server.Id
		reason   string
		weight   float64
	}{
		"unprobed": {clientId: unprobed, reason: ProviderExcludedUnprobed, weight: 1},
		"warming":  {clientId: warming, reason: ProviderExcludedHealth, weight: 0.1},
	} {
		decision := onlineSuccessTestDecide(filter, c.clientId, "us", settings)
		if decision.reason != c.reason || !decision.online || !decision.counted || decision.quality || decision.speed || decision.hardExcluded {
			t.Errorf("%s: decision %+v, want online and counted for %s", name, decision, c.reason)
		}
		score := onlineSuccessTestRankedScore(filter, c.clientId, decision, settings)
		if !score.Online || score.UrlProbeSuccessWeight != c.weight {
			t.Errorf("%s: published online=%t weight=%v, want online with weight %v", name, score.Online, score.UrlProbeSuccessWeight, c.weight)
		}
	}
}

// One success anywhere in the window restores the provider to Online, however
// many failures surround it; it stays out of the native buckets on its ratio.
func TestOnlineUrlSuccessRuleOneSuccessRestoresTheDecision(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	recovering, healthy := server.NewId(), server.NewId()
	clientIdCountryCodes := map[server.Id]string{recovering: "us", healthy: "us"}

	before := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{recovering: onlineSuccessTestCounts(0, 10), healthy: onlineSuccessTestCounts(5, 5)},
		clientIdCountryCodes,
		settings,
	)
	connect.AssertEqual(t, onlineSuccessTestDecide(before, recovering, "us", settings).online, false)

	after := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{recovering: onlineSuccessTestCounts(1, 11), healthy: onlineSuccessTestCounts(5, 5)},
		clientIdCountryCodes,
		settings,
	)
	decision := onlineSuccessTestDecide(after, recovering, "us", settings)
	if decision.reason != ProviderExcludedHealth || !decision.online || !decision.counted || decision.quality || decision.speed {
		t.Fatalf("recovered provider decision %+v, want online-only on its failing ratio", decision)
	}
}

// The kill switch, in settings and in provider.yml, returns every dead
// provider to Online in every country.
func TestOnlineUrlSuccessRuleKillSwitch(t *testing.T) {
	disabled := func() *EgressIndexSettings {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("online_requires_url_success: false\n"))
		defer pop()
		return egressIndexSettings()
	}()
	connect.AssertEqual(t, disabled.OnlineRequiresUrlSuccess, false)
	connect.AssertEqual(t, disabled.OnlineSuccessCountryFloor, DefaultEgressIndexSettings().OnlineSuccessCountryFloor)

	dead, healthy := server.NewId(), server.NewId()
	filter := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{dead: onlineSuccessTestCounts(0, 5), healthy: onlineSuccessTestCounts(5, 5)},
		map[server.Id]string{dead: "us", healthy: "us"},
		disabled,
	)
	decision := onlineSuccessTestDecide(filter, dead, "us", disabled)
	if decision.reason != ProviderExcludedHealth || !decision.online || !decision.counted {
		t.Fatalf("kill switch left the decision %+v, want the dead provider online for health", decision)
	}
	if !onlineSuccessTestRankedScore(filter, dead, decision, disabled).Online {
		t.Fatal("kill switch left the dead provider out of the published Online bucket")
	}
}

// The country guard: a dead provider keeps Online where fewer than the floor of
// the measured providers have a success, and leaves it where the probe works.
// The boundary share is not exempt, a country nothing measured is, and so is
// every country for a reader without a published guard.
func TestOnlineUrlSuccessCountryGuard(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	usDead, usHealthy := server.NewId(), server.NewId()
	vnDead, vnDead2, vnDead3, vnDead4, vnHealthy := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	clientIdCounts := map[server.Id]ProviderEgressHealthCounts{
		usDead:    onlineSuccessTestCounts(0, 5),
		usHealthy: onlineSuccessTestCounts(5, 5),
		vnDead:    onlineSuccessTestCounts(0, 5),
		vnDead2:   onlineSuccessTestCounts(0, 3),
		vnDead3:   onlineSuccessTestCounts(0, 7),
		vnDead4:   onlineSuccessTestCounts(0, 2),
		vnHealthy: onlineSuccessTestCounts(4, 5),
	}
	clientIdCountryCodes := map[server.Id]string{
		usDead: "us", usHealthy: "us",
		vnDead: "vn", vnDead2: "vn", vnDead3: "vn", vnDead4: "vn", vnHealthy: "vn",
	}
	filter := onlineSuccessTestFilter(clientIdCounts, clientIdCountryCodes, settings)

	// vn: 1 of 5 measured providers has a success, under the 0.36 floor
	connect.AssertEqual(t, filter.onlineSuccessBaseline.countryCodeCounts["vn"], providerOnlineSuccessCount{Measured: 5, Succeeded: 1, NoSuccess: 4})
	vnDecision := onlineSuccessTestDecide(filter, vnDead, "vn", settings)
	if vnDecision.reason != ProviderExcludedHealth || !vnDecision.online || !vnDecision.counted {
		t.Fatalf("exempt country decision %+v, want the dead provider online for health", vnDecision)
	}
	usDecision := onlineSuccessTestDecide(filter, usDead, "us", settings)
	connect.AssertEqual(t, usDecision.reason, ProviderExcludedNoSuccess)
	connect.AssertEqual(t, usDecision.online, false)

	// the comparison is share < floor
	baseline := &providerOnlineSuccessBaseline{countryCodeCounts: map[string]providerOnlineSuccessCount{
		"at":    {Measured: 4, Succeeded: 2},
		"under": {Measured: 3, Succeeded: 1},
	}}
	connect.AssertEqual(t, baseline.exempt("at", 0.5), false)
	connect.AssertEqual(t, baseline.exempt("under", 0.5), true)
	connect.AssertEqual(t, baseline.exempt("unmeasured", 0.5), true)
	connect.AssertEqual(t, baseline.exempt("at", 0), false)
	var unpublished *providerOnlineSuccessBaseline
	connect.AssertEqual(t, unpublished.exempt("at", 0), true)

	// a scoped reader without a published guard exempts every country
	filter.onlineSuccessBaseline = nil
	connect.AssertEqual(t, onlineSuccessTestDecide(filter, usDead, "us", settings).online, true)

	// the kill switch disables the rule in every country
	disabled := *settings
	disabled.OnlineRequiresUrlSuccess = false
	filter = onlineSuccessTestFilter(clientIdCounts, clientIdCountryCodes, &disabled)
	for _, c := range []struct {
		clientId    server.Id
		countryCode string
	}{{clientId: usDead, countryCode: "us"}, {clientId: vnDead, countryCode: "vn"}} {
		if !onlineSuccessTestDecide(filter, c.clientId, c.countryCode, &disabled).online {
			t.Errorf("%s: kill switch left a dead provider out of Online", c.countryCode)
		}
	}
}

// The guard counts only providers the common gates admit with current URL
// evidence; unmeasured, stale and hard-excluded providers say nothing about
// the probe in their country.
func TestOnlineUrlSuccessBaselineCountsMeasuredCommonGatePasses(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	settings.EvidenceMaxAge = 30 * time.Minute
	measured, dead, unmeasured, stale, unreliable, risky, intercepted := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	staleCounts := onlineSuccessTestCounts(3, 3)
	staleCounts.MeasuredAt = onlineSuccessTestNow.Add(-time.Hour)
	filter := providerCountFilter{
		now: onlineSuccessTestNow,
		healthCounts: map[server.Id]ProviderEgressHealthCounts{
			measured:    onlineSuccessTestCounts(1, 3),
			dead:        onlineSuccessTestCounts(0, 3),
			stale:       staleCounts,
			unreliable:  onlineSuccessTestCounts(3, 3),
			risky:       onlineSuccessTestCounts(3, 3),
			intercepted: onlineSuccessTestCounts(3, 3),
		},
		reliabilityFailed:       map[server.Id]bool{unreliable: true},
		arinRisk:                map[server.Id]bool{risky: true},
		tlsAuthenticationFailed: map[server.Id]bool{intercepted: true},
	}
	clientIdCountryCodes := map[server.Id]string{}
	for _, clientId := range []server.Id{measured, dead, unmeasured, stale, unreliable, risky, intercepted} {
		clientIdCountryCodes[clientId] = "us"
	}
	baseline := newProviderOnlineSuccessBaseline(filter, clientIdCountryCodes, settings)
	connect.AssertEqual(t, baseline.countryCodeCounts, map[string]providerOnlineSuccessCount{
		"us": {Measured: 2, Succeeded: 1, NoSuccess: 1},
	})
	connect.AssertEqual(t, baseline.computeTime, onlineSuccessTestNow)
}

// provider.yml sets the rule and its floor beside the egress_index block, and
// a floor outside [0, 1] keeps the default.
func TestEgressIndexSettingsOnlineUrlSuccess(t *testing.T) {
	load := func(config string) *EgressIndexSettings {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
		defer pop()
		return egressIndexSettings()
	}
	defaults := DefaultEgressIndexSettings()
	connect.AssertEqual(t, defaults.OnlineRequiresUrlSuccess, true)
	connect.AssertEqual(t, defaults.OnlineSuccessCountryFloor, 0.36)

	overridden := load("online_requires_url_success: false\nonline_success_country_floor: 0.5\negress_index:\n  max_failure_index: 9\n")
	connect.AssertEqual(t, overridden.OnlineRequiresUrlSuccess, false)
	connect.AssertEqual(t, overridden.OnlineSuccessCountryFloor, 0.5)
	connect.AssertEqual(t, overridden.MaxFailureIndex, 9)
	connect.AssertEqual(t, load("online_success_country_floor: 0\n").OnlineSuccessCountryFloor, float64(0))
	connect.AssertEqual(t, load("online_success_country_floor: 1\n").OnlineSuccessCountryFloor, float64(1))
	for _, config := range []string{
		"online_success_country_floor: 1.5\n",
		"online_success_country_floor: -0.1\n",
		"online_success_country_floor: .nan\n",
	} {
		if floor := load(config).OnlineSuccessCountryFloor; floor != defaults.OnlineSuccessCountryFloor {
			t.Errorf("%q set the floor to %v, want the default %v", config, floor, defaults.OnlineSuccessCountryFloor)
		}
	}
	// a document that is not yaml keeps every default, the rule included
	connect.AssertEqual(t, load("online_requires_url_success: [unterminated\n"), defaults)
}

// The published guard round-trips its counts, a reader refuses a document it
// must not trust, and the exempt list and log line name what the guard keeps.
func TestProviderOnlineSuccessBaselineDocument(t *testing.T) {
	baseline := &providerOnlineSuccessBaseline{
		computeTime: onlineSuccessTestNow,
		countryCodeCounts: map[string]providerOnlineSuccessCount{
			"us": {Measured: 10, Succeeded: 8, NoSuccess: 2},
			"vn": {Measured: 10, Succeeded: 1, NoSuccess: 9},
			"":   {Measured: 2, Succeeded: 0, NoSuccess: 2},
		},
	}
	value, err := json.Marshal(&providerOnlineSuccessBaselineDocument{
		Version:     providerOnlineSuccessBaselineVersion,
		ComputeTime: baseline.computeTime,
		Countries:   baseline.countryCodeCounts,
	})
	connect.AssertEqual(t, err, nil)
	decoded, err := decodeProviderOnlineSuccessBaseline(value)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, decoded.countryCodeCounts, baseline.countryCodeCounts)
	connect.AssertEqual(t, decoded.computeTime.Equal(onlineSuccessTestNow), true)

	for name, document := range map[string]string{
		"version":      `{"version":2,"compute_time":"2026-10-13T21:00:00Z","countries":{}}`,
		"no countries": `{"version":1,"compute_time":"2026-10-13T21:00:00Z"}`,
		"inconsistent": `{"version":1,"compute_time":"2026-10-13T21:00:00Z","countries":{"us":{"measured":2,"succeeded":2,"no_success":1}}}`,
		"negative":     `{"version":1,"compute_time":"2026-10-13T21:00:00Z","countries":{"us":{"measured":2,"succeeded":-1,"no_success":0}}}`,
		"not json":     `{"version":1,`,
	} {
		if _, err := decodeProviderOnlineSuccessBaseline([]byte(document)); err == nil {
			t.Errorf("%s: a reader accepted an untrustworthy guard", name)
		}
	}

	exemptCountries := baseline.exemptCountries(0.36)
	connect.AssertEqual(t, len(exemptCountries), 2)
	connect.AssertEqual(t, *exemptCountries[0], ProviderOnlineSuccessExemptCountry{CountryCode: "", Measured: 2, Succeeded: 0, Kept: 2})
	connect.AssertEqual(t, *exemptCountries[1], ProviderOnlineSuccessExemptCountry{CountryCode: "vn", Measured: 10, Succeeded: 1, Kept: 9})

	settings := DefaultEgressIndexSettings()
	summary := baseline.logSummary(settings)
	for _, part := range []string{"country floor 0.36", "2 countries exempt keeping 11 providers", "unknown 0/2 kept 2", "vn 1/10 kept 9"} {
		if !strings.Contains(summary, part) {
			t.Errorf("log summary %q is missing %q", summary, part)
		}
	}
	settings.OnlineRequiresUrlSuccess = false
	connect.AssertEqual(t, strings.HasPrefix(baseline.logSummary(settings), "disabled"), true)
}

// The status of a provider the rule excludes says what to fix; with the kill
// switch the same provider is online-only for its failing ratio.
func TestProviderStatusExplainsNoUrlSuccess(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	dead, healthy := server.NewId(), server.NewId()
	countryCode := "us"
	facts := &providerStatusFacts{
		clientId:                 dead,
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
	filter := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{dead: onlineSuccessTestCounts(0, 5), healthy: onlineSuccessTestCounts(5, 5)},
		map[server.Id]string{dead: "us", healthy: "us"},
		settings,
	)
	minimums := providerReliabilityMinimums()

	evaluation := evaluateProviderStatus(facts, filter, settings, false, minimums)
	connect.AssertEqual(t, evaluation.reason, ProviderStatusReasonEgressNoSuccess)
	status := newProviderStatus(facts, evaluation, filter, settings, minimums, onlineSuccessTestNow)
	if !strings.Contains(status.ReasonText, "isn't passing traffic") || !strings.Contains(status.ReasonText, "firewall") {
		t.Fatalf("reason text %q does not say what to fix", status.ReasonText)
	}
	connect.AssertEqual(t, status.Admission.Egress, ProviderEgressFail)
	for _, number := range status.Ranking {
		if number.Name == ProviderStatusNumberWeightQuality || number.Name == ProviderStatusNumberWeightSpeed {
			t.Fatalf("a provider out of every pool lists the mode weight %s", number.Name)
		}
	}

	disabled := *settings
	disabled.OnlineRequiresUrlSuccess = false
	connect.AssertEqual(t, evaluateProviderStatus(facts, filter, &disabled, false, minimums).reason, ProviderStatusReasonEgressFailing)
}
