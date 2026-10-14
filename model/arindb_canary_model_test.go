// The canary's comparison, breaker and state machine (arindbctl/ARINDB.md,
// "Comparison and breaker" and "Test plan"), over synthetic providers and an
// injected clock.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

const (
	testCanaryOlderEpoch  = int64(1699913600)
	testCanaryOldEpoch    = int64(1700000000)
	testCanaryLatestEpoch = int64(1700086400)
	testCanaryNewerEpoch  = int64(1700172800)
)

var testCanaryClock = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func testCanaryPairOf(latestEpoch int64, oldEpoch int64) arindbCanaryPair {
	return arindbCanaryPair{
		latest: server.ArinDatabaseIdentity{State: server.ArinDatabaseLoaded, BuildEpoch: latestEpoch, Sha256: fmt.Sprintf("%064x", latestEpoch)},
		old:    server.ArinDatabaseIdentity{State: server.ArinDatabaseLoaded, BuildEpoch: oldEpoch, Sha256: fmt.Sprintf("%064x", oldEpoch)},
	}
}

func testCanaryPair() arindbCanaryPair {
	return testCanaryPairOf(testCanaryLatestEpoch, testCanaryOldEpoch)
}

// One database's facts for a provider.
type testCanaryFacts struct {
	risk       bool
	nonQuality bool
}

var testCanaryClean = testCanaryFacts{}
var testCanaryNonQuality = testCanaryFacts{nonQuality: true}
var testCanaryRisky = testCanaryFacts{risk: true}

// A provider whose rows a restarted process wrote: both slots, latest first.
// Failing health keeps it out of Speed, as the export's rules do.
func testCanaryProvider(countryCode string, cityId *server.Id, passing bool, latest testCanaryFacts, old testCanaryFacts) *arindbCanaryProvider {
	return &arindbCanaryProvider{
		clientId:    server.NewId(),
		countryCode: countryCode,
		cityId:      cityId,
		slots: arindbRollupSlots{
			risk: latest.risk, nonQuality: latest.nonQuality, epoch: testCanaryLatestEpoch,
			oldRisk: old.risk, oldNonQuality: old.nonQuality, oldEpoch: testCanaryOldEpoch,
		},
		egress:  &clientScoreEgress{egressQuality: &passing},
		weights: map[int]float64{0: 1},
	}
}

// The shape of the export's rules: risk removes a provider from every bucket,
// non-quality from Quality only, and failing health from Speed and Quality.
func testCanaryMembership(provider *arindbCanaryProvider, risk bool, nonQuality bool) arindbCanaryMembership {
	if risk {
		return arindbCanaryMembership{}
	}
	passing := provider.egress.egressQuality != nil && *provider.egress.egressQuality
	return arindbCanaryMembership{quality: passing && !nonQuality, speed: passing, online: true}
}

func testCanaryProviders(n int, countryCode string, passing bool, latest testCanaryFacts, old testCanaryFacts) []*arindbCanaryProvider {
	providers := make([]*arindbCanaryProvider, 0, n)
	for range n {
		providers = append(providers, testCanaryProvider(countryCode, nil, passing, latest, old))
	}
	return providers
}

func decideTestCanary(pair arindbCanaryPair, providers []*arindbCanaryProvider, settings ArindbCanarySettings) *ArindbCanaryDecision {
	return decideArindbCanary(pair, providers, testCanaryMembership, settings, testCanaryClock.Add(-time.Minute), testCanaryClock)
}

