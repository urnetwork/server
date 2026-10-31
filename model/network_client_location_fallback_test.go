// Pins the FindProviders2 location fallback: which targets climb, to which
// level, under which of the request's filters, how each provider is marked,
// what the counters say of the final answer, and that an answer that did not
// climb is unchanged on the wire.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Adds one synthetic place to a directory, its row naming its parents as a
// location row does: a country names itself, a region itself and its country.
func testing_addFallbackPlace(
	entries map[server.Id]*locationDirectoryEntry,
	locationType LocationType,
	countryCode string,
	regionLocationId *server.Id,
	countryLocationId *server.Id,
) server.Id {
	locationId := server.NewId()
	entry := &locationDirectoryEntry{
		Name:              fmt.Sprintf("Synthetic %s %s", locationType, locationId),
		CountryCode:       countryCode,
		LocationType:      locationType,
		RegionLocationId:  regionLocationId,
		CountryLocationId: countryLocationId,
	}
	switch locationType {
	case LocationTypeCountry:
		entry.CountryLocationId = &locationId
	case LocationTypeRegion:
		entry.RegionLocationId = &locationId
	}
	entries[locationId] = entry
	return locationId
}

// The synthetic places of the fallback tests. Each target's pages are written
// as an export writes them, an empty page for a target without an admitted
// provider, and the directory is published with each place's parents:
//
//	country us: aV4, d, eV4, eDual
//	  region a: aV4          city a1: none   city a2: aV4
//	  region b: none         city b1: none
//	  region d: d            city d1: d
//	  region e: eV4, eDual   city e1: none   city e2: eV4, eDual
//	country ca: none
//	  region c: none         city c1: none
//	country nz: fQuality (Quality pages), fSpeed (Speed pages)
//	  region f: as nz        city f1: none
//
// Every provider is an online member except fQuality and fSpeed, natives of
// one mode each.
type testingFallbackPlaces struct {
	usId, regionAId, cityA1Id, cityA2Id, regionBId, cityB1Id, regionDId, cityD1Id, regionEId, cityE1Id, cityE2Id server.Id
	caId, regionCId, cityC1Id                                                                                    server.Id
	nzId, regionFId, cityF1Id                                                                                    server.Id

	aV4, d, eV4, eDual, fQuality, fSpeed *ClientScore
}

// Publishes the places and their pages, under the subscriber policy the
// Quality natives are validated by.
func testing_newFallbackPlaces(ctx context.Context, t testing.TB) *testingFallbackPlaces {
	t.Helper()
	enableSubscriberQualityPolicy(t)
	self := &testingFallbackPlaces{}
	entries := map[server.Id]*locationDirectoryEntry{}
	place := func(locationType LocationType, countryCode string, regionLocationId *server.Id, countryLocationId *server.Id) server.Id {
		return testing_addFallbackPlace(entries, locationType, countryCode, regionLocationId, countryLocationId)
	}
	self.usId = place(LocationTypeCountry, "us", nil, nil)
	self.regionAId = place(LocationTypeRegion, "us", nil, &self.usId)
	self.cityA1Id = place(LocationTypeCity, "us", &self.regionAId, &self.usId)
	self.cityA2Id = place(LocationTypeCity, "us", &self.regionAId, &self.usId)
	self.regionBId = place(LocationTypeRegion, "us", nil, &self.usId)
	self.cityB1Id = place(LocationTypeCity, "us", &self.regionBId, &self.usId)
	self.regionDId = place(LocationTypeRegion, "us", nil, &self.usId)
	self.cityD1Id = place(LocationTypeCity, "us", &self.regionDId, &self.usId)
	self.regionEId = place(LocationTypeRegion, "us", nil, &self.usId)
	self.cityE1Id = place(LocationTypeCity, "us", &self.regionEId, &self.usId)
	self.cityE2Id = place(LocationTypeCity, "us", &self.regionEId, &self.usId)
	self.caId = place(LocationTypeCountry, "ca", nil, nil)
	self.regionCId = place(LocationTypeRegion, "ca", nil, &self.caId)
	self.cityC1Id = place(LocationTypeCity, "ca", &self.regionCId, &self.caId)
	self.nzId = place(LocationTypeCountry, "nz", nil, nil)
	self.regionFId = place(LocationTypeRegion, "nz", nil, &self.nzId)
	self.cityF1Id = place(LocationTypeCity, "nz", &self.regionFId, &self.nzId)
	setLocationDirectoryCache(ctx, entries, time.Minute)
	loadLocationDirectory()

	self.aV4 = onlineBackfillScore(true, 1)
	self.d = onlineBackfillScore(true, 1)
	self.eV4 = onlineBackfillScore(true, 1)
	self.eDual = onlineBackfillScore(true, 1)
	self.eDual.IpFamilies = ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
	self.fQuality = onlineBackfillScore(false, 1)
	self.fQuality.PassesMinimums = map[string]bool{RankModeQuality: true}
	self.fSpeed = onlineBackfillScore(false, 1)
	self.fSpeed.PassesMinimums = map[string]bool{RankModeSpeed: true}

	write := func(locationId server.Id, scores ...*ClientScore) {
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			writeOnlineBackfillSample(ctx, t, locationId, mode, false, scores)
		}
	}
	write(self.usId, self.aV4, self.d, self.eV4, self.eDual)
	write(self.regionAId, self.aV4)
	write(self.cityA1Id)
	write(self.cityA2Id, self.aV4)
	write(self.regionBId)
	write(self.cityB1Id)
	write(self.regionDId, self.d)
	write(self.cityD1Id, self.d)
	write(self.regionEId, self.eV4, self.eDual)
	write(self.cityE1Id)
	write(self.cityE2Id, self.eV4, self.eDual)
	write(self.caId)
	write(self.regionCId)
	write(self.cityC1Id)
	for _, locationId := range []server.Id{self.nzId, self.regionFId} {
		writeOnlineBackfillSample(ctx, t, locationId, RankModeQuality, false, []*ClientScore{self.fQuality})
		writeOnlineBackfillSample(ctx, t, locationId, RankModeSpeed, false, []*ClientScore{self.fSpeed})
	}
	write(self.cityF1Id)
	return self
}

