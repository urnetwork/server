// The canary's comparison, breaker and state machine (arindbctl/ARINDB.md,
// "Comparison and breaker" and "Test plan"), over synthetic providers and an
// injected clock.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	return decideTestCanaryAfter(nil, pair, providers, settings)
}

// The publication after previous, one minute later.
func decideTestCanaryAfter(previous *ArindbCanaryDecision, pair arindbCanaryPair, providers []*arindbCanaryProvider, settings ArindbCanarySettings) *ArindbCanaryDecision {
	decidedAt := testCanaryClock
	if previous != nil {
		decidedAt = previous.DecidedAt.Add(time.Minute)
	}
	return decideArindbCanary(pair, providers, testCanaryMembership, settings, previous, decidedAt.Add(-time.Minute), decidedAt)
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
			// only a Quality trip falls back
			wantState, wantServing := ArindbCanaryStateLatest, testCanaryLatestEpoch
			if c.tripped && bucket == RankModeQuality {
				wantState, wantServing = ArindbCanaryStateFallback, testCanaryOldEpoch
			}
			if decision.State != wantState || decision.ServingEpoch != wantServing {
				t.Errorf("%s lost %d: state=%s serving=%d, want %s %d", bucket, c.lost, decision.State, decision.ServingEpoch, wantState, wantServing)
			}
		}
	}
}

// Only a Quality trip falls back. The serving epoch changes only Quality
// classification, since risk always comes from the latest slot, so a Speed or
// Online trip, which only risk causes, is reported without switching the
// fleet to the old database's Quality, which would repair none of it.
func TestArindbCanaryFallsBackOnlyWhenTheQualityBucketTrips(t *testing.T) {
	// 20 of 100 Speed providers turn risky under the latest; Quality is empty
	// under both databases
	providers := append(
		testCanaryProviders(20, "aa", true, testCanaryRisky, testCanaryNonQuality),
		testCanaryProviders(80, "aa", true, testCanaryNonQuality, testCanaryNonQuality)...)
	providers = append(providers, testCanaryProviders(300, "aa", false, testCanaryClean, testCanaryClean)...)
	decision := decideTestCanary(testCanaryPair(), providers, defaultArindbCanarySettings())
	if !decision.Buckets[RankModeSpeed].Tripped || decision.Buckets[RankModeQuality].Tripped || decision.Buckets[ProviderEgressBucketOnline].Tripped {
		t.Fatalf("fixture trips quality=%t speed=%t online=%t", decision.Buckets[RankModeQuality].Tripped,
			decision.Buckets[RankModeSpeed].Tripped, decision.Buckets[ProviderEgressBucketOnline].Tripped)
	}
	if decision.State != ArindbCanaryStateLatest || decision.ServingEpoch != testCanaryLatestEpoch {
		t.Fatalf("a Speed-only trip: state=%s serving=%d, want latest", decision.State, decision.ServingEpoch)
	}
}