// The net loss trips each bucket strictly above the threshold: 10 of 100 lost
// holds and 11 trips, in Quality, Speed and Online alike, and only the bucket
// that lost supply trips.
func TestArindbCanaryBreakerTripsJustOverTheLossThresholdInEachBucket(t *testing.T) {
	scenario := map[string]func(lost int) []*arindbCanaryProvider{
		// non-quality under latest removes Quality only
		RankModeQuality: func(lost int) []*arindbCanaryProvider {
			return append(
				testCanaryProviders(lost, "zz", true, testCanaryNonQuality, testCanaryClean),
				testCanaryProviders(100-lost, "zz", true, testCanaryClean, testCanaryClean)...)
		},
		// risk under latest removes Speed; Quality is empty under both, and
		// the Online bucket is twice as large, so it stays under the threshold
		RankModeSpeed: func(lost int) []*arindbCanaryProvider {
			providers := append(
				testCanaryProviders(lost, "zz", true, testCanaryRisky, testCanaryNonQuality),
				testCanaryProviders(100-lost, "zz", true, testCanaryNonQuality, testCanaryNonQuality)...)
			return append(providers, testCanaryProviders(100, "zz", false, testCanaryClean, testCanaryClean)...)
		},
		// risk under latest removes providers that fail health, which are in
		// Online only, so Speed and Quality are empty under both
		ProviderEgressBucketOnline: func(lost int) []*arindbCanaryProvider {
			return append(
				testCanaryProviders(lost, "zz", false, testCanaryRisky, testCanaryClean),
				testCanaryProviders(100-lost, "zz", false, testCanaryClean, testCanaryClean)...)
		},
	}
	for _, bucket := range ProviderEgressBuckets {
		for _, c := range []struct {
			lost    int
			tripped bool
		}{{lost: 10, tripped: false}, {lost: 11, tripped: true}} {
			decision := decideTestCanary(testCanaryPair(), scenario[bucket](c.lost), defaultArindbCanarySettings())
			b := decision.Buckets[bucket]
			if b.Tripped != c.tripped || b.Old != 100 || b.Latest != 100-c.lost || b.Removed != c.lost {
				t.Errorf("%s lost %d: tripped=%t old=%d latest=%d removed=%d loss=%v", bucket, c.lost, b.Tripped, b.Old, b.Latest, b.Removed, b.LossRatio)
			}
			for _, other := range ProviderEgressBuckets {
				if other != bucket && decision.Buckets[other].Tripped {
					t.Errorf("%s lost %d also tripped %s: %+v", bucket, c.lost, other, decision.Buckets[other])
				}
			}
			wantState, wantServing := ArindbCanaryStateLatest, testCanaryLatestEpoch
			if c.tripped {
				wantState, wantServing = ArindbCanaryStateFallback, testCanaryOldEpoch
			}
			if decision.State != wantState || decision.ServingEpoch != wantServing {
				t.Errorf("%s lost %d: state=%s serving=%d, want %s %d", bucket, c.lost, decision.State, decision.ServingEpoch, wantState, wantServing)
			}
		}
	}
}

// A country with supply under the old database and none under the latest trips
// the breaker in each bucket, however small its share; the switch can turn the
// rule off without hiding the report.
func TestArindbCanaryBreakerTripsOnACountryEmptiedInEachBucket(t *testing.T) {
	scenario := map[string][]*arindbCanaryProvider{
		RankModeQuality: {testCanaryProvider("bb", nil, true, testCanaryNonQuality, testCanaryClean)},
		RankModeSpeed: {
			testCanaryProvider("bb", nil, true, testCanaryRisky, testCanaryNonQuality),
			testCanaryProvider("bb", nil, false, testCanaryClean, testCanaryClean),
		},
		ProviderEgressBucketOnline: {testCanaryProvider("bb", nil, false, testCanaryRisky, testCanaryClean)},
	}
	for _, bucket := range ProviderEgressBuckets {
		for _, tripOnEmptiedCountry := range []bool{true, false} {
			settings := defaultArindbCanarySettings()
			settings.TripOnEmptiedCountry = tripOnEmptiedCountry
			providers := append(testCanaryProviders(100, "aa", true, testCanaryClean, testCanaryClean), scenario[bucket]...)
			decision := decideTestCanary(testCanaryPair(), providers, settings)
			b := decision.Buckets[bucket]
			if len(b.EmptiedCountries) != 1 || b.EmptiedCountries[0] != (ArindbCanaryEmptiedCountry{CountryCode: "bb", Old: 1}) {
				t.Errorf("%s: emptied countries %+v, want bb with old supply 1", bucket, b.EmptiedCountries)
			}
			if b.LossRatio > settings.BucketLossThreshold {
				t.Errorf("%s: the fixture's net loss %v trips on its own", bucket, b.LossRatio)
			}
			if b.Tripped != tripOnEmptiedCountry || (decision.State == ArindbCanaryStateFallback) != tripOnEmptiedCountry {
				t.Errorf("%s trip_on_emptied_country=%t: tripped=%t state=%s", bucket, tripOnEmptiedCountry, b.Tripped, decision.State)
			}
			for _, other := range ProviderEgressBuckets {
				if other != bucket && len(decision.Buckets[other].EmptiedCountries) != 0 {
					t.Errorf("%s fixture also emptied %s: %+v", bucket, other, decision.Buckets[other].EmptiedCountries)
				}
			}
		}
	}
}