// The answer to one request from a caller in callerNetworkId.
func testing_findFallback(ctx context.Context, t testing.TB, args *FindProviders2Args, callerNetworkId server.Id) []*FindProvidersProvider {
	t.Helper()
	clientSession := testingCreateProviderSearchSession(
		ctx,
		session.NewByJwt(callerNetworkId, server.NewId(), "location-fallback-test", false, false),
	)
	result, err := FindProviders2(args, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result.Providers
}

// A default request for one location, as a client before the strict flag
// sends it.
func testing_fallbackArgs(locationId server.Id) *FindProviders2Args {
	return &FindProviders2Args{
		Specs: []*ProviderSpec{{LocationId: &locationId}},
		Count: 10,
	}
}

// Fails unless the answer is exactly the providers, in any order, each with
// the match.
func testing_assertFallbackAnswer(t testing.TB, providers []*FindProvidersProvider, match LocationType, scores ...*ClientScore) {
	t.Helper()
	got := egressTestIds(providers)
	want := []server.Id{}
	for _, score := range scores {
		want = append(want, score.ClientId)
	}
	slices.SortFunc(got, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	slices.SortFunc(want, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	if !slices.Equal(got, want) {
		t.Fatalf("answered %d providers, want %d: the fallback read the wrong level or lost a filter", len(got), len(want))
	}
	for _, provider := range providers {
		if provider.Match != match {
			t.Fatalf("provider match=%q, want %q", provider.Match, match)
		}
	}
}

// The fallback counter's series for a target kind and level.
func testing_fallbackCount(t testing.TB, targetKind string, level string) float64 {
	t.Helper()
	return selectionMetricCount(t, "urnetwork_findproviders2_location_fallback_total", map[string]string{"target_kind": targetKind, "level": level})
}

// An empty city answers from its region, with the region's providers only,
// each marked region.
func TestFindProviders2LocationFallbackAnswersAnEmptyCityFromItsRegion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		before := testing_fallbackCount(t, LocationTypeCity, LocationTypeRegion)
		for _, rankMode := range []RankMode{"", RankModeQuality, RankModeSpeed, RankModeOnline} {
			args := testing_fallbackArgs(places.cityA1Id)
			args.RankMode = rankMode
			testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, args, server.NewId()), LocationTypeRegion, places.aV4)
		}
		if after := testing_fallbackCount(t, LocationTypeCity, LocationTypeRegion); after != before+4 {
			t.Fatalf("city to region fallback count=%g, want %g", after, before+4)
		}
	})
}

// An empty region answers from its country, each provider marked country.
func TestFindProviders2LocationFallbackAnswersAnEmptyRegionFromItsCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		before := testing_fallbackCount(t, LocationTypeRegion, LocationTypeCountry)
		providers := testing_findFallback(ctx, t, testing_fallbackArgs(places.regionBId), server.NewId())
		testing_assertFallbackAnswer(t, providers, LocationTypeCountry, places.aV4, places.d, places.eV4, places.eDual)
		if after := testing_fallbackCount(t, LocationTypeRegion, LocationTypeCountry); after != before+1 {
			t.Fatalf("region to country fallback count=%g, want %g", after, before+1)
		}
	})
}

// An empty city in an empty region answers from its country.
func TestFindProviders2LocationFallbackAnswersAnEmptyCityAndRegionFromTheCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		before := testing_fallbackCount(t, LocationTypeCity, LocationTypeCountry)
		providers := testing_findFallback(ctx, t, testing_fallbackArgs(places.cityB1Id), server.NewId())
		testing_assertFallbackAnswer(t, providers, LocationTypeCountry, places.aV4, places.d, places.eV4, places.eDual)
		if after := testing_fallbackCount(t, LocationTypeCity, LocationTypeCountry); after != before+1 {
			t.Fatalf("city to country fallback count=%g, want %g", after, before+1)
		}
	})
}

// A city or region whose country has no admitted provider either stays empty,
// counted as a fallback that found none. Its empty answer read the country
// last, so it is explained under the country's kind and code, which the
// supply probe judges as it judges an empty country; the selection metric
// keeps the requested kind and the final reason. The fallback does not cross
// into another country's supply.
func TestFindProviders2LocationFallbackLeavesAnEmptyCountryEmpty(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		emptyOutcomes := func(targetKind string, targetCountry string) float64 {
			return selectionMetricSum(t, "urnetwork_findproviders2_empty_outcomes_total", map[string]string{"target_kind": targetKind, "target_country": targetCountry, "reason": "cache_empty"})
		}
		selections := func(targetKind string) float64 {
			return selectionMetricSum(t, "urnetwork_findproviders2_selection_outcomes_total", map[string]string{"target_kind": targetKind, "outcome": "zero", "reason": "cache_empty"})
		}
		cityNone, regionNone := testing_fallbackCount(t, LocationTypeCity, findProviders2LocationFallbackNone), testing_fallbackCount(t, LocationTypeRegion, findProviders2LocationFallbackNone)
		countryEmpty, cityEmpty, regionEmpty := emptyOutcomes(LocationTypeCountry, "ca"), emptyOutcomes(LocationTypeCity, ""), emptyOutcomes(LocationTypeRegion, "ca")
		citySelections, regionSelections := selections(LocationTypeCity), selections(LocationTypeRegion)

		for _, locationId := range []server.Id{places.cityC1Id, places.regionCId} {
			if providers := testing_findFallback(ctx, t, testing_fallbackArgs(locationId), server.NewId()); len(providers) != 0 {
				t.Fatalf("an empty country answered %d providers", len(providers))
			}
		}

		connect.AssertEqual(t, testing_fallbackCount(t, LocationTypeCity, findProviders2LocationFallbackNone), cityNone+1)
		connect.AssertEqual(t, testing_fallbackCount(t, LocationTypeRegion, findProviders2LocationFallbackNone), regionNone+1)
		connect.AssertEqual(t, emptyOutcomes(LocationTypeCountry, "ca"), countryEmpty+2)
		connect.AssertEqual(t, emptyOutcomes(LocationTypeCity, ""), cityEmpty)
		connect.AssertEqual(t, emptyOutcomes(LocationTypeRegion, "ca"), regionEmpty)
		connect.AssertEqual(t, selections(LocationTypeCity), citySelections+1)
		connect.AssertEqual(t, selections(LocationTypeRegion), regionSelections+1)
	})
}

