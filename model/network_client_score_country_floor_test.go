// The country floor (network_client_score_country_floor.go), pure: which
// providers the URL bar alone keeps out of Online, the order the floor admits
// them in, what it admits and to which copies, what it never admits, its
// kill switch, and what it publishes. Every value below is synthetic.
package model

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// One provider of a floor test as the export ranks it: its own decision's
// buckets, its tier and score in both modes, its URL success weight and
// reliability.
type countryFloorTestProvider struct {
	quality           bool
	speed             bool
	online            bool
	tier              int
	score             int
	urlWeight         float64
	reliabilityWeight float64
	passesReliability bool
	networkOnly       bool
	downgrades        uint8
}

// A ranked score of the provider, one copy of it.
func countryFloorTestScore(clientId server.Id, provider countryFloorTestProvider) *ClientScore {
	clientScore := &ClientScore{
		ClientId:              clientId,
		NetworkOnly:           provider.networkOnly,
		Online:                provider.online,
		PassesMinimums:        map[string]bool{},
		ScaledWeights:         map[string]float32{},
		Tiers:                 map[string]int{RankModeQuality: provider.tier, RankModeSpeed: provider.tier},
		Scores:                map[string]int{RankModeQuality: provider.score, RankModeSpeed: provider.score},
		UrlProbeSuccessWeight: provider.urlWeight,
		ReliabilityWeight:     provider.reliabilityWeight,
		CapabilityDowngrades:  provider.downgrades,
		passesReliability:     provider.passesReliability,
	}
	if provider.quality {
		clientScore.PassesMinimums[RankModeQuality] = true
		clientScore.ScaledWeights[RankModeQuality] = float32(provider.urlWeight)
	}
	if provider.speed {
		clientScore.PassesMinimums[RankModeSpeed] = true
		clientScore.ScaledWeights[RankModeSpeed] = float32(provider.urlWeight)
	}
	return clientScore
}

// above the bar in every bucket
var countryFloorTestNative = countryFloorTestProvider{quality: true, speed: true, online: true, tier: 0, score: 0, urlWeight: 1, reliabilityWeight: 1, passesReliability: true}

// below the bar, out of every bucket (low_success), with the URL weight of
// its share of successes
func countryFloorTestBelow(share float64) countryFloorTestProvider {
	return countryFloorTestProvider{tier: 2, score: 50, urlWeight: 0.1 + 0.9*share, reliabilityWeight: 1, passesReliability: true}
}

// The export's maps for one country, its region and city holding the same
// providers, and one group holding them too; each map has its own copies.
type countryFloorTestMaps struct {
	countryId                 server.Id
	cityId                    server.Id
	groupId                   server.Id
	locationClientScores      map[server.Id]map[server.Id]*ClientScore
	locationGroupClientScores map[server.Id]map[server.Id]*ClientScore
	countryCodes              map[server.Id]string
}

// The maps of one country holding providers, a copy of each in each map.
func newCountryFloorTestMaps(countryCode string, providers map[server.Id]countryFloorTestProvider) *countryFloorTestMaps {
	testMaps := &countryFloorTestMaps{
		countryId:                 server.NewId(),
		cityId:                    server.NewId(),
		groupId:                   server.NewId(),
		locationClientScores:      map[server.Id]map[server.Id]*ClientScore{},
		locationGroupClientScores: map[server.Id]map[server.Id]*ClientScore{},
	}
	testMaps.countryCodes = map[server.Id]string{testMaps.countryId: countryCode}
	copies := func() map[server.Id]*ClientScore {
		clientScores := map[server.Id]*ClientScore{}
		for clientId, provider := range providers {
			clientScores[clientId] = countryFloorTestScore(clientId, provider)
		}
		return clientScores
	}
	testMaps.locationClientScores[testMaps.countryId] = copies()
	testMaps.locationClientScores[testMaps.cityId] = copies()
	testMaps.locationGroupClientScores[testMaps.groupId] = copies()
	return testMaps
}

// Applies the floor of `providers` with barred, the providers the strict
// decision's bar alone keeps out of Online, and no classified decision.
func (self *countryFloorTestMaps) apply(providers int, barred map[server.Id]bool) *clientScoreCountryFloor {
	return applyClientScoreCountryFloor(providers, self.locationClientScores, self.locationGroupClientScores, self.countryCodes, barred, map[server.Id]*clientScoreCapability{})
}