// A city the latest empties is reported with its old supply, while its country
// keeps supply, and never trips.
func TestArindbCanaryReportsEmptiedCitiesWithoutTripping(t *testing.T) {
	emptiedCityId, keptCityId := server.NewId(), server.NewId()
	providers := []*arindbCanaryProvider{
		testCanaryProvider("aa", &emptiedCityId, true, testCanaryNonQuality, testCanaryClean),
		testCanaryProvider("aa", &emptiedCityId, true, testCanaryNonQuality, testCanaryClean),
	}
	for range 98 {
		providers = append(providers, testCanaryProvider("aa", &keptCityId, true, testCanaryClean, testCanaryClean))
	}
	decision := decideTestCanary(testCanaryPair(), providers, defaultArindbCanarySettings())
	quality := decision.Buckets[RankModeQuality]
	if quality.EmptiedCityCount != 1 || len(quality.EmptiedCities) != 1 ||
		quality.EmptiedCities[0] != (ArindbCanaryEmptiedCity{CityLocationId: emptiedCityId, CountryCode: "aa", Old: 2}) {
		t.Fatalf("emptied cities %d %+v", quality.EmptiedCityCount, quality.EmptiedCities)
	}
	if quality.Tripped || len(quality.EmptiedCountries) != 0 || decision.State != ArindbCanaryStateLatest {
		t.Fatalf("an emptied city tripped: tripped=%t countries=%+v state=%s", quality.Tripped, quality.EmptiedCountries, decision.State)
	}
}

// Without old supply in a bucket there is no loss to measure: the ratio is 0
// and the bucket holds, whatever the latest adds.
func TestArindbCanaryZeroOldSupplyNeverTrips(t *testing.T) {
	decision := decideTestCanary(testCanaryPair(), testCanaryProviders(100, "aa", true, testCanaryClean, testCanaryNonQuality), defaultArindbCanarySettings())
	quality := decision.Buckets[RankModeQuality]
	if quality.Old != 0 || quality.Latest != 100 || quality.Added != 100 || quality.LossRatio != 0 || quality.Tripped || decision.State != ArindbCanaryStateLatest {
		t.Fatalf("zero old supply: %+v state=%s", quality, decision.State)
	}
}

// The breaker uses net loss: a release that moves as many providers into a
// bucket as out of it passes, with both reported.
func TestArindbCanaryNetLossToleratesAsManyAdditions(t *testing.T) {
	providers := append(
		testCanaryProviders(80, "aa", true, testCanaryClean, testCanaryClean),
		testCanaryProviders(20, "aa", true, testCanaryNonQuality, testCanaryClean)...)
	providers = append(providers, testCanaryProviders(20, "aa", true, testCanaryClean, testCanaryNonQuality)...)
	decision := decideTestCanary(testCanaryPair(), providers, defaultArindbCanarySettings())
	quality := decision.Buckets[RankModeQuality]
	if quality.Removed != 20 || quality.Added != 20 || quality.LossRatio != 0 || quality.Tripped || decision.State != ArindbCanaryStateLatest {
		t.Fatalf("balanced move: %+v state=%s", quality, decision.State)
	}
}

// The breaker compares only providers with facts for both databases: one
// classified by a process without an old database cannot count as a loss.
func TestArindbCanaryComparesOnlyPairedProviders(t *testing.T) {
	providers := testCanaryProviders(99, "aa", true, testCanaryClean, testCanaryClean)
	unpaired := testCanaryProvider("bb", nil, true, testCanaryClean, testCanaryClean)
	unpaired.slots.oldEpoch = 0
	decision := decideTestCanary(testCanaryPair(), append(providers, unpaired), defaultArindbCanarySettings())
	if decision.Coverage != (ArindbCanaryCoverage{Providers: 100, Latest: 100, Old: 99, Paired: 99}) {
		t.Fatalf("coverage %+v", decision.Coverage)
	}
	quality := decision.Buckets[RankModeQuality]
	if quality.Old != 99 || quality.Latest != 99 || len(quality.EmptiedCountries) != 0 || quality.Countries["bb"] != (ArindbCanaryCount{}) {
		t.Fatalf("an unpaired provider entered the comparison: %+v", quality)
	}
}