// A climb stops at a level whose admitted providers the request's own
// exclusions removed, though the country above holds others, and its empty
// answer is explained by that level and reason.
func TestFindProviders2LocationFallbackStopsAtAnExcludedLevel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		emptyOutcomes := func(targetKind string, reason string) float64 {
			return selectionMetricSum(t, "urnetwork_findproviders2_empty_outcomes_total", map[string]string{"target_kind": targetKind, "target_country": "", "reason": reason})
		}
		none := testing_fallbackCount(t, LocationTypeCity, findProviders2LocationFallbackNone)
		regionExcluded := emptyOutcomes(LocationTypeRegion, "filtered_destinations")

		// city a1 has none, its region holds only aV4, the country more
		args := testing_fallbackArgs(places.cityA1Id)
		args.ExcludeDestinations = [][]server.Id{{places.aV4.ClientId}}
		if providers := testing_findFallback(ctx, t, args, server.NewId()); len(providers) != 0 {
			t.Fatalf("a climb past an excluded region answered %d providers from the country", len(providers))
		}

		connect.AssertEqual(t, testing_fallbackCount(t, LocationTypeCity, findProviders2LocationFallbackNone), none+1)
		connect.AssertEqual(t, emptyOutcomes(LocationTypeRegion, "filtered_destinations"), regionExcluded+1)
	})
}

// An empty answer is explained by the level it came from only when the
// request's targets lie in one country, whose supply can explain it.
func TestFindProviders2AnsweredTargetKind(t *testing.T) {
	for _, c := range []struct {
		targetKind    string
		fallbackLevel string
		targetCountry string
		want          string
	}{
		{targetKind: LocationTypeCity, want: LocationTypeCity},
		{targetKind: LocationTypeCity, targetCountry: "ca", want: LocationTypeCity},
		{targetKind: LocationTypeCity, fallbackLevel: LocationTypeCountry, targetCountry: "ca", want: LocationTypeCountry},
		{targetKind: LocationTypeCity, fallbackLevel: LocationTypeRegion, targetCountry: "ca", want: LocationTypeRegion},
		{targetKind: LocationTypeRegion, fallbackLevel: LocationTypeCountry, targetCountry: "ca", want: LocationTypeCountry},
		{targetKind: "mixed", fallbackLevel: LocationTypeCountry, targetCountry: "ca", want: LocationTypeCountry},
		// targets in two countries: no one country's supply explains it
		{targetKind: LocationTypeCity, fallbackLevel: LocationTypeCountry, want: LocationTypeCity},
	} {
		observation := &findProviders2SelectionObservation{targetKind: c.targetKind, fallbackLevel: c.fallbackLevel, targetCountry: c.targetCountry}
		if got := observation.answeredTargetKind(); got != c.want {
			t.Fatalf("%+v answered kind %s", c, got)
		}
	}
}

// A city with admitted providers that the request's own exclusions removed
// does not climb: the client already holds them. The region and country above
// each city hold other providers, so a climb would answer them.
func TestFindProviders2LocationFallbackDoesNotClimbPastTheRequestsExclusions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		// the controls: each city answers its own providers, unmarked
		testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, testing_fallbackArgs(places.cityA2Id), server.NewId()), "", places.aV4)
		testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, testing_fallbackArgs(places.cityE2Id), server.NewId()), "", places.eV4, places.eDual)

		before := map[string]float64{}
		for _, level := range []string{LocationTypeRegion, LocationTypeCountry, findProviders2LocationFallbackNone} {
			before[level] = testing_fallbackCount(t, LocationTypeCity, level)
		}
		for _, c := range []struct {
			cityId server.Id
			scores []*ClientScore
		}{
			{cityId: places.cityA2Id, scores: []*ClientScore{places.aV4}},
			{cityId: places.cityE2Id, scores: []*ClientScore{places.eV4, places.eDual}},
		} {
			// the window refill (filtered_destinations), then the client's own
			// removals (filtered_client_ids)
			for _, destinations := range []bool{true, false} {
				args := testing_fallbackArgs(c.cityId)
				for _, score := range c.scores {
					if destinations {
						args.ExcludeDestinations = append(args.ExcludeDestinations, []server.Id{score.ClientId})
					} else {
						args.ExcludeClientIds = append(args.ExcludeClientIds, score.ClientId)
					}
				}
				if providers := testing_findFallback(ctx, t, args, server.NewId()); len(providers) != 0 {
					t.Fatalf("a city whose providers the request excluded answered %d providers from above it", len(providers))
				}
			}
		}
		for level, count := range before {
			if after := testing_fallbackCount(t, LocationTypeCity, level); after != count {
				t.Fatalf("an excluded city counted a %s fallback", level)
			}
		}
	})
}

