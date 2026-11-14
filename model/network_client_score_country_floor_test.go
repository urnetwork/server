// The country floor (network_client_score_country_floor.go), pure: which
// buckets the URL bar alone keeps a provider out of, the order each bucket
// admits by, what the floor admits and to which copies, what it never
// admits, and its kill switch. Every value below is synthetic.
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
		rankWeights:           map[string]float32{RankModeQuality: float32(provider.urlWeight), RankModeSpeed: float32(provider.urlWeight)},
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

// Every bucket the bar alone keeps the low_success providers out of.
const countryFloorTestAllBuckets = clientScoreCountryFloorQuality | clientScoreCountryFloorSpeed | clientScoreCountryFloorOnline

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

// The tier offset of the export's admitted natives: the Online fallback's
// band under the default settings.
func countryFloorTestTierOffset() int {
	return 2 * DefaultEgressIndexSettings().BackfillTierOffset
}

// Applies the floor of `providers` with barred, the strict decision's barred
// buckets, and no classified decision.
func (self *countryFloorTestMaps) apply(providers int, barred map[server.Id]uint8) *clientScoreCountryFloor {
	return applyClientScoreCountryFloor(providers, countryFloorTestTierOffset(), self.locationClientScores, self.locationGroupClientScores, self.countryCodes, barred, map[server.Id]*clientScoreCapability{})
}

// The client ids each copy admits to the bucket, the same in every copy.
func (self *countryFloorTestMaps) members(t *testing.T, bucket clientScoreCountryFloorBucket) []server.Id {
	t.Helper()
	var members []server.Id
	for _, targetClientScores := range []map[server.Id]map[server.Id]*ClientScore{self.locationClientScores, self.locationGroupClientScores} {
		for _, clientScores := range targetClientScores {
			copyMembers := []server.Id{}
			for clientId, clientScore := range clientScores {
				if bucket.admits(clientScore) {
					copyMembers = append(copyMembers, clientId)
				}
			}
			slices.SortFunc(copyMembers, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			if members != nil && !slices.Equal(members, copyMembers) {
				t.Fatalf("%s: the copies disagree: %v and %v", bucket.name, members, copyMembers)
			}
			members = copyMembers
		}
	}
	return members
}

// The ids in client id order, as members lists them.
func countryFloorTestSorted(clientIds ...server.Id) []server.Id {
	sorted := slices.Clone(clientIds)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return sorted
}

// What the URL bar alone keeps a provider out of: low_success and no_success
// every bucket, a failing ratio the native buckets, and nothing the other
// rules decide. ARIN non-quality stays out of Quality, a provider short of
// Quality's runs is barred from Speed and Online only, an unmeasured one and
// a hard exclusion are barred from nothing.
func TestCountryFloorBarredBucketsAreTheBarAlone(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	settings.OnlineUrlSuccessRatio = 0.8
	providers := []*onlineUrlRatioTestProvider{
		{name: "us 60 of 60", countryCode: "us", ok: 60, total: 60},
		{name: "us 7 of 10", countryCode: "us", ok: 7, total: 10},
		{name: "us 0 of 5", countryCode: "us", ok: 0, total: 5},
		{name: "us arin non-quality 6 of 10", countryCode: "us", ok: 6, total: 10, arinNonQuality: true},
		{name: "us 1 of 2", countryCode: "us", ok: 1, total: 2},
		{name: "us unprobed", countryCode: "us"},
		{name: "us arin risk 3 of 5", countryCode: "us", ok: 3, total: 5, arinRisk: true},
		{name: "us tls 3 of 5", countryCode: "us", ok: 3, total: 5, tlsFailed: true},
		{name: "us 8 of 10 a", countryCode: "us", ok: 8, total: 10},
		{name: "us 9 of 10 b", countryCode: "us", ok: 9, total: 10},
	}
	filter, nameClientIds := onlineUrlRatioTestFilter(providers, settings)
	want := map[string]uint8{
		"us 60 of 60":                 0,
		"us 7 of 10":                  countryFloorTestAllBuckets,
		"us 0 of 5":                   countryFloorTestAllBuckets,
		"us arin non-quality 6 of 10": clientScoreCountryFloorSpeed | clientScoreCountryFloorOnline,
		// a passing ratio needs three runs for Quality, two for Speed
		"us 1 of 2":           clientScoreCountryFloorSpeed | clientScoreCountryFloorOnline,
		"us unprobed":         0,
		"us arin risk 3 of 5": 0,
		"us tls 3 of 5":       0,
		"us 8 of 10 a":        0,
		"us 9 of 10 b":        0,
	}
	for name, wantBuckets := range want {
		clientId := nameClientIds[name]
		countryCode := "us"
		facts := filter.egressFacts(clientId, &countryCode, nil, nil, settings)
		decision := decideProviderEgress(facts, false)
		if got := providerEgressBarredBuckets(facts, decision, false); got != wantBuckets {
			t.Errorf("%s (%s): barred %03b, want %03b", name, decision.reason, got, wantBuckets)
		}
	}
}

// A country without a provider above the bar gets its best N below it, in
// every bucket and every copy (country, city, group), each joining Online
// with its native admission; the rest stay out. The admitted natives draw
// with their mode's weight and carry their tier moved into the Online
// fallback's band.
func TestCountryFloorAdmitsTheBestNOfAnEmptyCountry(t *testing.T) {
	p70, p60, p50, p40, p0 := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	providers := map[server.Id]countryFloorTestProvider{
		p70: countryFloorTestBelow(0.7),
		p60: countryFloorTestBelow(0.6),
		p50: countryFloorTestBelow(0.5),
		p40: countryFloorTestBelow(0.4),
		p0:  countryFloorTestBelow(0),
	}
	barred := map[server.Id]uint8{}
	for clientId := range providers {
		barred[clientId] = countryFloorTestAllBuckets
	}
	testMaps := newCountryFloorTestMaps("pe", providers)
	countryFloor := testMaps.apply(3, barred)

	best := countryFloorTestSorted(p70, p60, p50)
	for _, bucket := range clientScoreCountryFloorBuckets {
		connect.AssertEqual(t, testMaps.members(t, bucket), best)
		connect.AssertEqual(t, countryFloor.countryCounts[clientScoreCountryFloorKey{country: "pe", decision: clientScoreCountryFloorStrict, bucket: bucket.name}], 3)
		connect.AssertEqual(t, countryFloor.countryCounts[clientScoreCountryFloorKey{country: "pe", decision: clientScoreCountryFloorClassified, bucket: bucket.name}], 0)
	}
	connect.AssertEqual(t, countryFloor.strictClientIds, map[server.Id]bool{p70: true, p60: true, p50: true})
	for _, clientScore := range testMaps.locationClientScores[testMaps.cityId] {
		if !slices.Contains(best, clientScore.ClientId) {
			connect.AssertEqual(t, clientScore.countryFloor, uint8(0))
			continue
		}
		connect.AssertEqual(t, clientScore.countryFloor, countryFloorTestAllBuckets)
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			connect.AssertEqual(t, clientScore.Tiers[rankMode], countryFloorTestTierOffset()+2)
			connect.AssertEqual(t, clientScore.ScaledWeights[rankMode], clientScore.rankWeights[rankMode])
		}
	}
	fleetCounts := countryFloor.fleetCounts()
	connect.AssertEqual(t, len(fleetCounts), 6)
	connect.AssertEqual(t, fleetCounts[clientScoreCountryFloorFleetKey{decision: clientScoreCountryFloorStrict, bucket: RankModeOnline}], 3)
}