// No usable old database in the indexer's own pair is single with S = 0,
// today's behavior, whatever the rows carry.
func TestArindbCanaryIsSingleWithoutAUsableOldDatabase(t *testing.T) {
	providers := testCanaryProviders(10, "aa", true, testCanaryNonQuality, testCanaryClean)
	for _, c := range []struct {
		name   string
		pair   arindbCanaryPair
		reason string
	}{
		{name: "missing", pair: func() arindbCanaryPair {
			p := testCanaryPair()
			p.old = server.ArinDatabaseIdentity{State: server.ArinDatabaseMissing}
			return p
		}(), reason: "old_missing"},
		{name: "identical", pair: func() arindbCanaryPair {
			p := testCanaryPair()
			p.old = p.latest
			p.old.State = server.ArinDatabaseIdentical
			return p
		}(), reason: "old_identical"},
		{name: "invalid", pair: func() arindbCanaryPair {
			p := testCanaryPair()
			p.old.State = server.ArinDatabaseInvalid
			return p
		}(), reason: "old_invalid"},
		{name: "loaded but not older", pair: testCanaryPairOf(testCanaryOldEpoch, testCanaryLatestEpoch), reason: "old_loaded"},
		{name: "latest unopenable", pair: func() arindbCanaryPair {
			p := testCanaryPair()
			p.err = errors.New("synthetic unopenable latest")
			p.latest.State = server.ArinDatabaseInvalid
			return p
		}(), reason: "latest_unopened"},
	} {
		decision := decideTestCanary(c.pair, providers, defaultArindbCanarySettings())
		if decision.State != ArindbCanaryStateSingle || decision.ServingEpoch != 0 || decision.SingleReason != c.reason {
			t.Errorf("%s: state=%s serving=%d reason=%s", c.name, decision.State, decision.ServingEpoch, decision.SingleReason)
		}
		for _, bucket := range ProviderEgressBuckets {
			if b := decision.Buckets[bucket]; b.Old != 0 || b.Latest != 0 || b.Tripped {
				t.Errorf("%s: single compared %s: %+v", c.name, bucket, b)
			}
		}
	}
}

// A provider whose rows another pair wrote: the latest slot at latestEpoch
// and the old slot at oldEpoch, 0 for none.
func testCanaryRowsOf(n int, latestEpoch int64, oldEpoch int64) []*arindbCanaryProvider {
	providers := testCanaryProviders(n, "aa", true, testCanaryClean, testCanaryClean)
	for _, provider := range providers {
		provider.slots.epoch = latestEpoch
		provider.slots.oldEpoch = oldEpoch
	}
	return providers
}

// Below paired coverage the indexer serves the database the rows carry: the
// old if it covers enough, else the latest if it does, else whichever covers
// more with ties to the old. It recovers to latest once the pair is covered.
func TestArindbCanaryWarmingServesTheDatabaseTheRowsCarry(t *testing.T) {
	for _, c := range []struct {
		name    string
		pair    arindbCanaryPair
		rows    []*arindbCanaryProvider
		state   string
		serving int64
	}{
		// Taskworker restarted onto the new pair before Connect: every row is
		// release N-1's, whose latest slot is this pair's old database
		{name: "taskworker ahead of connect", pair: testCanaryPair(),
			rows: testCanaryRowsOf(100, testCanaryOldEpoch, testCanaryOlderEpoch), state: ArindbCanaryStateWarming, serving: testCanaryOldEpoch},
		// Connect restarted onto the next pair before Taskworker: the rows'
		// old slot is this pair's latest database
		{name: "taskworker behind connect", pair: testCanaryPair(),
			rows: testCanaryRowsOf(100, testCanaryNewerEpoch, testCanaryLatestEpoch), state: ArindbCanaryStateWarming, serving: testCanaryLatestEpoch},
		{name: "neither covers, latest covers more", pair: testCanaryPair(),
			rows:  append(testCanaryRowsOf(60, testCanaryLatestEpoch, 0), testCanaryRowsOf(40, testCanaryOldEpoch, 0)...),
			state: ArindbCanaryStateWarming, serving: testCanaryLatestEpoch},
		{name: "neither covers, old covers more", pair: testCanaryPair(),
			rows:  append(testCanaryRowsOf(40, testCanaryLatestEpoch, 0), testCanaryRowsOf(60, testCanaryOldEpoch, 0)...),
			state: ArindbCanaryStateWarming, serving: testCanaryOldEpoch},
		{name: "neither covers, a tie goes to the old", pair: testCanaryPair(),
			rows:  append(testCanaryRowsOf(50, testCanaryLatestEpoch, 0), testCanaryRowsOf(50, testCanaryOldEpoch, 0)...),
			state: ArindbCanaryStateWarming, serving: testCanaryOldEpoch},
		{name: "a stuck host below coverage stays warming on the old", pair: testCanaryPair(),
			rows:  append(testCanaryRowsOf(97, testCanaryLatestEpoch, testCanaryOldEpoch), testCanaryRowsOf(3, testCanaryOldEpoch, testCanaryOlderEpoch)...),
			state: ArindbCanaryStateWarming, serving: testCanaryOldEpoch},
		{name: "recovery to latest at coverage", pair: testCanaryPair(),
			rows:  append(testCanaryRowsOf(98, testCanaryLatestEpoch, testCanaryOldEpoch), testCanaryRowsOf(2, testCanaryOldEpoch, testCanaryOlderEpoch)...),
			state: ArindbCanaryStateLatest, serving: testCanaryLatestEpoch},
		{name: "an empty cohort is covered", pair: testCanaryPair(), rows: nil, state: ArindbCanaryStateLatest, serving: testCanaryLatestEpoch},
	} {
		decision := decideTestCanary(c.pair, c.rows, defaultArindbCanarySettings())
		if decision.State != c.state || decision.ServingEpoch != c.serving {
			t.Errorf("%s: state=%s serving=%d coverage=%+v, want %s %d", c.name, decision.State, decision.ServingEpoch, decision.Coverage, c.state, c.serving)
		}
	}
}