// strict keeps today's exact answer: an empty city stays empty, explained as
// cache_empty, and a city with providers answers them unmarked.
func TestFindProviders2LocationFallbackStrictKeepsTheExactAnswer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		before := map[string]float64{}
		for _, level := range []string{LocationTypeRegion, LocationTypeCountry, findProviders2LocationFallbackNone} {
			before[level] = testing_fallbackCount(t, LocationTypeCity, level)
		}
		// a strict city's empty answer is the city's own, without a country
		explained := map[string]string{"target_kind": LocationTypeCity, "target_country": "", "reason": "cache_empty"}
		explainedBefore := selectionMetricSum(t, "urnetwork_findproviders2_empty_outcomes_total", explained)
		for _, cityId := range []server.Id{places.cityA1Id, places.cityB1Id, places.cityC1Id} {
			args := testing_fallbackArgs(cityId)
			args.Strict = true
			if providers := testing_findFallback(ctx, t, args, server.NewId()); len(providers) != 0 {
				t.Fatalf("a strict request for an empty city answered %d providers", len(providers))
			}
		}
		args := testing_fallbackArgs(places.cityA2Id)
		args.Strict = true
		testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, args, server.NewId()), "", places.aV4)
		if after := selectionMetricSum(t, "urnetwork_findproviders2_empty_outcomes_total", explained); after != explainedBefore+3 {
			t.Fatalf("strict empty cities explained=%g, want %g", after, explainedBefore+3)
		}
		for level, count := range before {
			if after := testing_fallbackCount(t, LocationTypeCity, level); after != count {
				t.Fatalf("a strict request counted a %s fallback", level)
			}
		}
	})
}

// The request's ip_family filter holds at the fallback level.
func TestFindProviders2LocationFallbackKeepsTheIpFamilyFilter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		for _, c := range []struct {
			ipFamily string
			scores   []*ClientScore
		}{
			{ipFamily: "", scores: []*ClientScore{places.eV4, places.eDual}},
			{ipFamily: "v6-capable", scores: []*ClientScore{places.eDual}},
			{ipFamily: "dualstack", scores: []*ClientScore{places.eDual}},
			{ipFamily: "v4-only", scores: []*ClientScore{places.eV4}},
		} {
			args := testing_fallbackArgs(places.cityE1Id)
			args.IpFamily = c.ipFamily
			providers := testing_findFallback(ctx, t, args, server.NewId())
			testing_assertFallbackAnswer(t, providers, LocationTypeRegion, c.scores...)
			for _, provider := range providers {
				for _, score := range c.scores {
					if provider.ClientId == score.ClientId {
						connect.AssertEqual(t, provider.IpFamily, string(score.IpFamily()))
					}
				}
			}
		}
		// no provider of the family at any level: the climb stops empty
		args := testing_fallbackArgs(places.cityE1Id)
		args.IpFamily = "v6-only"
		if providers := testing_findFallback(ctx, t, args, server.NewId()); len(providers) != 0 {
			t.Fatalf("a v6-only request answered %d providers without v6-only supply", len(providers))
		}
	})
}

// Counts and exclusions hold at the fallback level, and the request's rank
// mode draws it.
func TestFindProviders2LocationFallbackKeepsTheRequestFilters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)

		// a count of one answers one of the region's two providers
		args := testing_fallbackArgs(places.cityE1Id)
		args.Count, args.ForceCount = 1, true
		providers := testing_findFallback(ctx, t, args, server.NewId())
		if len(providers) != 1 || providers[0].Match != LocationTypeRegion || (providers[0].ClientId != places.eV4.ClientId && providers[0].ClientId != places.eDual.ClientId) {
			t.Fatalf("a count of one answered %d providers", len(providers))
		}

		// a provider the request excludes stays out of the level
		args = testing_fallbackArgs(places.cityE1Id)
		args.ExcludeClientIds = []server.Id{places.eV4.ClientId}
		testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, args, server.NewId()), LocationTypeRegion, places.eDual)

		// the rank mode draws the level: each mode answers its own native
		for _, c := range []struct {
			rankMode RankMode
			score    *ClientScore
		}{
			{rankMode: RankModeQuality, score: places.fQuality},
			{rankMode: RankModeSpeed, score: places.fSpeed},
		} {
			args := testing_fallbackArgs(places.cityF1Id)
			args.RankMode, args.Count, args.ForceCount = c.rankMode, 1, true
			providers := testing_findFallback(ctx, t, args, server.NewId())
			testing_assertFallbackAnswer(t, providers, LocationTypeRegion, c.score)
			connect.AssertEqual(t, providers[0].Tier, 0)
		}
	})
}

// A network-only provider at the fallback level is answered only to a caller
// in its own network.
func TestFindProviders2LocationFallbackKeepsNetworkOnlyAccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		entries := map[server.Id]*locationDirectoryEntry{}
		countryId := testing_addFallbackPlace(entries, LocationTypeCountry, "us", nil, nil)
		regionId := testing_addFallbackPlace(entries, LocationTypeRegion, "us", nil, &countryId)
		cityId := testing_addFallbackPlace(entries, LocationTypeCity, "us", &regionId, &countryId)
		setLocationDirectoryCache(ctx, entries, time.Minute)
		loadLocationDirectory()
		public := onlineBackfillScore(true, 1)
		networkOnly := onlineBackfillScore(true, 1)
		networkOnly.NetworkOnly = true
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			writeOnlineBackfillSample(ctx, t, cityId, mode, false, nil)
			writeOnlineBackfillSample(ctx, t, regionId, mode, false, []*ClientScore{public, networkOnly})
			writeOnlineBackfillSample(ctx, t, countryId, mode, false, []*ClientScore{public, networkOnly})
		}
		testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, testing_fallbackArgs(cityId), server.NewId()), LocationTypeRegion, public)
		testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, testing_fallbackArgs(cityId), networkOnly.NetworkId), LocationTypeRegion, public, networkOnly)
	})
}