// The client ids every copy holds in Online, or in a native mode, the same
// in every copy.
func (self *countryFloorTestMaps) members(t *testing.T, admits func(clientScore *ClientScore) bool) []server.Id {
	t.Helper()
	var members []server.Id
	for _, targetClientScores := range []map[server.Id]map[server.Id]*ClientScore{self.locationClientScores, self.locationGroupClientScores} {
		for _, clientScores := range targetClientScores {
			copyMembers := []server.Id{}
			for clientId, clientScore := range clientScores {
				if admits(clientScore) {
					copyMembers = append(copyMembers, clientId)
				}
			}
			slices.SortFunc(copyMembers, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			if members != nil && !slices.Equal(members, copyMembers) {
				t.Fatalf("the copies disagree: %v and %v", members, copyMembers)
			}
			members = copyMembers
		}
	}
	return members
}

func countryFloorTestOnline(clientScore *ClientScore) bool {
	return clientScore.Online
}

func countryFloorTestQuality(clientScore *ClientScore) bool {
	return clientScore.PassesMinimums[RankModeQuality]
}

func countryFloorTestSpeed(clientScore *ClientScore) bool {
	return clientScore.PassesMinimums[RankModeSpeed]
}

// The ids in client id order, as members lists them.
func countryFloorTestSorted(clientIds ...server.Id) []server.Id {
	sorted := slices.Clone(clientIds)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return sorted
}

// What the URL bar alone keeps out of Online: low_success and no_success
// providers, whatever the other rules say of their native buckets. A failing
// ratio the Online rule keeps (health), an unmeasured provider, one short of
// the native URL-run minimums and a hard exclusion are not.
func TestCountryFloorBarredFromOnlineIsTheBarAlone(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	settings.OnlineUrlSuccessRatio = 0.8
	providers := []*onlineUrlRatioTestProvider{
		{name: "us 60 of 60", countryCode: "us", ok: 60, total: 60},
		{name: "us 7 of 10", countryCode: "us", ok: 7, total: 10},
		{name: "us 0 of 5", countryCode: "us", ok: 0, total: 5},
		{name: "us arin non-quality 6 of 10", countryCode: "us", ok: 6, total: 10, arinNonQuality: true},
		{name: "us 1 of 2", countryCode: "us", ok: 1, total: 2},
		{name: "us 1 of 1", countryCode: "us", ok: 1, total: 1},
		{name: "us unprobed", countryCode: "us"},
		{name: "us arin risk 3 of 5", countryCode: "us", ok: 3, total: 5, arinRisk: true},
		{name: "us tls 3 of 5", countryCode: "us", ok: 3, total: 5, tlsFailed: true},
		{name: "us unreliable 3 of 5", countryCode: "us", ok: 3, total: 5, reliabilityFailed: true},
		{name: "us 8 of 10 a", countryCode: "us", ok: 8, total: 10},
		{name: "us 9 of 10 b", countryCode: "us", ok: 9, total: 10},
	}
	filter, nameClientIds := onlineUrlRatioTestFilter(providers, settings)
	barred := map[string]bool{
		"us 7 of 10":                  true,
		"us 0 of 5":                   true,
		"us arin non-quality 6 of 10": true,
		"us 1 of 2":                   true,
	}
	for _, provider := range providers {
		clientId := nameClientIds[provider.name]
		countryCode := "us"
		facts := filter.egressFacts(clientId, &countryCode, nil, nil, settings)
		decision := decideProviderEgress(facts, false)
		if got := providerEgressBarredFromOnline(facts, decision, false); got != barred[provider.name] {
			t.Errorf("%s (%s): barred from Online %t, want %t", provider.name, decision.reason, got, barred[provider.name])
		}
	}

	// with the ratio off a failing ratio is online, and only the rule of one
	// success bars
	settings.OnlineUrlSuccessRatio = 0
	filter, nameClientIds = onlineUrlRatioTestFilter(providers, settings)
	for _, provider := range providers {
		clientId := nameClientIds[provider.name]
		countryCode := "us"
		facts := filter.egressFacts(clientId, &countryCode, nil, nil, settings)
		decision := decideProviderEgress(facts, false)
		want := provider.name == "us 0 of 5"
		if got := providerEgressBarredFromOnline(facts, decision, false); got != want {
			t.Errorf("ratio off, %s (%s): barred from Online %t, want %t", provider.name, decision.reason, got, want)
		}
	}
}

// A country without a provider above the bar gets its best three below it
// online, in every copy (country, city, group), in the Online fallback's
// order; the two below them stay out. Quality and Speed keep the bar's
// members, none, and every admitted copy keeps its tiers and weights.
func TestCountryFloorAdmitsTheBestThreeOfAnEmptyCountryToOnline(t *testing.T) {
	p70, p60, p50, p40, p0 := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	providers := map[server.Id]countryFloorTestProvider{
		p70: countryFloorTestBelow(0.7),
		p60: countryFloorTestBelow(0.6),
		p50: countryFloorTestBelow(0.5),
		p40: countryFloorTestBelow(0.4),
		p0:  countryFloorTestBelow(0),
	}
	barred := map[server.Id]bool{}
	for clientId := range providers {
		barred[clientId] = true
	}
	testMaps := newCountryFloorTestMaps("pe", providers)
	countryFloor := testMaps.apply(3, barred)

	connect.AssertEqual(t, testMaps.members(t, countryFloorTestOnline), countryFloorTestSorted(p70, p60, p50))
	connect.AssertEqual(t, len(testMaps.members(t, countryFloorTestQuality)), 0)
	connect.AssertEqual(t, len(testMaps.members(t, countryFloorTestSpeed)), 0)
	connect.AssertEqual(t, countryFloor.countryCounts, map[clientScoreCountryFloorKey]int{{country: "pe", decision: clientScoreCountryFloorStrict}: 3})
	connect.AssertEqual(t, countryFloor.strictClientIds, map[server.Id]bool{p70: true, p60: true, p50: true})
	connect.AssertEqual(t, countryFloor.fleetCounts(), map[string]int{clientScoreCountryFloorStrict: 3, clientScoreCountryFloorClassified: 0})
	for _, clientScore := range testMaps.locationClientScores[testMaps.cityId] {
		admitted := slices.Contains([]server.Id{p70, p60, p50}, clientScore.ClientId)
		connect.AssertEqual(t, clientScore.countryFloor, admitted)
		connect.AssertEqual(t, clientScore.Tiers, map[string]int{RankModeQuality: 2, RankModeSpeed: 2})
		connect.AssertEqual(t, len(clientScore.ScaledWeights), 0)
	}
}

// A country with three online is left exactly as it was, and one with two
// gets only the best below the bar as the third: no provider above the bar
// loses a bucket, a tier or a weight, and its native buckets stay the bar's.
func TestCountryFloorFillsOnlyToTheFloorAndDisplacesNoProvider(t *testing.T) {
	native1, native2, native3 := server.NewId(), server.NewId(), server.NewId()
	below1, below2 := server.NewId(), server.NewId()
	barred := map[server.Id]bool{below1: true, below2: true}
	full := newCountryFloorTestMaps("nz", map[server.Id]countryFloorTestProvider{
		native1: countryFloorTestNative,
		native2: countryFloorTestNative,
		native3: countryFloorTestNative,
		below1:  countryFloorTestBelow(0.7),
		below2:  countryFloorTestBelow(0.6),
	})
	beforeTiers := map[server.Id]map[string]int{}
	beforeWeights := map[server.Id]map[string]float32{}
	for clientId, clientScore := range full.locationClientScores[full.countryId] {
		beforeTiers[clientId] = maps.Clone(clientScore.Tiers)
		beforeWeights[clientId] = maps.Clone(clientScore.ScaledWeights)
	}
	countryFloor := full.apply(3, barred)
	connect.AssertEqual(t, full.members(t, countryFloorTestOnline), countryFloorTestSorted(native1, native2, native3))
	connect.AssertEqual(t, len(countryFloor.countryCounts), 0)
	connect.AssertEqual(t, len(countryFloor.strictClientIds), 0)
	for clientId, clientScore := range full.locationClientScores[full.countryId] {
		connect.AssertEqual(t, clientScore.Tiers, beforeTiers[clientId])
		connect.AssertEqual(t, clientScore.ScaledWeights, beforeWeights[clientId])
		connect.AssertEqual(t, clientScore.countryFloor, false)
	}

	short := newCountryFloorTestMaps("uy", map[server.Id]countryFloorTestProvider{
		native1: countryFloorTestNative,
		native2: countryFloorTestNative,
		below1:  countryFloorTestBelow(0.7),
		below2:  countryFloorTestBelow(0.6),
	})
	short.apply(3, barred)
	connect.AssertEqual(t, short.members(t, countryFloorTestOnline), countryFloorTestSorted(native1, native2, below1))
	connect.AssertEqual(t, short.members(t, countryFloorTestQuality), countryFloorTestSorted(native1, native2))
	connect.AssertEqual(t, short.members(t, countryFloorTestSpeed), countryFloorTestSorted(native1, native2))
	clientScores := short.locationClientScores[short.countryId]
	for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
		connect.AssertEqual(t, clientScores[native1].Tiers[rankMode], 0)
		connect.AssertEqual(t, clientScores[native1].ScaledWeights[rankMode], float32(1))
	}
}