// A warming run reports what it compared without acting on it.
func TestArindbCanaryWarmingNeverFallsBack(t *testing.T) {
	providers := append(
		testCanaryProviders(50, "aa", true, testCanaryNonQuality, testCanaryClean),
		testCanaryRowsOf(50, testCanaryOldEpoch, testCanaryOlderEpoch)...)
	decision := decideTestCanary(testCanaryPair(), providers, defaultArindbCanarySettings())
	if decision.State != ArindbCanaryStateWarming || !decision.Buckets[RankModeQuality].Tripped || decision.ServingEpoch != testCanaryOldEpoch {
		t.Fatalf("warming acted on its partial comparison: state=%s quality=%+v serving=%d", decision.State, decision.Buckets[RankModeQuality], decision.ServingEpoch)
	}
}

func testCanaryFilter(slots map[server.Id]arindbRollupSlots) providerCountFilter {
	filter := providerCountFilter{
		now: testCanaryClock, arinRisk: map[server.Id]bool{}, arinNonQuality: map[server.Id]bool{},
		reliabilityFailed: map[server.Id]bool{}, tlsAuthenticationFailed: map[server.Id]bool{},
		countryCodes: map[server.Id]string{}, healthCounts: map[server.Id]ProviderEgressHealthCounts{},
	}
	// the rollup's latest slots, as the common filter loads them
	for clientId, s := range slots {
		if s.risk {
			filter.arinRisk[clientId] = true
		}
		if s.nonQuality {
			filter.arinNonQuality[clientId] = true
		}
	}
	return filter
}

// One choice for the whole publication: in fallback every provider takes the
// old classification, including one the latest database classifies better.
func TestArindbCanaryFallbackServesTheOldClassificationToEveryProvider(t *testing.T) {
	betterUnderLatest, worseUnderLatest := server.NewId(), server.NewId()
	slots := map[server.Id]arindbRollupSlots{
		betterUnderLatest: {epoch: testCanaryLatestEpoch, oldEpoch: testCanaryOldEpoch, oldNonQuality: true},
		worseUnderLatest:  {epoch: testCanaryLatestEpoch, oldEpoch: testCanaryOldEpoch, nonQuality: true},
	}
	filter := testCanaryFilter(slots)
	applyArindbServingEpoch(&filter, slots, testCanaryOldEpoch)
	if !filter.arinNonQuality[betterUnderLatest] || filter.arinNonQuality[worseUnderLatest] || filter.arindbServingEpoch != testCanaryOldEpoch {
		t.Fatalf("fallback decided providers separately: better=%t worse=%t", filter.arinNonQuality[betterUnderLatest], filter.arinNonQuality[worseUnderLatest])
	}
	filter = testCanaryFilter(slots)
	applyArindbServingEpoch(&filter, slots, testCanaryLatestEpoch)
	if filter.arinNonQuality[betterUnderLatest] || !filter.arinNonQuality[worseUnderLatest] {
		t.Fatal("latest did not serve the latest classification")
	}
}

// Risk always comes from the latest slot: a fallback keeps a latest-only risk
// veto and never readmits that provider to any bucket.
func TestArindbCanaryFallbackKeepsTheLatestRiskVeto(t *testing.T) {
	riskyUnderLatest := server.NewId()
	slots := map[server.Id]arindbRollupSlots{
		riskyUnderLatest: {epoch: testCanaryLatestEpoch, oldEpoch: testCanaryOldEpoch, risk: true},
	}
	filter := testCanaryFilter(slots)
	applyArindbServingEpoch(&filter, slots, testCanaryOldEpoch)
	passing := true
	facts := filter.egressFacts(riskyUnderLatest, nil, nil, &passing, DefaultEgressIndexSettings())
	decision := decideProviderEgress(facts, false)
	if !filter.hasHardEgressFailure(riskyUnderLatest) || !decision.hardExcluded || decision.quality || decision.speed || decision.online {
		t.Fatalf("fallback readmitted a provider the latest calls risky: %+v", decision)
	}
	if filter.arinNonQuality[riskyUnderLatest] {
		t.Fatal("the old clean classification was not served")
	}
}