// A named provider keeps its place ahead of discovery and is answered once,
// unmarked; the fallback's providers follow it, marked.
func TestFindProviders2LocationFallbackAnswersANamedProviderOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		cityId, namedClientId := places.cityA1Id, places.d.ClientId
		providers := testing_findFallback(ctx, t, &FindProviders2Args{
			Specs:    []*ProviderSpec{{ClientId: &namedClientId}, {LocationId: &cityId}},
			Count:    10,
			RankMode: RankModeSpeed,
		}, server.NewId())
		if len(providers) != 2 || providers[0].ClientId != namedClientId || providers[1].ClientId != places.aV4.ClientId {
			t.Fatalf("answered %d providers, want the named provider then the region's one", len(providers))
		}
		connect.AssertEqual(t, providers[0].Match, LocationType(""))
		connect.AssertEqual(t, providers[1].Match, LocationTypeRegion)
	})
}

// The outcome counters describe the final answer: a fallback answer is in a
// nonempty band, is not explained as empty, and its selection outcome is
// nonempty for the city the request named.
func TestFindProviders2LocationFallbackOutcomesDescribeTheFinalAnswer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		outcomes := func(resultCount string) float64 {
			return selectionMetricSum(t, "urnetwork_findproviders2_outcomes_total", map[string]string{"location_kind": "location", "rank_mode": "quality", "result_count": resultCount})
		}
		emptyOutcomes := func() float64 {
			return selectionMetricSum(t, "urnetwork_findproviders2_empty_outcomes_total", map[string]string{"target_kind": LocationTypeCity})
		}
		selections := func(outcome string) float64 {
			return selectionMetricSum(t, "urnetwork_findproviders2_selection_outcomes_total", map[string]string{"target_kind": LocationTypeCity, "rank_mode": "quality", "outcome": outcome})
		}
		// the borrowed-provider histogram observes each answer once, not each
		// level it read
		backfillAnswers := func() uint64 {
			families, err := prometheus.DefaultGatherer.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, family := range families {
				if family.GetName() != "urnetwork_provider_backfill" {
					continue
				}
				for _, metric := range family.Metric {
					for _, pair := range metric.Label {
						if pair.GetName() == "rank_mode" && pair.GetValue() == RankModeQuality {
							return metric.GetHistogram().GetSampleCount()
						}
					}
				}
			}
			return 0
		}
		zero, small, empty, nonempty, zeroSelections, backfill := outcomes("0"), outcomes("1-2"), emptyOutcomes(), selections("nonempty"), selections("zero"), backfillAnswers()

		testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, testing_fallbackArgs(places.cityA1Id), server.NewId()), LocationTypeRegion, places.aV4)

		connect.AssertEqual(t, outcomes("0"), zero)
		connect.AssertEqual(t, outcomes("1-2"), small+1)
		connect.AssertEqual(t, emptyOutcomes(), empty)
		connect.AssertEqual(t, selections("nonempty"), nonempty+1)
		connect.AssertEqual(t, selections("zero"), zeroSelections)
		connect.AssertEqual(t, backfillAnswers(), backfill+1)
	})
}

// FindProvidersProvider as it was before the location fallback, to pin the
// wire of an answer that did not climb.
type testingLegacyFindProvidersProvider struct {
	ClientId                   server.Id         `json:"client_id"`
	EstimatedBytesPerSecond    ByteCount         `json:"estimated_bytes_per_second"`
	HasEstimatedBytesPerSecond bool              `json:"has_estimated_bytes_per_second"`
	Tier                       int               `json:"tier"`
	IntermediaryIds            []server.Id       `json:"intermediary_ids,omitempty"`
	NetworkOnly                bool              `json:"network_only,omitempty"`
	ReputationFailedNames      string            `json:"reputation_failed_names,omitempty"`
	Location                   *ProviderLocation `json:"location,omitempty"`
	IpFamily                   string            `json:"ip_family,omitempty"`
}

type testingLegacyFindProviders2Result struct {
	Providers []*testingLegacyFindProvidersProvider `json:"providers"`
}

// The legacy wire of an answer: every field but the match.
func testing_legacyFindProviders2Json(t testing.TB, providers []*FindProvidersProvider) []byte {
	t.Helper()
	legacy := &testingLegacyFindProviders2Result{Providers: []*testingLegacyFindProvidersProvider{}}
	for _, provider := range providers {
		legacy.Providers = append(legacy.Providers, &testingLegacyFindProvidersProvider{
			ClientId:                   provider.ClientId,
			EstimatedBytesPerSecond:    provider.EstimatedBytesPerSecond,
			HasEstimatedBytesPerSecond: provider.HasEstimatedBytesPerSecond,
			Tier:                       provider.Tier,
			IntermediaryIds:            provider.IntermediaryIds,
			NetworkOnly:                provider.NetworkOnly,
			ReputationFailedNames:      provider.ReputationFailedNames,
			Location:                   provider.Location,
			IpFamily:                   provider.IpFamily,
		})
	}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An answer that did not climb is byte-identical on the wire to the answer
// before the fallback, and a request without strict decodes as not strict.
func TestFindProviders2LocationFallbackLeavesTheExactWireUnchanged(t *testing.T) {
	countryId, regionId, cityId := server.NewId(), server.NewId(), server.NewId()
	latitude, longitude := 37.4419, -122.1430
	directory := map[server.Id]*locationDirectoryEntry{
		countryId: {Name: "United States", CountryCode: "us", LocationType: LocationTypeCountry, CountryLocationId: &countryId},
		regionId:  {Name: "California", CountryCode: "us", LocationType: LocationTypeRegion, RegionLocationId: &regionId, CountryLocationId: &countryId},
		cityId:    {Name: "Palo Alto", CountryCode: "us", LocationType: LocationTypeCity, Latitude: &latitude, Longitude: &longitude, RegionLocationId: &regionId, CountryLocationId: &countryId},
	}
	located := onlineBackfillScore(false, 1)
	located.CityLocationId, located.RegionLocationId, located.CountryLocationId = &cityId, &regionId, &countryId
	located.MaxBytesPerSecond, located.HasSpeedTest = 5_000_000, true
	located.ReputationFailedNames = "synthetic-vendor"
	unlocated := onlineBackfillScore(true, 1)
	unlocated.NetworkOnly = true
	providers := []*FindProvidersProvider{
		// a named provider, as appendSpecProviders answers it
		{ClientId: server.NewId()},
		findProvidersProviderFromClientScore(located, RankModeQuality, directory, ""),
		findProvidersProviderFromClientScore(unlocated, RankModeSpeed, directory, ""),
	}
	exact, err := json.Marshal(&FindProviders2Result{Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	if legacy := testing_legacyFindProviders2Json(t, providers); !bytes.Equal(exact, legacy) {
		t.Fatalf("an exact answer changed on the wire:\n%s\nwant\n%s", exact, legacy)
	}
	if bytes.Contains(exact, []byte(`"match"`)) {
		t.Fatal("an exact answer carries a match")
	}

	// a climbed answer differs by the match alone
	climbed := []*FindProvidersProvider{
		findProvidersProviderFromClientScore(located, RankModeQuality, directory, LocationTypeRegion),
	}
	b, err := json.Marshal(&FindProviders2Result{Providers: climbed})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`,"match":"region"}`)) {
		t.Fatalf("a climbed provider does not carry its match: %s", b)
	}
	if legacy := testing_legacyFindProviders2Json(t, climbed); !bytes.Equal(bytes.Replace(b, []byte(`,"match":"region"`), nil, 1), legacy) {
		t.Fatalf("a climbed answer differs by more than the match:\n%s\nwant\n%s", b, legacy)
	}

	// an older client sends no strict field
	var args FindProviders2Args
	if err := json.Unmarshal([]byte(`{"specs":[{"location_id":"`+cityId.String()+`"}],"count":10}`), &args); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, args.Strict, false)
	if err := json.Unmarshal([]byte(`{"strict":true}`), &args); err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, args.Strict, true)
}