// A bucket that holds the floor's count above the bar is left exactly as
// it was, and one that holds fewer is filled only to the count: the
// providers above the bar keep their tiers and weights, and the one admitted
// below it ranks behind them.
func TestCountryFloorFillsOnlyToTheFloorAndDisplacesNoProvider(t *testing.T) {
	native1, native2, native3 := server.NewId(), server.NewId(), server.NewId()
	below1, below2 := server.NewId(), server.NewId()
	full := newCountryFloorTestMaps("nz", map[server.Id]countryFloorTestProvider{
		native1: countryFloorTestNative,
		native2: countryFloorTestNative,
		native3: countryFloorTestNative,
		below1:  countryFloorTestBelow(0.7),
		below2:  countryFloorTestBelow(0.6),
	})
	barred := map[server.Id]uint8{below1: countryFloorTestAllBuckets, below2: countryFloorTestAllBuckets}
	beforeTiers := map[server.Id]map[string]int{}
	beforeWeights := map[server.Id]map[string]float32{}
	for clientId, clientScore := range full.locationClientScores[full.countryId] {
		beforeTiers[clientId] = maps.Clone(clientScore.Tiers)
		beforeWeights[clientId] = maps.Clone(clientScore.ScaledWeights)
	}
	countryFloor := full.apply(3, barred)
	for _, bucket := range clientScoreCountryFloorBuckets {
		connect.AssertEqual(t, full.members(t, bucket), countryFloorTestSorted(native1, native2, native3))
	}
	connect.AssertEqual(t, len(countryFloor.countryCounts), 0)
	connect.AssertEqual(t, len(countryFloor.strictClientIds), 0)
	for clientId, clientScore := range full.locationClientScores[full.countryId] {
		connect.AssertEqual(t, clientScore.Tiers, beforeTiers[clientId])
		connect.AssertEqual(t, clientScore.ScaledWeights, beforeWeights[clientId])
		connect.AssertEqual(t, clientScore.countryFloor, uint8(0))
	}

	// two above the bar: the best below it joins them, behind them
	short := newCountryFloorTestMaps("uy", map[server.Id]countryFloorTestProvider{
		native1: countryFloorTestNative,
		native2: countryFloorTestNative,
		below1:  countryFloorTestBelow(0.7),
		below2:  countryFloorTestBelow(0.6),
	})
	short.apply(3, barred)
	for _, bucket := range clientScoreCountryFloorBuckets {
		connect.AssertEqual(t, short.members(t, bucket), countryFloorTestSorted(native1, native2, below1))
	}
	clientScores := short.locationClientScores[short.countryId]
	// the most a borrowed native ranks at: the other mode's past-cutoff tier
	// with the offset, and every demerit the client adds
	borrowedWorstTier := DefaultEgressIndexSettings().BackfillTierOffset + ClientScoreCutoffTier + MaxClientTierDemerit
	for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
		connect.AssertEqual(t, clientScores[native1].Tiers[rankMode], 0)
		connect.AssertEqual(t, clientScores[native1].ScaledWeights[rankMode], float32(1))
		if clientScores[below1].Tiers[rankMode] <= borrowedWorstTier {
			t.Fatalf("%s: the admitted provider's tier %d does not rank behind every borrowed native (%d)", rankMode, clientScores[below1].Tiers[rankMode], borrowedWorstTier)
		}
	}
	connect.AssertEqual(t, clientScores[below2].Tiers[RankModeQuality], 2)
}