// The floor admits in the Online fallback's order: reliability times URL
// weight, then the speed tier, then the URL weight; ties to the lower client
// id.
func TestCountryFloorRanksByTheOnlineFallbacksOrder(t *testing.T) {
	score := func(speedTier int, urlWeight float64, reliabilityWeight float64) *ClientScore {
		return countryFloorTestScore(server.NewId(), countryFloorTestProvider{tier: speedTier, score: 20 * speedTier, urlWeight: urlWeight, reliabilityWeight: reliabilityWeight, passesReliability: true})
	}
	// reliability times URL weight first: 0.6 against 0.35
	reliable, unreliableBetterUrl := score(2, 0.6, 1), score(0, 0.7, 0.5)
	connect.AssertEqual(t, compareClientScoreCountryFloor(reliable, unreliableBetterUrl) < 0, true)
	// then the speed tier
	faster, slower := score(1, 0.6, 1), score(2, 0.6, 1)
	connect.AssertEqual(t, compareClientScoreCountryFloor(faster, slower) < 0, true)
	// full ties go to the lower client id
	a, b := score(2, 0.5, 1), score(2, 0.5, 1)
	if b.ClientId.Cmp(a.ClientId) < 0 {
		a, b = b, a
	}
	connect.AssertEqual(t, compareClientScoreCountryFloor(a, b) < 0, true)

	// and the floor admits by it
	worst, middle, top := server.NewId(), server.NewId(), server.NewId()
	testMaps := newCountryFloorTestMaps("ar", map[server.Id]countryFloorTestProvider{
		worst:  {tier: 0, score: 0, urlWeight: 0.3, reliabilityWeight: 1, passesReliability: true},
		middle: {tier: 2, score: 40, urlWeight: 0.6, reliabilityWeight: 1, passesReliability: true},
		top:    {tier: 2, score: 50, urlWeight: 0.7, reliabilityWeight: 1, passesReliability: true},
	})
	testMaps.apply(2, map[server.Id]bool{worst: true, middle: true, top: true})
	connect.AssertEqual(t, testMaps.members(t, countryFloorTestOnline), countryFloorTestSorted(top, middle))
}

