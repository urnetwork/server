package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The country floor against the database (owner decision 2026-11-13: keep
// the 80% URL bar, but never let a country go empty): the passes in the order
// the taskworker runs them, then FindProviders2 and the location counts,
// under the URL ratio Main runs (FP2 variant (c)). These tests use no symbol
// the floor added, so they run unchanged against the code before it, where
// every country below the bar answers nothing and lists no provider.

// FP2 variant (c), as Main's provider.yml sets it.
const countryFloorTestProviderConfig = "online_url_success_ratio: 0.8\n"

// Connects one provider at the city per entry of oks, each with ten URL runs
// in the window of which ok succeeded, and reliability that passes.
func countryFloorTestConnect(ctx context.Context, t testing.TB, city *Location, oks ...int) []server.Id {
	t.Helper()
	clientIds := []server.Id{}
	for _, ok := range oks {
		provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, provider.clientId, server.NowUtc(), 10, 10-ok)
		for lookbackIndex := range 3 {
			egressTestReliability(ctx, provider.clientId, lookbackIndex, 1, 1)
		}
		clientIds = append(clientIds, provider.clientId)
	}
	return clientIds
}

// The ids in client id order, for comparing answers as sets.
func countryFloorTestSortedIds(clientIds []server.Id) []server.Id {
	sorted := slices.Clone(clientIds)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return sorted
}

// The client ids FindProviders2 answers for the location in the mode, sorted,
// and the answer itself.
func countryFloorTestFind(ctx context.Context, t testing.TB, location *Location, rankMode RankMode) ([]server.Id, []*FindProvidersProvider) {
	t.Helper()
	providers := egressTestFind(ctx, t, egressTestLocationSpec(location), rankMode, 10, false, server.NewId())
	return countryFloorTestSortedIds(egressTestIds(providers)), providers
}

// A country none of whose providers passes the bar, as pe, nz and ar went on
// 2026-11-13, is served by its best three below it in every mode, at its
// country and its city, and listed with them. Quality and Speed answer them
// in the Online fallback's band, where every native and borrowed native would
// come first. The two below them stay out.
func TestCountryFloorKeepsACountryBelowTheBarServed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(countryFloorTestProviderConfig))
		defer pop()
		city := egressTestCity(ctx, "Lima", "Lima", "Peru", "pe")
		resetCountryCodeLocationIds()
		// 7, 6, 5, 4 and 0 of 10: four have a success, so the country is
		// not exempt, and none reaches 0.8
		clientIds := countryFloorTestConnect(ctx, t, city, 7, 6, 5, 4, 0)
		egressTestPasses(ctx, t)

		country := &Location{LocationId: city.CountryLocationId}
		best := countryFloorTestSortedIds(clientIds[:3])
		for _, location := range []*Location{country, city} {
			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
				answered, providers := countryFloorTestFind(ctx, t, location, rankMode)
				if !slices.Equal(answered, best) {
					t.Fatalf("%s %s: answered %v, want the best three below the bar %v", location.LocationId, rankMode, answered, best)
				}
				if rankMode == RankModeOnline {
					continue
				}
				for _, provider := range providers {
					if provider.Tier < 2*egressTestBackfillOffset() {
						t.Fatalf("%s: a provider below the bar was answered at tier %d, ahead of the Online fallback's band", rankMode, provider.Tier)
					}
				}
			}
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, country), 3)
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 3)
	})
}

// A country whose buckets each hold three providers above the bar is
// answered and listed exactly as before the floor: the two below the bar stay
// out, and the natives keep their tiers.
func TestCountryFloorLeavesACountryAboveItUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(countryFloorTestProviderConfig))
		defer pop()
		city := egressTestCity(ctx, "Auckland", "Auckland", "New Zealand", "nz")
		resetCountryCodeLocationIds()
		above := countryFloorTestConnect(ctx, t, city, 10, 10, 9)
		countryFloorTestConnect(ctx, t, city, 7, 2)
		egressTestPasses(ctx, t)

		country := &Location{LocationId: city.CountryLocationId}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
			answered, providers := countryFloorTestFind(ctx, t, country, rankMode)
			connect.AssertEqual(t, answered, countryFloorTestSortedIds(above))
			for _, provider := range providers {
				if ClientScoreCutoffTier < provider.Tier {
					t.Fatalf("%s: a native was answered at tier %d", rankMode, provider.Tier)
				}
			}
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, country), 3)
	})
}

// A country with two providers above the bar gets the best one below it as
// the third, answered behind the two in every native mode: the floor fills,
// it never displaces.
func TestCountryFloorTopsUpBehindTheProvidersAboveTheBar(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(countryFloorTestProviderConfig))
		defer pop()
		city := egressTestCity(ctx, "Montevideo", "Montevideo", "Uruguay", "uy")
		resetCountryCodeLocationIds()
		above := countryFloorTestConnect(ctx, t, city, 10, 9)
		below := countryFloorTestConnect(ctx, t, city, 7, 6, 3)
		egressTestPasses(ctx, t)

		country := &Location{LocationId: city.CountryLocationId}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			answered, providers := countryFloorTestFind(ctx, t, country, rankMode)
			connect.AssertEqual(t, answered, countryFloorTestSortedIds(append(slices.Clone(above), below[0])))
			for i, provider := range providers {
				admitted := provider.ClientId == below[0]
				if admitted != (i == len(providers)-1) {
					t.Fatalf("%s: the provider below the bar is not answered last: %v", rankMode, egressTestIds(providers))
				}
				if admitted && provider.Tier < 2*egressTestBackfillOffset() || !admitted && ClientScoreCutoffTier < provider.Tier {
					t.Fatalf("%s: %s answered at tier %d", rankMode, provider.ClientId, provider.Tier)
				}
			}
		}
		answered, _ := countryFloorTestFind(ctx, t, country, RankModeOnline)
		connect.AssertEqual(t, answered, countryFloorTestSortedIds(append(slices.Clone(above), below[0])))
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, country), 3)
	})
}