// The answer FindProviders2 gives a city with providers is byte-identical to
// its answer before the fallback.
func TestFindProviders2LocationFallbackLeavesTheExactAnswerUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		for _, locationId := range []server.Id{places.cityA2Id, places.cityE2Id, places.regionEId, places.usId} {
			providers := testing_findFallback(ctx, t, testing_fallbackArgs(locationId), server.NewId())
			if len(providers) == 0 {
				t.Fatal("a target with providers answered none")
			}
			exact, err := json.Marshal(&FindProviders2Result{Providers: providers})
			if err != nil {
				t.Fatal(err)
			}
			if legacy := testing_legacyFindProviders2Json(t, providers); !bytes.Equal(exact, legacy) {
				t.Fatalf("an exact answer changed on the wire:\n%s\nwant\n%s", exact, legacy)
			}
		}
	})
}

// provider.yml's kill switch turns the fallback off for every request.
func TestFindProviders2LocationFallbackKillSwitch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		places := testing_newFallbackPlaces(ctx, t)
		testing_assertFallbackAnswer(t, testing_findFallback(ctx, t, testing_fallbackArgs(places.cityA1Id), server.NewId()), LocationTypeRegion, places.aV4)

		resource, err := server.Config.SimpleResource(providerConfigResourceName)
		if err != nil {
			t.Fatal(err)
		}
		config, err := resource.ParseE()
		if err != nil {
			t.Fatal(err)
		}
		config["egress_index"] = map[string]any{"location_fallback": false}
		encoded, err := yaml.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		pop := server.Config.PushSimpleResource(providerConfigResourceName, encoded)
		defer pop()
		requestEgressIndexSettingsSnapshot.Store(nil)
		defer requestEgressIndexSettingsSnapshot.Store(nil)
		if providers := testing_findFallback(ctx, t, testing_fallbackArgs(places.cityA1Id), server.NewId()); len(providers) != 0 {
			t.Fatalf("the kill switch answered %d providers from above an empty city", len(providers))
		}
	})
}

// The setting's default is on, false turns it off, and an unreadable block
// keeps the default.
func TestEgressIndexSettingsLocationFallback(t *testing.T) {
	load := func(config string) *EgressIndexSettings {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
		defer pop()
		return egressIndexSettings()
	}
	connect.AssertEqual(t, DefaultEgressIndexSettings().LocationFallback, true)
	connect.AssertEqual(t, load("").LocationFallback, true)
	connect.AssertEqual(t, load("egress_index:\n  location_fallback: false\n").LocationFallback, false)
	connect.AssertEqual(t, load("egress_index:\n  location_fallback: true\n").LocationFallback, true)
	connect.AssertEqual(t, load("egress_index:\n  location_fallback: sometimes\n").LocationFallback, true)
}