// The floor admits only what the bar alone keeps out: never a provider below
// the reliability minimums or kept out by another rule, so a country with too
// few candidates stays below the floor. A network-only provider neither
// counts nor is admitted.
func TestCountryFloorAdmitsOnlyWhatTheBarAloneKeepsOut(t *testing.T) {
	eligible, unreliable, notBarred, networkOnly := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	networkOnlyNative := countryFloorTestNative
	networkOnlyNative.networkOnly = true
	networkOnlyBelow := countryFloorTestBelow(0.9)
	networkOnlyBelow.networkOnly = true
	unreliableProvider := countryFloorTestBelow(0.9)
	unreliableProvider.passesReliability = false
	networkOnlyBarred := server.NewId()
	testMaps := newCountryFloorTestMaps("cl", map[server.Id]countryFloorTestProvider{
		eligible:          countryFloorTestBelow(0.5),
		unreliable:        unreliableProvider,
		notBarred:         countryFloorTestBelow(0.9),
		networkOnly:       networkOnlyNative,
		networkOnlyBarred: networkOnlyBelow,
	})
	countryFloor := testMaps.apply(3, map[server.Id]bool{eligible: true, unreliable: true, networkOnlyBarred: true})
	connect.AssertEqual(t, testMaps.members(t, countryFloorTestOnline), countryFloorTestSorted(eligible, networkOnly))
	connect.AssertEqual(t, countryFloor.countryCounts, map[clientScoreCountryFloorKey]int{{country: "cl", decision: clientScoreCountryFloorStrict}: 1})
	connect.AssertEqual(t, countryFloor.strictClientIds, map[server.Id]bool{eligible: true})
}