// Facts unavailable under S are non-Quality, never clean; S = 0 is each row's
// latest slot.
func TestArindbCanaryUnavailableFactsAreNotQuality(t *testing.T) {
	lagging := arindbRollupSlots{epoch: testCanaryOldEpoch, oldEpoch: testCanaryOlderEpoch}
	ahead := arindbRollupSlots{epoch: testCanaryNewerEpoch, oldEpoch: testCanaryLatestEpoch, oldNonQuality: false}
	mixed := arindbRollupSlots{}
	for _, c := range []struct {
		name         string
		slots        arindbRollupSlots
		servingEpoch int64
		nonQuality   bool
	}{
		{name: "a not yet restarted row under the new latest", slots: lagging, servingEpoch: testCanaryLatestEpoch, nonQuality: true},
		{name: "a not yet restarted row under the old", slots: lagging, servingEpoch: testCanaryOldEpoch, nonQuality: false},
		{name: "a newer row's old slot", slots: ahead, servingEpoch: testCanaryLatestEpoch, nonQuality: false},
		{name: "a mixed rollup", slots: mixed, servingEpoch: testCanaryLatestEpoch, nonQuality: true},
		{name: "single serves the latest slot", slots: mixed, servingEpoch: 0, nonQuality: false},
	} {
		if got := c.slots.servingNonQuality(c.servingEpoch); got != c.nonQuality {
			t.Errorf("%s: non-quality=%t, want %t", c.name, got, c.nonQuality)
		}
	}
}

// The comparison applies the export's own rules with only the ARIN facts
// substituted: risk removes every bucket, non-quality only Quality, failing
// health keeps Online only, and the reliability minimums still hold.
func TestArindbCanaryMembershipIsTheExportRule(t *testing.T) {
	healthy, failing, unreliable := server.NewId(), server.NewId(), server.NewId()
	filter := testCanaryFilter(nil)
	filter.healthCounts[healthy] = ProviderEgressHealthCounts{MeasuredAt: testCanaryClock, FirstMeasuredAt: testCanaryClock, OKCount: 10, Total: 10}
	filter.healthCounts[failing] = ProviderEgressHealthCounts{MeasuredAt: testCanaryClock, FirstMeasuredAt: testCanaryClock, OKCount: 1, Total: 10}
	filter.healthCounts[unreliable] = filter.healthCounts[healthy]
	minimums := map[int]float64{1: 0.95, 2: 0.7, 3: 0.6}
	membership := arindbCanaryMembershipRule(&filter, DefaultEgressIndexSettings(), false, minimums)
	provider := func(clientId server.Id, weight float64) *arindbCanaryProvider {
		return &arindbCanaryProvider{clientId: clientId, egress: &clientScoreEgress{}, weights: map[int]float64{1: weight}}
	}
	for _, c := range []struct {
		name             string
		provider         *arindbCanaryProvider
		risk, nonQuality bool
		want             arindbCanaryMembership
	}{
		{name: "clean", provider: provider(healthy, 1), want: arindbCanaryMembership{quality: true, speed: true, online: true}},
		{name: "non-quality", provider: provider(healthy, 1), nonQuality: true, want: arindbCanaryMembership{speed: true, online: true}},
		{name: "risk", provider: provider(healthy, 1), risk: true},
		{name: "failing health", provider: provider(failing, 1), want: arindbCanaryMembership{online: true}},
		{name: "below the reliability minimum", provider: provider(unreliable, 0.5)},
	} {
		if got := membership(c.provider, c.risk, c.nonQuality); got != c.want {
			t.Errorf("%s: membership %+v, want %+v", c.name, got, c.want)
		}
	}
	// the filter's own ARIN facts never leak into the substituted ones
	filter.arinRisk[healthy] = true
	if got := membership(provider(healthy, 1), false, false); got != (arindbCanaryMembership{quality: true, speed: true, online: true}) {
		t.Fatalf("the filter's latest risk leaked into the substituted facts: %+v", got)
	}
}