// The levels a request's targets climb: each city's region, then each city's
// and region's country, in the target's own country, leaving out targets the
// request or a nearer level reads.
func TestFindProviders2LocationFallbackLevels(t *testing.T) {
	entries := map[server.Id]*locationDirectoryEntry{}
	place := func(locationType LocationType, countryCode string, regionLocationId *server.Id, countryLocationId *server.Id) server.Id {
		return testing_addFallbackPlace(entries, locationType, countryCode, regionLocationId, countryLocationId)
	}
	usId := place(LocationTypeCountry, "us", nil, nil)
	caId := place(LocationTypeCountry, "ca", nil, nil)
	regionId := place(LocationTypeRegion, "us", nil, &usId)
	cityId := place(LocationTypeCity, "us", &regionId, &usId)
	otherCityId := place(LocationTypeCity, "us", &regionId, &usId)
	foreignRegionId := place(LocationTypeRegion, "ca", nil, &caId)
	// a city whose row names a region and a country of another country
	misfiledCityId := place(LocationTypeCity, "us", &foreignRegionId, &caId)
	// a city whose row names no region, and one the directory holds without parents
	regionlessCityId := place(LocationTypeCity, "us", nil, &usId)
	orphanCityId := place(LocationTypeCity, "us", nil, nil)
	// a city whose region is missing from the directory
	unknownRegionId := server.NewId()
	unknownRegionCityId := place(LocationTypeCity, "us", &unknownRegionId, &usId)
	groupId := server.NewId()

	type wantLevel struct {
		match       LocationType
		locationIds []server.Id
	}
	for i, c := range []struct {
		locationIds []server.Id
		climbable   bool
		levels      []wantLevel
	}{
		{locationIds: []server.Id{cityId}, climbable: true, levels: []wantLevel{{LocationTypeRegion, []server.Id{regionId}}, {LocationTypeCountry, []server.Id{usId}}}},
		{locationIds: []server.Id{regionId}, climbable: true, levels: []wantLevel{{LocationTypeCountry, []server.Id{usId}}}},
		{locationIds: []server.Id{cityId, otherCityId}, climbable: true, levels: []wantLevel{{LocationTypeRegion, []server.Id{regionId}}, {LocationTypeCountry, []server.Id{usId}}}},
		// the region the request already reads is not read again
		{locationIds: []server.Id{cityId, regionId}, climbable: true, levels: []wantLevel{{LocationTypeCountry, []server.Id{usId}}}},
		{locationIds: []server.Id{cityId, usId}, climbable: true, levels: []wantLevel{{LocationTypeRegion, []server.Id{regionId}}}},
		{locationIds: []server.Id{cityId, foreignRegionId}, climbable: true, levels: []wantLevel{{LocationTypeRegion, []server.Id{regionId}}, {LocationTypeCountry, []server.Id{usId, caId}}}},
		// never into another country
		{locationIds: []server.Id{misfiledCityId}, climbable: true},
		{locationIds: []server.Id{regionlessCityId}, climbable: true, levels: []wantLevel{{LocationTypeCountry, []server.Id{usId}}}},
		{locationIds: []server.Id{orphanCityId}, climbable: true},
		{locationIds: []server.Id{unknownRegionCityId}, climbable: true, levels: []wantLevel{{LocationTypeCountry, []server.Id{usId}}}},
		// nothing above a country, a group or a location the directory lacks
		{locationIds: []server.Id{usId}},
		{locationIds: []server.Id{groupId}},
		{locationIds: []server.Id{}},
	} {
		locationIds := map[server.Id]bool{}
		for _, locationId := range c.locationIds {
			locationIds[locationId] = true
		}
		fallback := newFindProviders2LocationFallback(locationIds, entries)
		if fallback.climbable != c.climbable {
			t.Fatalf("case %d climbable=%t, want %t", i, fallback.climbable, c.climbable)
		}
		for _, want := range c.levels {
			level, ok := fallback.nextLevel()
			if !ok {
				t.Fatalf("case %d stopped before its %s level", i, want.match)
			}
			wantIds := slices.Clone(want.locationIds)
			gotIds := slices.Collect(maps.Keys(level.locationIds))
			slices.SortFunc(wantIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			slices.SortFunc(gotIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
			if level.match != want.match || !slices.Equal(gotIds, wantIds) {
				t.Fatalf("case %d level %s with %d targets, want %s with %d", i, level.match, len(gotIds), want.match, len(wantIds))
			}
		}
		if level, ok := fallback.nextLevel(); ok {
			t.Fatalf("case %d climbed past its levels to %s", i, level.match)
		}
	}
	// an empty directory, as before the first load, climbs nowhere
	if fallback := newFindProviders2LocationFallback(map[server.Id]bool{cityId: true}, nil); fallback.climbable {
		t.Fatal("a target an unloaded directory does not hold could climb")
	}
}

// The counter counts a climbable request once, by the level that answered or
// none, and a request whose targets could not climb not at all.
func TestFindProviders2LocationFallbackRecordsOnce(t *testing.T) {
	climbable := &findProviders2LocationFallback{climbable: true}
	for _, c := range []struct {
		match    LocationType
		answered bool
		level    string
	}{
		{match: LocationTypeRegion, answered: true, level: LocationTypeRegion},
		{match: LocationTypeCountry, answered: true, level: LocationTypeCountry},
		{match: LocationTypeCountry, answered: false, level: findProviders2LocationFallbackNone},
		{match: "", answered: false, level: findProviders2LocationFallbackNone},
	} {
		before := testing_fallbackCount(t, LocationTypeCity, c.level)
		climbable.record(LocationTypeCity, c.match, c.answered)
		if after := testing_fallbackCount(t, LocationTypeCity, c.level); after != before+1 {
			t.Fatalf("%s answered=%t counted %g, want %g", c.match, c.answered, after, before+1)
		}
	}
	unclimbable := &findProviders2LocationFallback{}
	before := testing_fallbackCount(t, "country", findProviders2LocationFallbackNone)
	unclimbable.record("country", "", false)
	if after := testing_fallbackCount(t, "country", findProviders2LocationFallbackNone); after != before {
		t.Fatal("a target that cannot climb was counted")
	}
}

// targetsWithoutSupply is complete's cache_empty boundary: for a discovery
// request asking for providers, a zero answer is cache_empty exactly when it
// holds.
func TestFindProviders2TargetsWithoutSupplyIsTheCacheEmptyBoundary(t *testing.T) {
	cityId := server.NewId()
	for i, mutate := range []func(observation *findProviders2SelectionObservation){
		func(observation *findProviders2SelectionObservation) {},
		func(observation *findProviders2SelectionObservation) { observation.loaded = 1 },
		func(observation *findProviders2SelectionObservation) {
			observation.loaded, observation.eligible = 1, 1
		},
		func(observation *findProviders2SelectionObservation) {
			observation.loaded, observation.dropped[3], observation.explicitSources = 1, 1, 2
		},
		func(observation *findProviders2SelectionObservation) { observation.load.missingPages = 1 },
		func(observation *findProviders2SelectionObservation) { observation.load.missingTargets = 1 },
		func(observation *findProviders2SelectionObservation) { observation.backfillUnavailable = true },
		func(observation *findProviders2SelectionObservation) { observation.load.unreadNonModernRows = 1 },
	} {
		observation := newFindProviders2SelectionObservation(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &cityId}}, Count: 10})
		observation.discovery, observation.targetKind, observation.requestedCount = true, LocationTypeCity, 20
		mutate(observation)
		withoutSupply := observation.targetsWithoutSupply()
		observation.complete(0)
		if withoutSupply != (observation.reason == "cache_empty") {
			t.Fatalf("case %d without supply=%t, reason %s", i, withoutSupply, observation.reason)
		}
		observation.finish(context.Background())
	}
}