// Each bucket admits in its own order: the native modes by tier, then score,
// then URL success weight; Online by reliability times URL weight; ties by
// client id.
func TestCountryFloorRanksByTheBucketsOwnOrder(t *testing.T) {
	quality := clientScoreCountryFloorBuckets[0]
	online := clientScoreCountryFloorBuckets[2]
	score := func(tier int, scoreValue int, urlWeight float64, reliabilityWeight float64) *ClientScore {
		return countryFloorTestScore(server.NewId(), countryFloorTestProvider{tier: tier, score: scoreValue, urlWeight: urlWeight, reliabilityWeight: reliabilityWeight, passesReliability: true})
	}
	// a lower tier wins over a better URL weight
	lowTier, highTierBetterUrl := score(1, 30, 0.2, 1), score(2, 40, 0.9, 1)
	connect.AssertEqual(t, quality.compare(lowTier, highTierBetterUrl) < 0, true)
	// a lower score within the tier wins over a better URL weight
	lowScore, highScoreBetterUrl := score(2, 40, 0.2, 1), score(2, 50, 0.9, 1)
	connect.AssertEqual(t, quality.compare(lowScore, highScoreBetterUrl) < 0, true)
	// at one tier and score, the better URL weight
	betterUrl, worseUrl := score(2, 50, 0.8, 1), score(2, 50, 0.5, 1)
	connect.AssertEqual(t, quality.compare(betterUrl, worseUrl) < 0, true)
	// Online reads reliability times URL weight, not the native score
	reliable, unreliableBetterScore := score(2, 50, 0.6, 1), score(0, 0, 0.7, 0.5)
	connect.AssertEqual(t, online.compare(reliable, unreliableBetterScore) < 0, true)
	// full ties go to the lower client id
	a, b := score(2, 50, 0.5, 1), score(2, 50, 0.5, 1)
	if b.ClientId.Cmp(a.ClientId) < 0 {
		a, b = b, a
	}
	connect.AssertEqual(t, quality.compare(a, b) < 0, true)
	connect.AssertEqual(t, online.compare(a, b) < 0, true)

	// and the floor admits by it
	worst, middle, top := server.NewId(), server.NewId(), server.NewId()
	testMaps := newCountryFloorTestMaps("ar", map[server.Id]countryFloorTestProvider{
		worst:  {tier: 2, score: 50, urlWeight: 0.9, reliabilityWeight: 1, passesReliability: true},
		middle: {tier: 2, score: 40, urlWeight: 0.3, reliabilityWeight: 1, passesReliability: true},
		top:    {tier: 1, score: 30, urlWeight: 0.2, reliabilityWeight: 1, passesReliability: true},
	})
	testMaps.apply(2, map[server.Id]uint8{worst: clientScoreCountryFloorQuality, middle: clientScoreCountryFloorQuality, top: clientScoreCountryFloorQuality})
	connect.AssertEqual(t, testMaps.members(t, quality), countryFloorTestSorted(top, middle))
}