// Today's facets keep the hard gates of UDP and IP family: a provider whose
// UDP the probe dropped is never admitted to its strict record, even as the
// best below the bar. Its classified copy is, in the capability facets
// selector mode reads, where the country's supply counts every class and the
// strict admissions with it.
func TestCountryFloorKeepsDowngradedProvidersOutOfTodaysFacets(t *testing.T) {
	plain, udpBest, udpNext, udpWorst := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	udpProvider := func(share float64) countryFloorTestProvider {
		provider := countryFloorTestBelow(share)
		provider.downgrades = ClientScoreCapabilityDowngradeUdp
		return provider
	}
	testMaps := newCountryFloorTestMaps("ar", map[server.Id]countryFloorTestProvider{
		plain:    countryFloorTestBelow(0.4),
		udpBest:  udpProvider(0.75),
		udpNext:  udpProvider(0.7),
		udpWorst: udpProvider(0.6),
	})
	capabilities := map[server.Id]*clientScoreCapability{}
	for _, clientId := range []server.Id{udpBest, udpNext, udpWorst} {
		capabilities[clientId] = &clientScoreCapability{barredFromOnline: true}
		for _, targetClientScores := range []map[server.Id]map[server.Id]*ClientScore{testMaps.locationClientScores, testMaps.locationGroupClientScores} {
			for _, clientScores := range targetClientScores {
				// as the export makes it: its own buckets and weights
				classified := *clientScores[clientId]
				classified.PassesMinimums = maps.Clone(classified.PassesMinimums)
				classified.ScaledWeights = maps.Clone(classified.ScaledWeights)
				clientScores[clientId].capabilityCopy = &classified
			}
		}
	}
	barred := map[server.Id]bool{plain: true, udpBest: true, udpNext: true, udpWorst: true}
	countryFloor := applyClientScoreCountryFloor(3, testMaps.locationClientScores, testMaps.locationGroupClientScores, testMaps.countryCodes, barred, capabilities)

	// today's facets: the plain provider alone
	connect.AssertEqual(t, testMaps.members(t, countryFloorTestOnline), []server.Id{plain})
	connect.AssertEqual(t, countryFloor.strictClientIds, map[server.Id]bool{plain: true})
	// the capability facets: the best two downgraded copies make the
	// country's three with the plain provider
	for _, targetClientScores := range []map[server.Id]map[server.Id]*ClientScore{testMaps.locationClientScores, testMaps.locationGroupClientScores} {
		for _, clientScores := range targetClientScores {
			for clientId, clientScore := range clientScores {
				if classified := clientScore.capabilityCopy; classified != nil {
					connect.AssertEqual(t, classified.Online, clientId == udpBest || clientId == udpNext)
					connect.AssertEqual(t, classified.PassesMinimums[RankModeQuality], false)
				}
			}
		}
	}
	connect.AssertEqual(t, countryFloor.countryCounts, map[clientScoreCountryFloorKey]int{
		{country: "ar", decision: clientScoreCountryFloorStrict}:     1,
		{country: "ar", decision: clientScoreCountryFloorClassified}: 2,
	})
}

// A floor of zero is off: nothing is admitted and nothing is counted.
func TestCountryFloorOfZeroAdmitsNothing(t *testing.T) {
	below := server.NewId()
	testMaps := newCountryFloorTestMaps("pe", map[server.Id]countryFloorTestProvider{below: countryFloorTestBelow(0.7)})
	countryFloor := testMaps.apply(0, map[server.Id]bool{below: true})
	connect.AssertEqual(t, len(testMaps.members(t, countryFloorTestOnline)), 0)
	connect.AssertEqual(t, len(countryFloor.countryCounts), 0)
	connect.AssertEqual(t, countryFloor.logSummary(), "off (egress_index.country_floor_providers is not positive)")
}

// country_floor_providers in the egress_index block: absent is the default
// of three, a count replaces it, zero turns the floor off, and a negative
// count is out of range and keeps the default.
func TestEgressIndexSettingsReadTheCountryFloor(t *testing.T) {
	connect.AssertEqual(t, DefaultEgressIndexSettings().CountryFloorProviders, 3)
	for _, c := range []struct {
		config string
		want   int
	}{
		{config: "online_url_success_ratio: 0.8\n", want: 3},
		{config: "egress_index: {country_floor_providers: 5}\n", want: 5},
		{config: "egress_index: {country_floor_providers: 0}\n", want: 0},
		{config: "egress_index: {country_floor_providers: -1}\n", want: 3},
	} {
		settings := func() *EgressIndexSettings {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(c.config))
			defer pop()
			return egressIndexSettings()
		}()
		if settings.CountryFloorProviders != c.want {
			t.Errorf("%q: country floor %d, want %d", c.config, settings.CountryFloorProviders, c.want)
		}
	}
}