// A country with min_emptied_country_supply under the old database and none
// under the latest trips the breaker in each bucket, however small its share;
// the switch can turn the rule off without hiding the report.
func TestArindbCanaryBreakerTripsOnACountryEmptiedInEachBucket(t *testing.T) {
	scenario := map[string][]*arindbCanaryProvider{
		RankModeQuality: testCanaryProviders(3, "bb", true, testCanaryNonQuality, testCanaryClean),
		RankModeSpeed: append(
			testCanaryProviders(3, "bb", true, testCanaryRisky, testCanaryNonQuality),
			testCanaryProviders(3, "bb", false, testCanaryClean, testCanaryClean)...),
		ProviderEgressBucketOnline: testCanaryProviders(3, "bb", false, testCanaryRisky, testCanaryClean),
	}
	for _, bucket := range ProviderEgressBuckets {
		for _, tripOnEmptiedCountry := range []bool{true, false} {
			settings := defaultArindbCanarySettings()
			settings.TripOnEmptiedCountry = tripOnEmptiedCountry
			providers := append(testCanaryProviders(100, "aa", true, testCanaryClean, testCanaryClean), scenario[bucket]...)
			decision := decideTestCanary(testCanaryPair(), providers, settings)
			b := decision.Buckets[bucket]
			if len(b.EmptiedCountries) != 1 || b.EmptiedCountries[0] != (ArindbCanaryEmptiedCountry{CountryCode: "bb", Old: 3, Trips: true}) {
				t.Errorf("%s: emptied countries %+v, want bb with old supply 3", bucket, b.EmptiedCountries)
			}
			if b.LossRatio > settings.BucketLossThreshold {
				t.Errorf("%s: the fixture's net loss %v trips on its own", bucket, b.LossRatio)
			}
			if b.Tripped != tripOnEmptiedCountry || (decision.State == ArindbCanaryStateFallback) != (tripOnEmptiedCountry && bucket == RankModeQuality) {
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

// An emptied country trips only with min_emptied_country_supply under the old
// database, so one provider connecting and disconnecting cannot flip the
// fleet; a smaller one is reported.
func TestArindbCanaryEmptiedCountryNeedsTheMinimumOldSupplyToTrip(t *testing.T) {
	for _, c := range []struct {
		old   int
		trips bool
	}{{old: 1}, {old: 2}, {old: 3, trips: true}} {
		providers := append(
			testCanaryProviders(100, "aa", true, testCanaryClean, testCanaryClean),
			testCanaryProviders(c.old, "bb", true, testCanaryNonQuality, testCanaryClean)...)
		decision := decideTestCanary(testCanaryPair(), providers, defaultArindbCanarySettings())
		quality := decision.Buckets[RankModeQuality]
		if len(quality.EmptiedCountries) != 1 || quality.EmptiedCountries[0] != (ArindbCanaryEmptiedCountry{CountryCode: "bb", Old: c.old, Trips: c.trips}) {
			t.Errorf("old supply %d: emptied countries %+v", c.old, quality.EmptiedCountries)
		}
		if quality.Tripped != c.trips || (decision.State == ArindbCanaryStateFallback) != c.trips {
			t.Errorf("old supply %d: tripped=%t state=%s, want tripped=%t", c.old, quality.Tripped, decision.State, c.trips)
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

// Gains and gross churn, which the net loss does not trip on, are measured for
// paging: a flood into a bucket, and a reshuffle that moves as many out as in.
func TestArindbCanaryMeasuresGainsAndGrossChurn(t *testing.T) {
	flood := append(
		testCanaryProviders(100, "aa", true, testCanaryClean, testCanaryClean),
		testCanaryProviders(30, "aa", true, testCanaryClean, testCanaryNonQuality)...)
	quality := decideTestCanary(testCanaryPair(), flood, defaultArindbCanarySettings()).Buckets[RankModeQuality]
	if quality.GainRatio != 0.3 || quality.ChurnRatio != 0.3 || quality.LossRatio != -0.3 || quality.Tripped {
		t.Fatalf("a flood: %+v", quality)
	}
	reshuffle := append(
		testCanaryProviders(80, "aa", true, testCanaryClean, testCanaryClean),
		testCanaryProviders(20, "aa", true, testCanaryNonQuality, testCanaryClean)...)
	reshuffle = append(reshuffle, testCanaryProviders(20, "aa", true, testCanaryClean, testCanaryNonQuality)...)
	quality = decideTestCanary(testCanaryPair(), reshuffle, defaultArindbCanarySettings()).Buckets[RankModeQuality]
	if quality.GainRatio != 0 || quality.ChurnRatio != 0.4 || quality.LossRatio != 0 || quality.Tripped {
		t.Fatalf("a reshuffle: %+v", quality)
	}
}

// The cohort is compared with its largest size over the last two hours, from
// samples the decisions carry at most every ten minutes, so providers that
// never reach the cohort, such as those whose latest lookup fails, show even
// when a rolling restart drops them gradually.
func TestArindbCanaryComparesTheCohortWithItsRecentSize(t *testing.T) {
	settings := defaultArindbCanarySettings()
	var decision *ArindbCanaryDecision
	at := func(minutes int, providers int) {
		t.Helper()
		rows := testCanaryProviders(providers, "aa", true, testCanaryClean, testCanaryClean)
		decision = decideArindbCanary(testCanaryPair(), rows, testCanaryMembership, settings, decision,
			testCanaryClock.Add(time.Duration(minutes-1)*time.Minute), testCanaryClock.Add(time.Duration(minutes)*time.Minute))
	}
	at(0, 200)
	if decision.Cohort.ReferenceProviders != 0 || decision.Cohort.LossRatio != 0 || len(decision.Cohort.History) != 1 {
		t.Fatalf("first decision: %+v", decision.Cohort)
	}
	// within the sample interval no sample is added
	at(5, 196)
	at(10, 190)
	at(40, 170)
	if len(decision.Cohort.History) != 3 || decision.Cohort.ReferenceProviders != 200 || decision.Cohort.LossRatio != 0.15 {
		t.Fatalf("a gradual drop: %+v", decision.Cohort)
	}
	// the window forgets the larger cohort after two hours
	at(125, 170)
	if decision.Cohort.ReferenceProviders != 190 || decision.Cohort.History[0].Providers != 190 {
		t.Fatalf("after the window: %+v", decision.Cohort)
	}
	// a growing cohort is no loss
	at(140, 400)
	if decision.Cohort.LossRatio != 0 {
		t.Fatalf("a growing cohort: %+v", decision.Cohort)
	}
}

// A decision records when its pair was first compared, carried while the pair
// is unchanged, so a cohort that shrinks soon after a release can be told from
// one that shrinks for another reason.
func TestArindbCanaryRecordsWhenThePairWasFirstCompared(t *testing.T) {
	settings := defaultArindbCanarySettings()
	first := decideTestCanary(testCanaryPair(), testCanaryLossRun(0), settings)
	if !first.PairSince.Equal(first.DecidedAt) {
		t.Fatalf("first decision: pair since %s, decided %s", first.PairSince, first.DecidedAt)
	}
	same := decideTestCanaryAfter(first, testCanaryPair(), testCanaryLossRun(0), settings)
	if !same.PairSince.Equal(first.DecidedAt) {
		t.Fatalf("the same pair: pair since %s, want %s", same.PairSince, first.DecidedAt)
	}
	next := testCanaryLossRun(0)
	for _, provider := range next {
		provider.slots.epoch, provider.slots.oldEpoch = testCanaryNewerEpoch, testCanaryLatestEpoch
	}
	changed := decideTestCanaryAfter(same, testCanaryPairOf(testCanaryNewerEpoch, testCanaryLatestEpoch), next, settings)
	if !changed.PairSince.Equal(changed.DecidedAt) {
		t.Fatalf("a new pair: pair since %s, decided %s", changed.PairSince, changed.DecidedAt)
	}
}

// Each decision that serves another epoch than the previous one is a change,
// kept for six hours so a canary that switches the fleet back and forth pages.
func TestArindbCanaryCountsTheServingEpochsChanges(t *testing.T) {
	settings := defaultArindbCanarySettings()
	settings.ReleasePassingRuns = 1
	var decision *ArindbCanaryDecision
	at := func(minutes int, lost int) {
		t.Helper()
		decision = decideArindbCanary(testCanaryPair(), testCanaryLossRun(lost), testCanaryMembership, settings, decision,
			testCanaryClock.Add(time.Duration(minutes-1)*time.Minute), testCanaryClock.Add(time.Duration(minutes)*time.Minute))
	}
	at(0, 0)
	if len(decision.ServingChanges) != 0 {
		t.Fatalf("the first decision counted %v", decision.ServingChanges)
	}
	// a trip, a release and another trip switch the fleet three times
	at(10, 20)
	at(20, 0)
	at(30, 0)
	at(40, 20)
	if len(decision.ServingChanges) != 3 || !decision.ServingChanges[2].Equal(testCanaryClock.Add(40*time.Minute)) {
		t.Fatalf("serving changes %v, want three", decision.ServingChanges)
	}
	// the window forgets changes older than six hours
	at(400, 20)
	if len(decision.ServingChanges) != 1 {
		t.Fatalf("after the window: %v", decision.ServingChanges)
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

// A Taskworker that cannot use its own pair compares the previous decision's,
// since the comparison needs only epochs and the rows carry the facts: a
// damaged file on the publishing host cannot drop a fallback. It says why, and
// that its latest is unavailable, which pages. Without a compared previous
// decision, or beside another latest, it is single.
func TestArindbCanaryComparesThePreviousPairWhenTheTaskworkerCannotUseItsOwn(t *testing.T) {
	settings := defaultArindbCanarySettings()
	fellBack := decideTestCanary(testCanaryPair(), testCanaryLossRun(20), settings)
	if fellBack.State != ArindbCanaryStateFallback {
		t.Fatalf("fixture state %s", fellBack.State)
	}
	unopened := arindbCanaryPair{
		latest: server.ArinDatabaseIdentity{State: server.ArinDatabaseInvalid},
		old:    server.ArinDatabaseIdentity{State: server.ArinDatabaseUnopened},
		err:    errors.New("synthetic unopenable latest"),
	}
	decision := decideTestCanaryAfter(fellBack, unopened, testCanaryLossRun(20), settings)
	if decision.State != ArindbCanaryStateFallback || decision.ServingEpoch != testCanaryOldEpoch || decision.Latch == nil ||
		decision.PairCarriedReason != "latest_unopened" || !decision.LatestUnavailable || decision.Latest != fellBack.Latest || decision.Old != fellBack.Old {
		t.Fatalf("latest unopened: state=%s serving=%d carried=%q unavailable=%t latch=%+v", decision.State, decision.ServingEpoch,
			decision.PairCarriedReason, decision.LatestUnavailable, decision.Latch)
	}
	// its own latest beside an invalid old borrows the previous old
	invalidOld := testCanaryPair()
	invalidOld.old.State = server.ArinDatabaseInvalid
	decision = decideTestCanaryAfter(fellBack, invalidOld, testCanaryLossRun(20), settings)
	if decision.State != ArindbCanaryStateFallback || decision.PairCarriedReason != "old_invalid" || decision.LatestUnavailable || decision.Old != fellBack.Old {
		t.Fatalf("old invalid: state=%s carried=%q unavailable=%t", decision.State, decision.PairCarriedReason, decision.LatestUnavailable)
	}
	// another latest cannot borrow the previous pair's old
	nextInvalidOld := testCanaryPairOf(testCanaryNewerEpoch, testCanaryLatestEpoch)
	nextInvalidOld.old.State = server.ArinDatabaseInvalid
	decision = decideTestCanaryAfter(fellBack, nextInvalidOld, testCanaryLossRun(20), settings)
	if decision.State != ArindbCanaryStateSingle || decision.SingleReason != "old_invalid" || decision.PairCarriedReason != "" {
		t.Fatalf("another latest: state=%s reason=%s carried=%q", decision.State, decision.SingleReason, decision.PairCarriedReason)
	}
	// without a compared previous decision the canary is single
	for name, previous := range map[string]*ArindbCanaryDecision{"no previous decision": nil, "a single previous decision": decision} {
		single := decideTestCanaryAfter(previous, unopened, testCanaryLossRun(20), settings)
		if single.State != ArindbCanaryStateSingle || single.SingleReason != "latest_unopened" || !single.LatestUnavailable || single.PairCarriedReason != "" {
			t.Errorf("%s: state=%s reason=%s unavailable=%t carried=%q", name, single.State, single.SingleReason, single.LatestUnavailable, single.PairCarriedReason)
		}
	}
}

// Single right after a fallback is flagged, which pages: the comparison that
// held the latest out is gone. The flag lasts while single does.
func TestArindbCanaryFlagsSingleRightAfterAFallback(t *testing.T) {
	settings := defaultArindbCanarySettings()
	fellBack := decideTestCanary(testCanaryPair(), testCanaryLossRun(20), settings)
	// the next release ships without an old database
	withoutOld := testCanaryPairOf(testCanaryNewerEpoch, testCanaryLatestEpoch)
	withoutOld.old = server.ArinDatabaseIdentity{State: server.ArinDatabaseMissing}
	single := decideTestCanaryAfter(fellBack, withoutOld, testCanaryLossRun(0), settings)
	if single.State != ArindbCanaryStateSingle || !single.FallbackDropped {
		t.Fatalf("after a fallback: state=%s fallback_dropped=%t", single.State, single.FallbackDropped)
	}
	if again := decideTestCanaryAfter(single, withoutOld, testCanaryLossRun(0), settings); !again.FallbackDropped {
		t.Fatal("the flag did not last while single did")
	}
	passed := decideTestCanary(testCanaryPair(), testCanaryLossRun(0), settings)
	if after := decideTestCanaryAfter(passed, withoutOld, testCanaryLossRun(0), settings); after.FallbackDropped {
		t.Fatal("single after a passing decision was flagged")
	}
}

// A latch held while the decision serves another database than its old one
// is a dropped fallback, which pages: Connect restarted onto the next release
// before the Taskworker, the old database's coverage fell below what warming
// may serve, and warming serves each row's latest slot, the latest the latch
// holds out included.
func TestArindbCanaryFlagsALatchHeldWithoutItsOldDatabase(t *testing.T) {
	settings := defaultArindbCanarySettings()
	fellBack := decideTestCanary(testCanaryPair(), testCanaryLossRun(20), settings)
	// 60 providers' rows come from Connect on the next release, whose old
	// database is this pair's latest
	rows := append(testCanaryLossRun(0)[:40], testCanaryRowsOf(60, testCanaryNewerEpoch, testCanaryLatestEpoch)...)
	decision := decideTestCanaryAfter(fellBack, testCanaryPair(), rows, settings)
	if decision.State != ArindbCanaryStateWarming || decision.ServingEpoch != 0 || decision.Latch == nil {
		t.Fatalf("fixture: state=%s serving=%d latch=%+v", decision.State, decision.ServingEpoch, decision.Latch)
	}
	if !decision.FallbackDropped || !decision.Flags()[ArindbCanaryFlagFallbackDropped] {
		t.Fatal("a held latch whose old database is not served was not flagged")
	}
	// while the old database is served the latch is honoured
	covered := append(testCanaryLossRun(0)[:95], testCanaryRowsOf(5, testCanaryNewerEpoch, testCanaryLatestEpoch)...)
	if decision := decideTestCanaryAfter(fellBack, testCanaryPair(), covered, settings); decision.ServingEpoch != testCanaryOldEpoch || decision.FallbackDropped {
		t.Fatalf("an honoured latch: serving=%d fallback_dropped=%t", decision.ServingEpoch, decision.FallbackDropped)
	}
}

// A release built without the canary's baseline compares against the latest a
// latched fallback held out: the latch is dropped, which pages and lasts while
// the pair's old database is that latest. A release compared against the
// latch's own old database keeps the protection, and accepting the held-out
// latest clears the flag.
func TestArindbCanaryFlagsANewPairWhoseOldDatabaseALatchRejected(t *testing.T) {
	settings := defaultArindbCanarySettings()
	fellBack := decideTestCanary(testCanaryPair(), testCanaryLossRun(20), settings)
	rowsOf := func(oldEpoch int64) []*arindbCanaryProvider {
		rows := testCanaryLossRun(0)
		for _, provider := range rows {
			provider.slots.epoch, provider.slots.oldEpoch = testCanaryNewerEpoch, oldEpoch
		}
		return rows
	}
	ungated := testCanaryPairOf(testCanaryNewerEpoch, testCanaryLatestEpoch)
	decision := decideTestCanaryAfter(fellBack, ungated, rowsOf(testCanaryLatestEpoch), settings)
	if decision.State != ArindbCanaryStateLatest || !decision.FallbackDropped || decision.DroppedLatch == nil || *decision.DroppedLatch != *fellBack.Latch {
		t.Fatalf("an ungated release: state=%s fallback_dropped=%t dropped=%+v", decision.State, decision.FallbackDropped, decision.DroppedLatch)
	}
	if again := decideTestCanaryAfter(decision, ungated, rowsOf(testCanaryLatestEpoch), settings); !again.FallbackDropped || again.DroppedLatch == nil {
		t.Fatal("the dropped latch did not last while the pair's old database was the held-out latest")
	}
	accept := settings
	accept.AcceptLatestBuildEpoch = testCanaryLatestEpoch
	if accepted := decideTestCanaryAfter(decision, ungated, rowsOf(testCanaryLatestEpoch), accept); accepted.FallbackDropped || accepted.DroppedLatch != nil {
		t.Fatal("accepting the held-out latest did not clear the dropped latch")
	}
	pinned := testCanaryPairOf(testCanaryNewerEpoch, testCanaryOldEpoch)
	if decision := decideTestCanaryAfter(fellBack, pinned, rowsOf(testCanaryOldEpoch), settings); decision.FallbackDropped || decision.DroppedLatch != nil {
		t.Fatalf("a release compared against the latch's old database: fallback_dropped=%t dropped=%+v", decision.FallbackDropped, decision.DroppedLatch)
	}
}

// A dropped latch lasts however the pairs move on, since every later baseline
// descends from the rejected latest: until its latest is accepted, or the
// canary compares against the latch's old database again, verifying a latest
// against it or latching it anew.
func TestArindbCanaryKeepsADroppedLatchUntilItsOldDatabaseIsComparedAgain(t *testing.T) {
	settings := defaultArindbCanarySettings()
	newestEpoch := testCanaryNewerEpoch + 86400
	rowsOf := func(latestEpoch int64, oldEpoch int64, lost int) []*arindbCanaryProvider {
		rows := testCanaryLossRun(lost)
		for _, provider := range rows {
			provider.slots.epoch, provider.slots.oldEpoch = latestEpoch, oldEpoch
		}
		return rows
	}
	fellBack := decideTestCanary(testCanaryPair(), testCanaryLossRun(20), settings)
	// an ungated release compares against the held-out latest: dropped
	dropped := decideTestCanaryAfter(fellBack, testCanaryPairOf(testCanaryNewerEpoch, testCanaryLatestEpoch),
		rowsOf(testCanaryNewerEpoch, testCanaryLatestEpoch, 0), settings)
	if dropped.DroppedLatch == nil {
		t.Fatal("fixture: the latch was not dropped")
	}
	// the next ungated release moves the old database past the held-out
	// latest, but the baseline chain still runs through it
	moved := decideTestCanaryAfter(dropped, testCanaryPairOf(newestEpoch, testCanaryNewerEpoch),
		rowsOf(newestEpoch, testCanaryNewerEpoch, 0), settings)
	if moved.DroppedLatch == nil || *moved.DroppedLatch != *fellBack.Latch || !moved.FallbackDropped {
		t.Fatalf("a pair past the held-out latest: dropped=%+v fallback_dropped=%t", moved.DroppedLatch, moved.FallbackDropped)
	}
	// a latest verified against the latch's old database ends it
	verified := decideTestCanaryAfter(moved, testCanaryPairOf(newestEpoch, testCanaryOldEpoch),
		rowsOf(newestEpoch, testCanaryOldEpoch, 0), settings)
	if verified.State != ArindbCanaryStateLatest || verified.DroppedLatch != nil || verified.FallbackDropped {
		t.Fatalf("verified against the old database: state=%s dropped=%+v fallback_dropped=%t", verified.State, verified.DroppedLatch, verified.FallbackDropped)
	}
	// so does a latch on such a pair, which protects it anew
	relatched := decideTestCanaryAfter(moved, testCanaryPairOf(newestEpoch, testCanaryOldEpoch),
		rowsOf(newestEpoch, testCanaryOldEpoch, 20), settings)
	if relatched.State != ArindbCanaryStateFallback || relatched.DroppedLatch != nil || relatched.FallbackDropped {
		t.Fatalf("latched anew on the old database: state=%s dropped=%+v fallback_dropped=%t", relatched.State, relatched.DroppedLatch, relatched.FallbackDropped)
	}
	// a latch on another pair keeps it: its baseline descends from the
	// rejected latest
	downstream := decideTestCanaryAfter(moved, testCanaryPairOf(newestEpoch, testCanaryNewerEpoch),
		rowsOf(newestEpoch, testCanaryNewerEpoch, 20), settings)
	if downstream.Latch == nil || downstream.DroppedLatch == nil || !downstream.FallbackDropped {
		t.Fatalf("a latch downstream of the rejected latest: latch=%+v dropped=%+v", downstream.Latch, downstream.DroppedLatch)
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

// Below paired coverage the indexer serves a database the rows carry: the old
// if it covers enough, else the latest if it does. It recovers to latest once
// the pair is covered.
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
		{name: "a stuck host below coverage stays warming on the old", pair: testCanaryPair(),
			rows:  append(testCanaryRowsOf(97, testCanaryLatestEpoch, testCanaryOldEpoch), testCanaryRowsOf(3, testCanaryOldEpoch, testCanaryOlderEpoch)...),
			state: ArindbCanaryStateWarming, serving: testCanaryOldEpoch},
		{name: "recovery to latest at coverage", pair: testCanaryPair(),
			rows:  append(testCanaryRowsOf(98, testCanaryLatestEpoch, testCanaryOldEpoch), testCanaryRowsOf(2, testCanaryOldEpoch, testCanaryOlderEpoch)...),
			state: ArindbCanaryStateLatest, serving: testCanaryLatestEpoch},
	} {
		decision := decideTestCanary(c.pair, c.rows, defaultArindbCanarySettings())
		if decision.State != c.state || decision.ServingEpoch != c.serving {
			t.Errorf("%s: state=%s serving=%d coverage=%+v, want %s %d", c.name, decision.State, decision.ServingEpoch, decision.Coverage, c.state, c.serving)
		}
	}
}

// When no database is carried by min_coverage of the cohort, warming serves
// each row's latest slot, as before the canary, rather than a database most
// rows lack, whose unavailable facts would make them non-Quality. A rollup
// that has not refreshed since the migration carries neither.
func TestArindbCanaryWarmingServesEachRowsLatestSlotWhenNoDatabaseIsCarried(t *testing.T) {
	for _, c := range []struct {
		name string
		rows []*arindbCanaryProvider
	}{
		{name: "neither database on any row", rows: testCanaryRowsOf(100, 0, 0)},
		{name: "the latest on more rows", rows: append(testCanaryRowsOf(60, testCanaryLatestEpoch, 0), testCanaryRowsOf(40, testCanaryOldEpoch, 0)...)},
		{name: "the old on more rows", rows: append(testCanaryRowsOf(40, testCanaryLatestEpoch, 0), testCanaryRowsOf(60, testCanaryOldEpoch, 0)...)},
		{name: "a tie", rows: append(testCanaryRowsOf(50, testCanaryLatestEpoch, 0), testCanaryRowsOf(50, testCanaryOldEpoch, 0)...)},
	} {
		decision := decideTestCanary(testCanaryPair(), c.rows, defaultArindbCanarySettings())
		if decision.State != ArindbCanaryStateWarming || decision.ServingEpoch != 0 || decision.ServingBasis != ArindbCanaryBasisUncovered {
			t.Errorf("%s: state=%s serving=%d basis=%s coverage=%+v, want warming on each row's latest slot",
				c.name, decision.State, decision.ServingEpoch, decision.ServingBasis, decision.Coverage)
		}
		// every row keeps its own latest slot's Quality
		filter := testCanaryFilter(nil)
		slots := map[server.Id]arindbRollupSlots{}
		for _, provider := range c.rows {
			slots[provider.clientId] = provider.slots
		}
		applyArindbServingEpoch(&filter, testCanarySlotIds(slots), slots, decision.ServingEpoch)
		if len(filter.arinNonQuality) != 0 {
			t.Errorf("%s: %d clean rows became non-Quality", c.name, len(filter.arinNonQuality))
		}
	}
}

// Warming keeps the previous decision's database while the rows still carry
// it, so coverage hovering near min_coverage cannot switch the fleet between
// the databases, and a Taskworker restarted onto the next release keeps the
// database a fallback served while Connect has not restarted.
func TestArindbCanaryWarmingKeepsThePreviousChoiceWhileTheRowsCarryIt(t *testing.T) {
	settings := defaultArindbCanarySettings()
	pair := testCanaryPair()
	passed := decideTestCanary(pair, testCanaryProviders(100, "aa", true, testCanaryClean, testCanaryClean), settings)
	if passed.State != ArindbCanaryStateLatest {
		t.Fatalf("fixture state %s", passed.State)
	}
	// paired coverage dips to 96 with both databases still at 98
	hovering := append(testCanaryRowsOf(96, testCanaryLatestEpoch, testCanaryOldEpoch), testCanaryRowsOf(2, testCanaryLatestEpoch, 0)...)
	hovering = append(hovering, testCanaryRowsOf(2, testCanaryOldEpoch, 0)...)
	if decision := decideTestCanaryAfter(passed, pair, hovering, settings); decision.State != ArindbCanaryStateWarming ||
		decision.ServingEpoch != testCanaryLatestEpoch || decision.ServingBasis != ArindbCanaryBasisPrevious {
		t.Fatalf("hovering coverage: state=%s serving=%d basis=%s, want warming on the previous latest", decision.State, decision.ServingEpoch, decision.ServingBasis)
	}

	// release N fell back from the old database to the older one
	lagging := testCanaryProviders(100, "aa", true, testCanaryNonQuality, testCanaryClean)
	for _, provider := range lagging {
		provider.slots.epoch, provider.slots.oldEpoch = testCanaryOldEpoch, testCanaryOlderEpoch
	}
	fellBack := decideTestCanary(testCanaryPairOf(testCanaryOldEpoch, testCanaryOlderEpoch), lagging, settings)
	if fellBack.State != ArindbCanaryStateFallback || fellBack.ServingEpoch != testCanaryOlderEpoch {
		t.Fatalf("fixture state %s serving %d", fellBack.State, fellBack.ServingEpoch)
	}
	// the Taskworker restarts onto release N+1, whose old database is the one
	// that fell back, while Connect still writes release N's slots
	decision := decideTestCanaryAfter(fellBack, pair, lagging, settings)
	if decision.State != ArindbCanaryStateWarming || decision.ServingEpoch != testCanaryOlderEpoch || decision.ServingBasis != ArindbCanaryBasisPrevious {
		t.Fatalf("after the next release: state=%s serving=%d basis=%s, want the database the fallback served", decision.State, decision.ServingEpoch, decision.ServingBasis)
	}
	// once no row carries it, warming serves the old
	uncarried := testCanaryRowsOf(100, testCanaryOldEpoch, 0)
	if decision := decideTestCanaryAfter(decision, pair, uncarried, settings); decision.ServingEpoch != testCanaryOldEpoch || decision.ServingBasis != ArindbCanaryBasisOld {
		t.Fatalf("once no row carries it: serving=%d basis=%s, want the old", decision.ServingEpoch, decision.ServingBasis)
	}
}

// A warming comparison whose paired sample reaches min_paired_sample and
// trips blocks the latest: warming serves the old while the providers it lacks
// are within the loss the breaker tolerates, else each row's latest slot, and
// says that it tripped. A smaller sample cannot block; warming then serves the
// latest unverified, and says so.
func TestArindbCanaryWarmingNeverServesALatestItsPairedSampleTrips(t *testing.T) {
	// tripping of the paired providers lose Quality under the latest
	rows := func(paired int, tripping int, latestOnly int, oldOnly int) []*arindbCanaryProvider {
		providers := testCanaryProviders(tripping, "aa", true, testCanaryNonQuality, testCanaryClean)
		providers = append(providers, testCanaryProviders(paired-tripping, "aa", true, testCanaryClean, testCanaryClean)...)
		providers = append(providers, testCanaryRowsOf(latestOnly, testCanaryLatestEpoch, 0)...)
		return append(providers, testCanaryRowsOf(oldOnly, testCanaryOldEpoch, 0)...)
	}
	settings := defaultArindbCanarySettings()
	for _, c := range []struct {
		name         string
		rows         []*arindbCanaryProvider
		serving      int64
		basis        string
		tripped      bool
		servesLatest bool
	}{
		{name: "the old within the tolerated loss", rows: rows(92, 20, 7, 1), serving: testCanaryOldEpoch, basis: ArindbCanaryBasisLatestBlocked, tripped: true},
		{name: "the old short of it", rows: rows(60, 20, 39, 1), serving: 0, basis: ArindbCanaryBasisUncovered, tripped: true},
		{name: "a sample below the floor", rows: rows(40, 20, 59, 1), serving: testCanaryLatestEpoch, basis: ArindbCanaryBasisLatest, servesLatest: true},
	} {
		decision := decideTestCanary(testCanaryPair(), c.rows, settings)
		if decision.State != ArindbCanaryStateWarming || !decision.Buckets[RankModeQuality].Tripped {
			t.Fatalf("%s: fixture state=%s quality=%+v", c.name, decision.State, decision.Buckets[RankModeQuality])
		}
		if decision.ServingEpoch != c.serving || decision.ServingBasis != c.basis || decision.WarmingTripped != c.tripped || decision.WarmingServesLatest != c.servesLatest {
			t.Errorf("%s: serving=%d basis=%s warming_tripped=%t serves_latest=%t, want %d %s %t %t", c.name,
				decision.ServingEpoch, decision.ServingBasis, decision.WarmingTripped, decision.WarmingServesLatest, c.serving, c.basis, c.tripped, c.servesLatest)
		}
	}
	// a latest the previous decision served is not kept once its sample trips
	passed := decideTestCanary(testCanaryPair(), testCanaryProviders(100, "aa", true, testCanaryClean, testCanaryClean), settings)
	if decision := decideTestCanaryAfter(passed, testCanaryPair(), rows(92, 20, 7, 1), settings); decision.ServingEpoch != testCanaryOldEpoch || decision.ServingBasis != ArindbCanaryBasisLatestBlocked {
		t.Fatalf("after a passing decision: serving=%d basis=%s, want the old", decision.ServingEpoch, decision.ServingBasis)
	}
}

// 100 paired providers in one country, of which lost leave Quality under the
// latest database.
func testCanaryLossRun(lost int) []*arindbCanaryProvider {
	return append(
		testCanaryProviders(lost, "aa", true, testCanaryNonQuality, testCanaryClean),
		testCanaryProviders(100-lost, "aa", true, testCanaryClean, testCanaryClean)...)
}

// A fallback latches on its pair: a loss alternating around the threshold no
// longer switches the fleet every publication. It releases only after
// release_passing_runs consecutive comparisons pass with release_margin, and a
// comparison inside the threshold but not the margin starts the count again.
func TestArindbCanaryFallbackLatchesUntilTheComparisonPassesWithMargin(t *testing.T) {
	settings := defaultArindbCanarySettings()
	pair := testCanaryPair()
	decision := decideTestCanary(pair, testCanaryLossRun(11), settings)
	if decision.State != ArindbCanaryStateFallback || decision.ServingBasis != ArindbCanaryBasisTripped || decision.Latch == nil ||
		*decision.Latch != (ArindbCanaryLatch{LatestEpoch: testCanaryLatestEpoch, OldEpoch: testCanaryOldEpoch, Since: decision.DecidedAt}) {
		t.Fatalf("trip: state=%s basis=%s latch=%+v", decision.State, decision.ServingBasis, decision.Latch)
	}
	since := decision.DecidedAt
	run := func(lost int, state string, basis string, passingRuns int) {
		t.Helper()
		decision = decideTestCanaryAfter(decision, pair, testCanaryLossRun(lost), settings)
		if decision.State != state || decision.ServingBasis != basis {
			t.Fatalf("loss %d%%: state=%s basis=%s, want %s %s", lost, decision.State, decision.ServingBasis, state, basis)
		}
		if state == ArindbCanaryStateFallback && (decision.Latch == nil || decision.Latch.PassingRuns != passingRuns || !decision.Latch.Since.Equal(since)) {
			t.Fatalf("loss %d%%: latch %+v, want %d passing runs since %s", lost, decision.Latch, passingRuns, since)
		}
		if state == ArindbCanaryStateLatest && decision.Latch != nil {
			t.Fatalf("loss %d%%: released with latch %+v", lost, decision.Latch)
		}
	}
	// alternating around the threshold stays on the old database
	run(9, ArindbCanaryStateFallback, ArindbCanaryBasisLatched, 0)
	run(11, ArindbCanaryStateFallback, ArindbCanaryBasisLatched, 0)
	run(9, ArindbCanaryStateFallback, ArindbCanaryBasisLatched, 0)
	// within the margin it counts, and a run outside it starts again
	for passing := 1; passing < settings.ReleasePassingRuns; passing += 1 {
		run(5, ArindbCanaryStateFallback, ArindbCanaryBasisLatched, passing)
	}
	run(9, ArindbCanaryStateFallback, ArindbCanaryBasisLatched, 0)
	for passing := 1; passing < settings.ReleasePassingRuns; passing += 1 {
		run(8, ArindbCanaryStateFallback, ArindbCanaryBasisLatched, passing)
	}
	run(8, ArindbCanaryStateLatest, ArindbCanaryBasisReleased, 0)
	// released, the next run decides afresh
	run(9, ArindbCanaryStateLatest, ArindbCanaryBasisPassed, 0)
}

// A latch belongs to its pair of databases: the next release decides afresh,
// and warming carries the latch unchanged without serving the latest.
func TestArindbCanaryLatchIsPerPairAndHoldsThroughWarming(t *testing.T) {
	settings := defaultArindbCanarySettings()
	fellBack := decideTestCanary(testCanaryPair(), testCanaryLossRun(11), settings)
	// coverage dips below min_coverage: the latch holds, the latest is blocked
	dipped := append(testCanaryLossRun(0)[:95], testCanaryRowsOf(5, testCanaryLatestEpoch, 0)...)
	warming := decideTestCanaryAfter(fellBack, testCanaryPair(), dipped, settings)
	if warming.State != ArindbCanaryStateWarming || warming.ServingEpoch != testCanaryOldEpoch || warming.Latch == nil || *warming.Latch != *fellBack.Latch {
		t.Fatalf("warming after a fallback: state=%s serving=%d latch=%+v, want the old with the latch", warming.State, warming.ServingEpoch, warming.Latch)
	}
	// the next release's pair is compared without the latch
	next := testCanaryLossRun(9)
	for _, provider := range next {
		provider.slots.epoch, provider.slots.oldEpoch = testCanaryNewerEpoch, testCanaryLatestEpoch
	}
	if decision := decideTestCanaryAfter(warming, testCanaryPairOf(testCanaryNewerEpoch, testCanaryLatestEpoch), next, settings); decision.State != ArindbCanaryStateLatest || decision.Latch != nil {
		t.Fatalf("the next pair: state=%s latch=%+v, want latest without a latch", decision.State, decision.Latch)
	}
}

// The operator accepts a latest database by its build epoch in provider.yml:
// it is served although it trips, and its latch is released. Another epoch's
// key changes nothing.
func TestArindbCanaryOperatorAcceptsTheLatestDatabase(t *testing.T) {
	settings := defaultArindbCanarySettings()
	fellBack := decideTestCanary(testCanaryPair(), testCanaryLossRun(20), settings)
	if fellBack.State != ArindbCanaryStateFallback || fellBack.Accepted {
		t.Fatalf("fixture state %s accepted %t", fellBack.State, fellBack.Accepted)
	}
	other := settings
	other.AcceptLatestBuildEpoch = testCanaryNewerEpoch
	if decision := decideTestCanaryAfter(fellBack, testCanaryPair(), testCanaryLossRun(20), other); decision.State != ArindbCanaryStateFallback || decision.Accepted {
		t.Fatalf("another epoch's key: state=%s accepted=%t", decision.State, decision.Accepted)
	}
	accept := settings
	accept.AcceptLatestBuildEpoch = testCanaryLatestEpoch
	for name, previous := range map[string]*ArindbCanaryDecision{"a fresh trip": nil, "a latched fallback": fellBack} {
		decision := decideTestCanaryAfter(previous, testCanaryPair(), testCanaryLossRun(20), accept)
		if decision.State != ArindbCanaryStateLatest || decision.ServingEpoch != testCanaryLatestEpoch || decision.ServingBasis != ArindbCanaryBasisAccepted ||
			!decision.Accepted || decision.Latch != nil || !decision.Buckets[RankModeQuality].Tripped {
			t.Errorf("%s: state=%s serving=%d basis=%s accepted=%t latch=%+v", name, decision.State, decision.ServingEpoch, decision.ServingBasis, decision.Accepted, decision.Latch)
		}
	}
}

// An empty public cohort compares nothing, so it never verifies the latest:
// the canary warms, carrying the previous choice and its latch, or each row's
// latest slot without a previous decision.
func TestArindbCanaryEmptyCohortCarriesThePreviousChoice(t *testing.T) {
	settings := defaultArindbCanarySettings()
	if decision := decideTestCanary(testCanaryPair(), nil, settings); decision.State != ArindbCanaryStateWarming ||
		decision.ServingEpoch != 0 || decision.ServingBasis != ArindbCanaryBasisEmptyCohort || decision.WarmingServesLatest {
		t.Fatalf("without a previous decision: state=%s serving=%d basis=%s", decision.State, decision.ServingEpoch, decision.ServingBasis)
	}
	fellBack := decideTestCanary(testCanaryPair(), testCanaryLossRun(20), settings)
	if decision := decideTestCanaryAfter(fellBack, testCanaryPair(), nil, settings); decision.State != ArindbCanaryStateWarming ||
		decision.ServingEpoch != testCanaryOldEpoch || decision.Latch == nil || *decision.Latch != *fellBack.Latch {
		t.Fatalf("after a fallback: state=%s serving=%d latch=%+v", decision.State, decision.ServingEpoch, decision.Latch)
	}
	passed := decideTestCanary(testCanaryPair(), testCanaryLossRun(0), settings)
	if decision := decideTestCanaryAfter(passed, testCanaryPair(), nil, settings); decision.ServingEpoch != testCanaryLatestEpoch || !decision.WarmingServesLatest {
		t.Fatalf("after a passing decision: serving=%d serves_latest=%t", decision.ServingEpoch, decision.WarmingServesLatest)
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

func testCanarySlotIds(slots map[server.Id]arindbRollupSlots) []server.Id {
	return slices.Collect(maps.Keys(slots))
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
	applyArindbServingEpoch(&filter, testCanarySlotIds(slots), slots, testCanaryOldEpoch)
	if !filter.arinNonQuality[betterUnderLatest] || filter.arinNonQuality[worseUnderLatest] || filter.arindbServingEpoch != testCanaryOldEpoch {
		t.Fatalf("fallback decided providers separately: better=%t worse=%t", filter.arinNonQuality[betterUnderLatest], filter.arinNonQuality[worseUnderLatest])
	}
	filter = testCanaryFilter(slots)
	applyArindbServingEpoch(&filter, testCanarySlotIds(slots), slots, testCanaryLatestEpoch)
	if filter.arinNonQuality[betterUnderLatest] || !filter.arinNonQuality[worseUnderLatest] {
		t.Fatal("latest did not serve the latest classification")
	}
}

// A client of the publication the slot read did not return has no facts under
// S, so it is non-Quality rather than keeping its latest slot's Quality.
func TestArindbCanaryClientMissingFromTheSlotReadIsNotQuality(t *testing.T) {
	read, missed := server.NewId(), server.NewId()
	slots := map[server.Id]arindbRollupSlots{read: {epoch: testCanaryLatestEpoch, oldEpoch: testCanaryOldEpoch}}
	filter := testCanaryFilter(slots)
	applyArindbServingEpoch(&filter, []server.Id{read, missed}, slots, testCanaryOldEpoch)
	if filter.arinNonQuality[read] || !filter.arinNonQuality[missed] {
		t.Fatalf("read=%t missed=%t, want only the missed client non-Quality", filter.arinNonQuality[read], filter.arinNonQuality[missed])
	}
	// each row's latest slot, S = 0, reads no slots at all
	filter = testCanaryFilter(slots)
	applyArindbServingEpoch(&filter, []server.Id{read, missed}, slots, 0)
	if len(filter.arinNonQuality) != 0 {
		t.Fatalf("S = 0 changed Quality: %v", filter.arinNonQuality)
	}
}

// The publication reads the rollup's slots for its cohort only when it can
// compare: single serves each row's latest slot and skips the read.
func TestArindbCanaryPublicationSkipsTheSlotReadWhenSingle(t *testing.T) {
	useTestArindbCanaryDecisionStore(t)
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("{}"))
	defer pop()
	var reads [][]server.Id
	previousLoad := arindbCanaryLoadSlots
	arindbCanaryLoadSlots = func(ctx context.Context, clientIds []server.Id) map[server.Id]arindbRollupSlots {
		reads = append(reads, clientIds)
		return map[server.Id]arindbRollupSlots{}
	}
	defer func() { arindbCanaryLoadSlots = previousLoad }()
	previousPair := arindbCanaryPairIdentities
	defer func() { arindbCanaryPairIdentities = previousPair }()

	country := "aa"
	rows := []clientScoreSourceRow{{
		score:  &ClientScore{ClientId: server.NewId(), LookbackIndex: 1, IndependentReliabilityWeight: 1},
		egress: &clientScoreEgress{publishedCountryCode: &country},
	}}
	for _, c := range []struct {
		name  string
		pair  arindbCanaryPair
		reads int
	}{
		{name: "single", pair: func() arindbCanaryPair {
			p := testCanaryPair()
			p.old = server.ArinDatabaseIdentity{State: server.ArinDatabaseMissing}
			return p
		}(), reads: 0},
		{name: "a comparable pair", pair: testCanaryPair(), reads: 1},
	} {
		reads = nil
		arindbCanaryPairIdentities = func() (server.ArinDatabaseIdentity, server.ArinDatabaseIdentity, error) {
			return c.pair.latest, c.pair.old, c.pair.err
		}
		filter := testCanaryFilter(nil)
		if _, err := decideArindbCanaryForPublication(t.Context(), testCanaryClock, rows, &filter, DefaultEgressIndexSettings(), false, map[int]float64{}); err != nil {
			t.Fatal(err)
		}
		if len(reads) != c.reads || (c.reads == 1 && (len(reads[0]) != 1 || reads[0][0] != rows[0].score.ClientId)) {
			t.Errorf("%s: slot reads %v, want %d of the cohort", c.name, reads, c.reads)
		}
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
	applyArindbServingEpoch(&filter, testCanarySlotIds(slots), slots, testCanaryOldEpoch)
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
	// ten runs over four hours, enough for Quality (egress_native_min_runs.go)
	filter.healthCounts[healthy] = ProviderEgressHealthCounts{MeasuredAt: testCanaryClock, FirstMeasuredAt: testCanaryClock.Add(-4 * time.Hour), OKCount: 10, Total: 10}
	filter.healthCounts[failing] = ProviderEgressHealthCounts{MeasuredAt: testCanaryClock, FirstMeasuredAt: testCanaryClock.Add(-4 * time.Hour), OKCount: 1, Total: 10}
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
		"warming old serving another": func(d map[string]any) {
			d["state"], d["serving_basis"], d["serving_epoch"] = ArindbCanaryStateWarming, ArindbCanaryBasisOld, testCanaryLatestEpoch
		},
		"uncovered warming serving a database": func(d map[string]any) {
			d["state"], d["serving_basis"] = ArindbCanaryStateWarming, ArindbCanaryBasisUncovered
		},
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
	// warming may serve each row's latest slot, or the previous decision's
	// database, which need be neither of this pair
	for name, edit := range map[string]func(map[string]any){
		"uncovered warming": func(d map[string]any) {
			d["state"], d["serving_basis"], d["serving_epoch"] = ArindbCanaryStateWarming, ArindbCanaryBasisUncovered, 0
		},
		"warming on the previous choice": func(d map[string]any) {
			d["state"], d["serving_basis"], d["serving_epoch"] = ArindbCanaryStateWarming, ArindbCanaryBasisPrevious, testCanaryOlderEpoch
		},
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
		if _, err := parseArindbCanaryDecision(data); err != nil {
			t.Errorf("%s was refused: %s", name, err)
		}
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
	if defaults != (ArindbCanarySettings{
		BucketLossThreshold: 0.10, TripOnEmptiedCountry: true, MinCoverage: 0.98, MinPairedSample: 50,
		MinEmptiedCountrySupply: 3, ReleasePassingRuns: 6, ReleaseMargin: 0.02,
	}) {
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
		{config: "arindb_canary: {bucket_loss_threshold: 0.2, trip_on_emptied_country: false, min_coverage: 0.9, min_paired_sample: 10, " +
			"min_emptied_country_supply: 5, release_passing_runs: 2, release_margin: 0.05, accept_latest_build_epoch: 1700086400}",
			want: ArindbCanarySettings{
				BucketLossThreshold: 0.2, TripOnEmptiedCountry: false, MinCoverage: 0.9, MinPairedSample: 10,
				MinEmptiedCountrySupply: 5, ReleasePassingRuns: 2, ReleaseMargin: 0.05, AcceptLatestBuildEpoch: 1700086400,
			}},
		{config: "arindb_canary: {min_coverage: 0.5}", want: func() ArindbCanarySettings {
			settings := defaults
			settings.MinCoverage = 0.5
			return settings
		}()},
		{config: "arindb_canary: {min_paired_sample: 0}", want: defaults},
		{config: "arindb_canary: {min_emptied_country_supply: 0, release_passing_runs: 0, release_margin: -0.01, accept_latest_build_epoch: -1}", want: defaults},
		{config: "arindb_canary: {release_margin: 1.5}", want: defaults},
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