// The floor admits no provider a hard exclusion, the reliability minimums or
// another rule keeps out: one the bar does not bar from a bucket stays out
// of it, as does one below the reliability minimums, so a country with too
// few candidates stays below the floor. A network-only provider neither
// counts nor is admitted.
func TestCountryFloorAdmitsOnlyWhatTheBarAloneKeepsOut(t *testing.T) {
	eligible, unreliable, notBarred, arinNonQuality, networkOnly := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	networkOnlyNative := countryFloorTestNative
	networkOnlyNative.networkOnly = true
	unreliableProvider := countryFloorTestBelow(0.9)
	unreliableProvider.passesReliability = false
	testMaps := newCountryFloorTestMaps("cl", map[server.Id]countryFloorTestProvider{
		eligible:       countryFloorTestBelow(0.5),
		unreliable:     unreliableProvider,
		notBarred:      countryFloorTestBelow(0.9),
		arinNonQuality: countryFloorTestBelow(0.8),
		networkOnly:    networkOnlyNative,
	})
	countryFloor := testMaps.apply(3, map[server.Id]uint8{
		eligible:       countryFloorTestAllBuckets,
		unreliable:     countryFloorTestAllBuckets,
		arinNonQuality: clientScoreCountryFloorSpeed | clientScoreCountryFloorOnline,
	})
	connect.AssertEqual(t, testMaps.members(t, clientScoreCountryFloorBuckets[0]), countryFloorTestSorted(eligible, networkOnly))
	connect.AssertEqual(t, testMaps.members(t, clientScoreCountryFloorBuckets[1]), countryFloorTestSorted(eligible, arinNonQuality, networkOnly))
	connect.AssertEqual(t, testMaps.members(t, clientScoreCountryFloorBuckets[2]), countryFloorTestSorted(eligible, arinNonQuality, networkOnly))
	connect.AssertEqual(t, countryFloor.countryCounts[clientScoreCountryFloorKey{country: "cl", decision: clientScoreCountryFloorStrict, bucket: RankModeQuality}], 1)
	connect.AssertEqual(t, countryFloor.countryCounts[clientScoreCountryFloorKey{country: "cl", decision: clientScoreCountryFloorStrict, bucket: RankModeSpeed}], 2)
	connect.AssertEqual(t, countryFloor.strictClientIds, map[server.Id]bool{eligible: true, arinNonQuality: true})
}

// Today's facets keep the hard gates of UDP and IP family: a provider whose
// UDP or IPv6 the probe dropped is never admitted to its strict record, even
// as the best below the bar. Its classified copy is, in the capability
// facets selector mode reads, where the country's supply counts every class
// and the strict admissions with it.
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
		capabilities[clientId] = &clientScoreCapability{barredBuckets: countryFloorTestAllBuckets}
		for _, targetClientScores := range []map[server.Id]map[server.Id]*ClientScore{testMaps.locationClientScores, testMaps.locationGroupClientScores} {
			for _, clientScores := range targetClientScores {
				// as the export makes it: its own buckets and weights, its
				// tiers shared with the strict record
				classified := *clientScores[clientId]
				classified.PassesMinimums = maps.Clone(classified.PassesMinimums)
				classified.ScaledWeights = maps.Clone(classified.ScaledWeights)
				clientScores[clientId].capabilityCopy = &classified
			}
		}
	}
	barred := map[server.Id]uint8{}
	for _, clientId := range []server.Id{plain, udpBest, udpNext, udpWorst} {
		barred[clientId] = countryFloorTestAllBuckets
	}
	countryFloor := applyClientScoreCountryFloor(3, countryFloorTestTierOffset(), testMaps.locationClientScores, testMaps.locationGroupClientScores, testMaps.countryCodes, barred, capabilities)

	// today's facets: the plain provider alone
	for _, bucket := range clientScoreCountryFloorBuckets {
		connect.AssertEqual(t, testMaps.members(t, bucket), []server.Id{plain})
	}
	connect.AssertEqual(t, countryFloor.strictClientIds, map[server.Id]bool{plain: true})
	// the capability facets: the best two downgraded copies fill the
	// country's three with the plain provider
	for _, clientScores := range testMaps.locationClientScores {
		for clientId, clientScore := range clientScores {
			classified := clientScore.capabilityCopy
			if classified == nil {
				continue
			}
			admitted := clientId == udpBest || clientId == udpNext
			connect.AssertEqual(t, classified.PassesMinimums[RankModeQuality], admitted)
			connect.AssertEqual(t, classified.Online, admitted)
			connect.AssertEqual(t, clientScore.PassesMinimums[RankModeQuality], false)
			connect.AssertEqual(t, clientScore.Online, false)
			// the copy's admission moves its own tier, never the strict
			// record's it shared
			connect.AssertEqual(t, clientScore.Tiers[RankModeQuality], 2)
			if admitted {
				connect.AssertEqual(t, classified.Tiers[RankModeQuality], countryFloorTestTierOffset()+2)
			}
		}
	}
	for _, bucket := range clientScoreCountryFloorBuckets {
		connect.AssertEqual(t, countryFloor.countryCounts[clientScoreCountryFloorKey{country: "ar", decision: clientScoreCountryFloorClassified, bucket: bucket.name}], 2)
	}
}