// One comparison provider per public client from its location row, every
// lookback weight attached; group-only and Network-only clients still belong
// to the cohort the serving epoch is applied to.
func TestArindbCanaryProvidersComeFromPublicLocationRows(t *testing.T) {
	public, networkOnly, groupOnly := server.NewId(), server.NewId(), server.NewId()
	cityId := server.NewId()
	country := "AA "
	row := func(clientId server.Id, lookback int, weight float64, networkOnlyRow bool, group bool) clientScoreSourceRow {
		return clientScoreSourceRow{
			score:  &ClientScore{ClientId: clientId, LookbackIndex: lookback, IndependentReliabilityWeight: weight, NetworkOnly: networkOnlyRow},
			cityId: &cityId, egress: &clientScoreEgress{publishedCountryCode: &country}, locationGroup: group,
		}
	}
	providers, cohortIds := arindbCanaryProvidersFromRows([]clientScoreSourceRow{
		row(public, 1, 0.9, false, false), row(public, 2, 0.8, false, false), row(public, 1, 0.9, false, true),
		row(networkOnly, 1, 1, true, false), row(groupOnly, 1, 1, false, true),
	})
	if len(providers) != 1 || providers[0].clientId != public || providers[0].countryCode != "aa" || *providers[0].cityId != cityId ||
		providers[0].weights[1] != 0.9 || providers[0].weights[2] != 0.8 {
		t.Fatalf("comparison providers %+v", providers)
	}
	if len(cohortIds) != 3 {
		t.Fatalf("cohort %v, want every client", cohortIds)
	}
}

// An in-memory decision store that records the ttl of each write.
type testArindbCanaryDecisionStore struct {
	value []byte
	ttl   time.Duration
	err   error
}

func (self *testArindbCanaryDecisionStore) set(ctx context.Context, value []byte, ttl time.Duration) error {
	if self.err != nil {
		return self.err
	}
	self.value = value
	self.ttl = ttl
	return nil
}

func (self *testArindbCanaryDecisionStore) get(ctx context.Context) ([]byte, error) {
	return self.value, self.err
}

func useTestArindbCanaryDecisionStore(t *testing.T) *testArindbCanaryDecisionStore {
	t.Helper()
	store := &testArindbCanaryDecisionStore{}
	previous := arindbCanaryDecisions
	arindbCanaryDecisions = store
	t.Cleanup(func() { arindbCanaryDecisions = previous })
	return store
}