// The directory's parents round-trip through the fleet cache; a row without
// them decodes as a place that cannot climb.
func TestLocationDirectoryCacheCarriesParents(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		countryId, regionId, cityId := server.NewId(), server.NewId(), server.NewId()
		setLocationDirectoryCache(ctx, map[server.Id]*locationDirectoryEntry{
			cityId: {Name: "Synthetic City", CountryCode: "xx", LocationType: LocationTypeCity, RegionLocationId: &regionId, CountryLocationId: &countryId},
		}, time.Minute)
		entry := getLocationDirectoryCache(ctx)[cityId]
		if entry == nil || entry.RegionLocationId == nil || *entry.RegionLocationId != regionId || entry.CountryLocationId == nil || *entry.CountryLocationId != countryId {
			t.Fatal("the directory cache lost a city's parents")
		}
		legacy, err := json.Marshal([]map[string]any{{"location_id": cityId, "name": "Synthetic City", "country_code": "xx", "location_type": LocationTypeCity}})
		if err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, locationDirectoryRedisKey, string(legacy), time.Minute).Err())
		})
		entry = getLocationDirectoryCache(ctx)[cityId]
		if entry == nil || entry.RegionLocationId != nil || entry.CountryLocationId != nil {
			t.Fatal("a row without parents was assigned guessed ones")
		}
	})
}

// The score export and the directory query, end to end: a city whose one
// provider fails a common gate is published without an admitted provider, and
// is answered from its region; a city and region in that state from the
// country. The directory query supplies the parents.
func TestFindProviders2LocationFallbackFromTheScoreExport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		place := func(city string, region string) *Location {
			location := &Location{
				LocationType: LocationTypeCity,
				City:         city,
				Region:       region,
				Country:      "United States",
				CountryCode:  "us",
			}
			CreateLocation(ctx, location)
			return location
		}
		paloAlto := place("Palo Alto", "California")
		sanJose := place("San Jose", "California")
		reno := place("Reno", "Nevada")

		handlerId := CreateNetworkClientHandler(ctx)
		connectProvider := func(location *Location, address string) server.Id {
			networkId := server.NewId()
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, address, handlerId)
			connect.AssertEqual(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{ArinQualityVerified: true})
			connect.AssertEqual(t, err, nil)
			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic: []byte("public-secret"),
			})
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client_latency (connection_id, latency_ms, sample_count) VALUES ($1, $2, $3)`,
					connectionId, 30, 1,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client_speed (connection_id, bytes_per_second, sample_count) VALUES ($1, $2, $3)`,
					connectionId, 100*1024*1024, 1,
				))
			})
			return clientId
		}
		// a current hard egress failure keeps a provider out of every target,
		// while the targets its row names are still published
		refused := func(location *Location, address string) {
			clientId := connectProvider(location, address)
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId: clientId, MeasuredAt: server.NowUtc(),
				OKCount: 130, Total: 131, TLSAuthenticationFailure: true,
			})
		}
		refused(paloAlto, "192.0.2.1:0")
		refused(reno, "192.0.2.2:0")
		admittedClientId := connectProvider(sanJose, "192.0.2.3:0")
		testing_setProviderEgressHealthy(ctx, admittedClientId)
		UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())

		err := UpdateClientScores(ctx, time.Hour, 1)
		connect.AssertEqual(t, err, nil)
		// the reliability rows exist now, so the directory holds every place
		// and its parents
		loadLocationDirectory()
		directory := locationDirectory()
		if entry := directory[paloAlto.LocationId]; entry == nil || entry.RegionLocationId == nil || *entry.RegionLocationId != paloAlto.RegionLocationId || entry.CountryLocationId == nil || *entry.CountryLocationId != paloAlto.CountryLocationId {
			t.Fatal("the directory query did not carry the city's parents")
		}
		if entry := directory[paloAlto.RegionLocationId]; entry == nil || entry.CountryLocationId == nil || *entry.CountryLocationId != paloAlto.CountryLocationId {
			t.Fatal("the directory query did not carry the region's country")
		}

		find := func(location *Location, ipFamily string, strict bool) []*FindProvidersProvider {
			args := testing_fallbackArgs(location.LocationId)
			args.IpFamily, args.Strict = ipFamily, strict
			return testing_findFallback(ctx, t, args, server.NewId())
		}
		admitted := []server.Id{admittedClientId}

		// the control: the city with the admitted provider answers it, unmarked
		providers := find(sanJose, "", false)
		connect.AssertEqual(t, egressTestIds(providers), admitted)
		connect.AssertEqual(t, providers[0].Match, LocationType(""))

		providers = find(paloAlto, "", false)
		connect.AssertEqual(t, egressTestIds(providers), admitted)
		connect.AssertEqual(t, providers[0].Match, LocationTypeRegion)
		if location := providers[0].Location; location == nil || location.City != "San Jose" || location.Region != "California" {
			t.Fatal("the region's provider lost its own location")
		}

		providers = find(reno, "", false)
		connect.AssertEqual(t, egressTestIds(providers), admitted)
		connect.AssertEqual(t, providers[0].Match, LocationTypeCountry)

		if providers := find(paloAlto, "", true); len(providers) != 0 {
			t.Fatalf("a strict request for the empty city answered %d providers", len(providers))
		}
		// the facet export holds no v6 provider at any level
		if providers := find(paloAlto, "v6-only", false); len(providers) != 0 {
			t.Fatalf("a v6-only request answered %d providers without v6 supply", len(providers))
		}
	})
}