// A floor of zero is off: nothing is admitted and nothing is counted.
func TestCountryFloorOfZeroAdmitsNothing(t *testing.T) {
	below := server.NewId()
	testMaps := newCountryFloorTestMaps("pe", map[server.Id]countryFloorTestProvider{below: countryFloorTestBelow(0.7)})
	countryFloor := testMaps.apply(0, map[server.Id]uint8{below: countryFloorTestAllBuckets})
	for _, bucket := range clientScoreCountryFloorBuckets {
		connect.AssertEqual(t, len(testMaps.members(t, bucket)), 0)
	}
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

// Capture the flag's presence counts the bar's Online alone: a provider in
// Online only by the floor is not present, one the floor admitted to a
// native mode while the bar kept it Online still is.
func TestCtfPresenceProvidersLeaveOutTheCountryFloor(t *testing.T) {
	barOnline, floorOnly, floorNative := server.NewId(), server.NewId(), server.NewId()
	clientScores := map[server.Id]*ClientScore{
		barOnline:   {ClientId: barOnline, Online: true},
		floorOnly:   {ClientId: floorOnly, Online: true, countryFloor: clientScoreCountryFloorOnline | clientScoreCountryFloorQuality},
		floorNative: {ClientId: floorNative, Online: true, countryFloor: clientScoreCountryFloorQuality},
	}
	providers := newCtfPresenceProviders(onlineSuccessTestNow, map[server.Id]map[server.Id]*ClientScore{server.NewId(): clientScores}, map[server.Id]*clientScoreEgress{})
	present := []server.Id{}
	for _, provider := range providers.Providers {
		present = append(present, provider.ClientId)
	}
	connect.AssertEqual(t, present, countryFloorTestSorted(barOnline, floorNative))
}

// The supply publishes the floor's admissions with the measurement they came
// from: a series for each country, bucket and decision the floor admitted
// to, a code the directory cannot vouch for left out, and the fleet totals
// of every bucket and decision, zero included.
func TestClientScoreSupplyPublishesTheCountryFloor(t *testing.T) {
	clock := newSupplyTestClock(time.Unix(1_800_000_000, 0))
	collector := supplyTestProcess(clock, "taskworker-a", &supplyTestLease{now: clock.now})
	supply := supplyTestMeasurement(clock.now(), 3, "pe")
	countryFloor := &clientScoreCountryFloor{providers: 3, countryCounts: map[clientScoreCountryFloorKey]int{
		{country: "pe", decision: clientScoreCountryFloorStrict, bucket: RankModeQuality}:          3,
		{country: "pe", decision: clientScoreCountryFloorStrict, bucket: RankModeOnline}:           3,
		{country: "pe", decision: clientScoreCountryFloorClassified, bucket: RankModeSpeed}:        2,
		{country: "not-a-country", decision: clientScoreCountryFloorStrict, bucket: RankModeSpeed}: 1,
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
				countrySeries[labels["country"]+" "+labels["bucket"]+" "+labels["decision"]] = metric.GetGauge().GetValue()
			case "urnetwork_client_score_country_floor_providers_fleet":
				fleetSeries[labels["bucket"]+" "+labels["decision"]] = metric.GetGauge().GetValue()
			}
		}
	}
	connect.AssertEqual(t, countrySeries, map[string]float64{
		"pe quality strict":   3,
		"pe online strict":    3,
		"pe speed classified": 2,
	})
	connect.AssertEqual(t, fleetSeries, map[string]float64{
		"quality strict":     3,
		"speed strict":       1,
		"online strict":      3,
		"quality classified": 0,
		"speed classified":   2,
		"online classified":  0,
	})
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