// The decision is written with the export's ttl as versioned JSON and reads
// back as written; another version, a serving epoch its state cannot choose
// or incomplete provenance is refused, and absence is no decision.
func TestArindbCanaryDecisionIsVersionedJsonWithTheExportTtl(t *testing.T) {
	store := useTestArindbCanaryDecisionStore(t)
	if decision, err := GetArindbCanaryDecision(t.Context()); decision != nil || err != nil {
		t.Fatalf("absent decision read as %+v %v", decision, err)
	}
	decision := decideTestCanary(testCanaryPair(), testCanaryProviders(10, "aa", true, testCanaryNonQuality, testCanaryClean), defaultArindbCanarySettings())
	if decision.State != ArindbCanaryStateFallback {
		t.Fatalf("fixture state %s", decision.State)
	}
	if err := writeArindbCanaryDecision(t.Context(), decision, 300*time.Minute); err != nil {
		t.Fatal(err)
	}
	if store.ttl != 300*time.Minute {
		t.Fatalf("decision written with ttl %s, want the export's 300m", store.ttl)
	}
	var document map[string]any
	if err := json.Unmarshal(store.value, &document); err != nil || document["version"] != float64(ArindbCanaryDecisionVersion) {
		t.Fatalf("decision is not versioned JSON: %v %s", err, store.value)
	}
	read, err := GetArindbCanaryDecision(t.Context())
	if err != nil || read.DecisionId != decision.DecisionId || read.State != decision.State || read.ServingEpoch != testCanaryOldEpoch ||
		read.Old.Sha256 != decision.Old.Sha256 || read.Buckets[RankModeQuality].Old != 10 || !read.DecidedAt.Equal(testCanaryClock) {
		t.Fatalf("decision read back as %+v %v", read, err)
	}
	for name, edit := range map[string]func(map[string]any){
		"another version":               func(d map[string]any) { d["version"] = 2 },
		"fallback serving the latest":   func(d map[string]any) { d["serving_epoch"] = testCanaryLatestEpoch },
		"single serving a database":     func(d map[string]any) { d["state"] = ArindbCanaryStateSingle },
		"an unknown state":              func(d map[string]any) { d["state"] = "paused" },
		"a missing bucket":              func(d map[string]any) { delete(d["buckets"].(map[string]any), RankModeSpeed) },
		"no decision time":              func(d map[string]any) { delete(d, "decided_at") },
		"a decision before its source":  func(d map[string]any) { d["source_started_at"] = testCanaryClock.Add(time.Hour) },
		"warming serving neither epoch": func(d map[string]any) { d["state"] = ArindbCanaryStateWarming; d["serving_epoch"] = 7 },
	} {
		var edited map[string]any
		if err := json.Unmarshal(store.value, &edited); err != nil {
			t.Fatal(err)
		}
		edit(edited)
		data, err := json.Marshal(edited)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseArindbCanaryDecision(data); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := parseArindbCanaryDecision([]byte("{not json")); err == nil {
		t.Error("malformed decision was accepted")
	}
}

// A failed decision write is an error the export stops on.
func TestArindbCanaryDecisionWriteFailureIsReturned(t *testing.T) {
	store := useTestArindbCanaryDecisionStore(t)
	store.err = errors.New("synthetic store failure")
	decision := decideTestCanary(testCanaryPair(), nil, defaultArindbCanarySettings())
	if err := writeArindbCanaryDecision(t.Context(), decision, time.Minute); err == nil {
		t.Fatal("a failed decision write was swallowed")
	}
}

// Absent keys use the defaults; out-of-range or unreadable values keep them.
func TestArindbCanarySettingsUseDefaultsForAbsentAndInvalidKeys(t *testing.T) {
	defaults := defaultArindbCanarySettings()
	if defaults != (ArindbCanarySettings{BucketLossThreshold: 0.10, TripOnEmptiedCountry: true, MinCoverage: 0.98}) {
		t.Fatalf("defaults %+v", defaults)
	}
	if got := arindbCanarySettingsFromResource(nil, server.ErrResourceNotFound); got != defaults {
		t.Fatalf("absent provider.yml: %+v", got)
	}
	for _, c := range []struct {
		config string
		want   ArindbCanarySettings
	}{
		{config: "{}", want: defaults},
		{config: "arindb_canary: {}", want: defaults},
		{config: "arindb_canary: {bucket_loss_threshold: 0.2, trip_on_emptied_country: false, min_coverage: 0.9}",
			want: ArindbCanarySettings{BucketLossThreshold: 0.2, TripOnEmptiedCountry: false, MinCoverage: 0.9}},
		{config: "arindb_canary: {min_coverage: 0.5}", want: ArindbCanarySettings{BucketLossThreshold: 0.10, TripOnEmptiedCountry: true, MinCoverage: 0.5}},
		{config: "arindb_canary: {bucket_loss_threshold: 1.5, min_coverage: 0}", want: defaults},
		{config: "arindb_canary: {bucket_loss_threshold: -0.1, min_coverage: 1.01}", want: defaults},
		{config: "arindb_canary: {bucket_loss_threshold: .nan}", want: defaults},
		{config: "arindb_canary: [0.2]", want: defaults},
		{config: "arindb_canary: {bucket_loss_threshold: loose}", want: defaults},
	} {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(c.config))
		got := arindbCanarySettings()
		pop()
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.config, got, c.want)
		}
	}
}

// The native census carries the decision's summary, and a census written
// before the canary still reads.
func TestClientScoreNativeCensusCarriesTheArindbSummary(t *testing.T) {
	decision := decideTestCanary(testCanaryPair(), testCanaryProviders(3, "aa", true, testCanaryClean, testCanaryClean), defaultArindbCanarySettings())
	census := &ClientScoreNativeCensus{SchemaVersion: 1, Arindb: decision.nativeSummary()}
	data, err := json.Marshal(census)
	if err != nil {
		t.Fatal(err)
	}
	var read ClientScoreNativeCensus
	if err := json.Unmarshal(data, &read); err != nil || read.Arindb == nil || read.Arindb.State != ArindbCanaryStateLatest ||
		read.Arindb.ServingEpoch != testCanaryLatestEpoch || read.Arindb.Buckets[RankModeQuality] != (ArindbCanaryCount{Latest: 3, Old: 3}) {
		t.Fatalf("census summary read as %+v %v", read.Arindb, err)
	}
	var older ClientScoreNativeCensus
	if err := json.Unmarshal([]byte(`{"schema_version":1,"publication_id":"synthetic","buckets":{}}`), &older); err != nil || older.Arindb != nil {
		t.Fatalf("a census without the canary summary: %+v %v", older.Arindb, err)
	}
}