// The floor admits nothing another rule excludes: the best three below the
// bar are a TLS finding the judge ruled the provider's own, an ARIN risk and
// a reliability failure, so only the two worse ones are served, and the
// country stays below the floor rather than take them.
func TestCountryFloorNeverAdmitsAnExcludedProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(countryFloorTestProviderConfig))
		defer pop()
		city := egressTestCity(ctx, "Buenos Aires", "Buenos Aires", "Argentina", "ar")
		resetCountryCodeLocationIds()
		now := server.NowUtc().Truncate(time.Microsecond)

		judged := countryFloorTestConnect(ctx, t, city, 7)[0]
		risky := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinQualityVerified: true, ArinRisk: true})
		egressTestHealth(ctx, risky.clientId, now, 10, 3)
		unreliable := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, unreliable.clientId, now, 10, 3)
		for lookbackIndex := range 3 {
			egressTestReliability(ctx, risky.clientId, lookbackIndex, 1, 1)
			// below every lookback's minimum
			egressTestReliability(ctx, unreliable.clientId, lookbackIndex, 0.1, 0.1)
		}
		eligible := countryFloorTestConnect(ctx, t, city, 5, 4)

		// the judged provider fails TLS on a URL that verifies directly and
		// on the providers of other networks
		site := testingTlsSite("country-floor-site", "https://country-floor.example/")
		SetProviderEgressHealth(ctx, testingTlsUrlResult(judged, time.Time{}, site, now, true))
		for i := range 3 {
			SetProviderEgressHealth(ctx, testingTlsUrlResult(testingTlsProvider(ctx), time.Time{}, site, now.Add(time.Duration(i+1)*time.Second), false))
		}
		testingJudgeTlsFindings(t, ctx, now.Add(time.Minute), []string{site.Name}, map[string]string{"country-floor.example": "verified"})
		if verdict := testingTlsVerdict(t, ctx, judged, site.Url); verdict != "provider" {
			t.Fatalf("the finding was judged %q, not the provider's", verdict)
		}
		egressTestPasses(ctx, t)

		country := &Location{LocationId: city.CountryLocationId}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
			answered, _ := countryFloorTestFind(ctx, t, country, rankMode)
			if !slices.Equal(answered, countryFloorTestSortedIds(eligible)) {
				t.Fatalf("%s: answered %v, want only the two eligible %v (judged %s, risky %s, unreliable %s)", rankMode, answered, countryFloorTestSortedIds(eligible), judged, risky.clientId, unreliable.clientId)
			}
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, country), 2)
	})
}

// Today's facets keep the UDP gate: a provider the probe found unable to
// carry UDP is never answered to a request without a selector, even as the
// best below the bar, so such a request in a country of them gets only the
// one that can. Selector mode's limited window, which takes tcp exits for
// tcp flows alone, gets the best two of them from their classified copies.
func TestCountryFloorKeepsTheUdpGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(countryFloorTestProviderConfig))
		defer pop()
		city := egressTestCity(ctx, "Santiago", "Santiago", "Chile", "cl")
		resetCountryCodeLocationIds()
		// 6 of 10, every failure a plain one: no capability is in doubt
		plain := capabilityTestConnect(ctx, t, city, false)
		capabilityTestHealth(ctx, plain, 6, []int{0, 0, 0, 0}, capabilityTestPqPositive)
		// five failures each, two of them on HTTP/3 alone: below 0.8 even
		// with those forgiven, and tcp
		udp := []server.Id{}
		for _, passes := range []int{5, 4, 3} {
			clientId := capabilityTestConnect(ctx, t, city, false)
			capabilityTestHealth(ctx, clientId, passes, []int{1, 1, 0, 0, 0}, capabilityTestPqPositive)
			udp = append(udp, clientId)
		}
		for _, clientId := range append([]server.Id{plain}, udp...) {
			for lookbackIndex := range 3 {
				egressTestReliability(ctx, clientId, lookbackIndex, 1, 1)
			}
		}
		egressTestPasses(ctx, t)

		country := &Location{LocationId: city.CountryLocationId}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
			answered, _ := countryFloorTestFind(ctx, t, country, rankMode)
			if !slices.Equal(answered, []server.Id{plain}) {
				t.Fatalf("%s: a request without a selector answered %v, want only the provider that carries UDP %s", rankMode, answered, plain)
			}
		}
		connect.AssertEqual(t, egressTestLocationCount(ctx, t, country), 1)

		clientSession := testingCreateProviderSearchSession(
			ctx,
			session.NewByJwt(server.NewId(), server.NewId(), "country-floor-test", false, false),
		)
		result, err := FindProviders2(&FindProviders2Args{
			Specs:      egressTestLocationSpec(country),
			Count:      10,
			ForceCount: true,
			RankMode:   RankModeQuality,
			Select:     capabilityTestSelect("ipv4", "tcp", 3, 0, 0, 0),
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		limited := map[server.Id]string{}
		for _, provider := range result.Providers {
			if provider.Capabilities == nil {
				t.Fatalf("a selector-mode answer has a provider without labels")
			}
			limited[provider.ClientId] = provider.Capabilities.Protocol
		}
		connect.AssertEqual(t, limited, map[server.Id]string{udp[0]: ProviderCapabilityProtocolTcp, udp[1]: ProviderCapabilityProtocolTcp})
	})
}