// Capture the flag's presence counts the bar's Online alone: a provider the
// floor admitted to Online is not present.
func TestCtfPresenceProvidersLeaveOutTheCountryFloor(t *testing.T) {
	barOnline, floorOnline := server.NewId(), server.NewId()
	clientScores := map[server.Id]*ClientScore{
		barOnline:   {ClientId: barOnline, Online: true},
		floorOnline: {ClientId: floorOnline, Online: true, countryFloor: true},
	}
	providers := newCtfPresenceProviders(onlineSuccessTestNow, map[server.Id]map[server.Id]*ClientScore{server.NewId(): clientScores}, map[server.Id]*clientScoreEgress{})
	present := []server.Id{}
	for _, provider := range providers.Providers {
		present = append(present, provider.ClientId)
	}
	connect.AssertEqual(t, present, []server.Id{barOnline})
}

// The supply publishes the floor's admissions with the measurement they came
// from: a series for each country and decision the floor admitted to, a code
// the directory cannot vouch for left out, and the fleet totals of both
// decisions, zero included.
func TestClientScoreSupplyPublishesTheCountryFloor(t *testing.T) {
	clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
	collector := supplyTestProcess(clock, "taskworker-a", &supplyTestLease{now: clock.now})
	supply := supplyTestMeasurement(clock.now(), 3, "pe")
	countryFloor := &clientScoreCountryFloor{providers: 3, countryCounts: map[clientScoreCountryFloorKey]int{
		{country: "pe", decision: clientScoreCountryFloorStrict}:            3,
		{country: "nz", decision: clientScoreCountryFloorStrict}:            1,
		{country: "not-a-country", decision: clientScoreCountryFloorStrict}: 1,
	}}
	supply.countryFloor = countryFloor.countryCounts
	supply.countryFloorFleet = countryFloor.fleetCounts()
	collector.publish(context.Background(), supply)

	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	gathered, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	countrySeries := map[string]float64{}
	fleetSeries := map[string]float64{}
	for _, family := range gathered {
		for _, metric := range family.Metric {
			labels := map[string]string{}
			for _, pair := range metric.Label {
				labels[pair.GetName()] = pair.GetValue()
			}
			switch family.GetName() {
			case "urnetwork_client_score_country_floor_providers":
				countrySeries[labels["country"]+" "+labels["decision"]] = metric.GetGauge().GetValue()
			case "urnetwork_client_score_country_floor_providers_fleet":
				fleetSeries[labels["decision"]] = metric.GetGauge().GetValue()
			}
		}
	}
	connect.AssertEqual(t, countrySeries, map[string]float64{"pe strict": 3, "nz strict": 1})
	connect.AssertEqual(t, fleetSeries, map[string]float64{"strict": 5, "classified": 0})
}

// The provider.yml line that turns the country floor off, for a test of the
// URL bar alone that pushes its own provider.yml: its countries are too small
// to hold the floor's three without the floor admitting the very providers
// the test keeps below the bar (network_client_score_country_floor_db_test.go
// tests the floor). Only for a config without an egress_index block of its
// own.
const egressTestCountryFloorOffConfig = "egress_index: {country_floor_providers: 0}\n"

// Turns the country floor off in provider.yml until the test ends, keeping
// every other setting, for a test of the URL bar alone that pushes nothing
// after it.
func egressTestCountryFloorOff(t testing.TB) {
	t.Helper()
	config := map[string]any{}
	if resource, err := server.Config.SimpleResource(providerConfigResourceName); err == nil && resource != nil {
		parsed, err := resource.ParseE()
		if err != nil {
			t.Fatal(err)
		}
		maps.Copy(config, parsed)
	}
	egressIndex := map[string]any{}
	if block, ok := config["egress_index"].(map[string]any); ok {
		maps.Copy(egressIndex, block)
	}
	egressIndex["country_floor_providers"] = 0
	config["egress_index"] = egressIndex
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, encoded))
	requestEgressIndexSettingsSnapshot.Store(nil)
}
