// A Quality request that borrows from the Speed bucket takes only the Speed
// natives within Quality's own latency and throughput cutoffs
// (connect/GEOMAP.md §10.1): their Quality tier is below
// ClientScoreCutoffTier. Those past the cutoffs are online members, as every
// native is, and only the Online fallback lends them, so no answer gets
// shorter. Speed, force_minimum, online and named requests are unchanged.
package model

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// past Quality's latency cutoff (200 ms), and its throughput cutoff
// (800 KiB/s); both are past Speed's cutoffs too
var egressTestPastQualityLatency = egressTestPerformance{latencyMs: 250, bytesPerSecond: 100 * 1024 * 1024}
var egressTestPastQualityThroughput = egressTestPerformance{latencyMs: 10, bytesPerSecond: 500 * 1024}

// One location's nine providers as the real export path publishes them.
type cutoffTestFixture struct {
	city *Location
	// verified: Quality and Speed natives
	natives []server.Id
	// ARIN non-quality Speed natives within Quality's cutoffs
	within []server.Id
	// ARIN non-quality Speed natives past them: two by latency, one by
	// throughput
	past []server.Id
	// a failing URL ratio: online, in neither native bucket
	onlineOnly server.Id
	// the non-forced Quality page, which holds all nine
	published map[server.Id]*ClientScore
}

func newCutoffTestFixture(ctx context.Context, t testing.TB, name string) *cutoffTestFixture {
	t.Helper()
	fixture := &cutoffTestFixture{
		city: egressTestCity(ctx, name+" City", name+" Region", "United States", "us"),
	}
	for range 2 {
		provider := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
		egressTestProbed(ctx, provider, fixture.city, 0, "us")
		fixture.natives = append(fixture.natives, provider.clientId)
	}
	for range 3 {
		fixture.within = append(fixture.within, egressTestOverLine(ctx, t, fixture.city, egressTestFast).clientId)
	}
	for _, performance := range []egressTestPerformance{egressTestPastQualityLatency, egressTestPastQualityLatency, egressTestPastQualityThroughput} {
		fixture.past = append(fixture.past, egressTestOverLine(ctx, t, fixture.city, performance).clientId)
	}
	onlineOnly := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
	egressTestHealth(ctx, onlineOnly.clientId, server.NowUtc(), 3, 1)
	fixture.onlineOnly = onlineOnly.clientId
	egressTestPasses(ctx, t)

	fixture.published = egressTestCachedScores(ctx, t, fixture.city, RankModeQuality, false)
	member := func(clientId server.Id, quality bool, speed bool, pastCutoffs bool) {
		score := fixture.published[clientId]
		if score == nil || !score.Online || score.PassesMinimums[RankModeQuality] != quality || score.PassesMinimums[RankModeSpeed] != speed ||
			(score.Tiers[RankModeQuality] == ClientScoreCutoffTier) != pastCutoffs {
			t.Fatal("fixture bucket membership or Quality tier changed")
		}
	}
	for _, clientId := range fixture.natives {
		member(clientId, true, true, false)
	}
	for _, clientId := range fixture.within {
		member(clientId, false, true, false)
	}
	for _, clientId := range fixture.past {
		member(clientId, false, true, true)
	}
	member(fixture.onlineOnly, false, false, false)
	return fixture
}

// The fixture publishes two verified natives, three Speed natives that
// Quality refuses within its cutoffs, three it refuses past them and one
// online member of neither bucket. A Quality request for ten borrows the
// three within the cutoffs at their Speed tier plus the offset and lends the
// three past them only from the Online fallback, at twice the offset with
// the online member: all nine, the count it answered before, when the three
// past the cutoffs were borrowed at the borrowed tier too. A request for six
// keeps its six: the fallback fills the place left after the borrowed, which
// one of the three past the cutoffs took at the borrowed tier before.
func TestBackfillQualityBorrowsSpeedWithinItsCutoffs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newCutoffTestFixture(ctx, t, "Cutoff")
		offset := egressTestBackfillOffset()
		lentByFallback := append(slices.Clone(fixture.past), fixture.onlineOnly)
		offlineTestForEachReader(t, func(nativeReader bool) {
			for _, count := range []int{10, 6} {
				label := fmt.Sprintf("reader=%t count=%d", nativeReader, count)
				providers := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeQuality, count, false, server.NewId())
				assertEgressTestNoRepeats(t, providers)
				assertEgressTestTiersKeepOrder(t, providers)
				if len(providers) != min(count, 9) {
					t.Fatalf("%s: answered %d providers, want %d", label, len(providers), min(count, 9))
				}
				for i, provider := range providers {
					switch {
					case i < 2:
						if !slices.Contains(fixture.natives, provider.ClientId) || provider.Tier != 0 {
							t.Fatalf("%s: place %d is not a native at tier 0", label, i)
						}
					case i < 5:
						if !slices.Contains(fixture.within, provider.ClientId) {
							t.Fatalf("%s: place %d is not a Speed native within Quality's cutoffs", label, i)
						}
						if want := fixture.published[provider.ClientId].Tiers[RankModeSpeed] + offset; provider.Tier != want {
							t.Fatalf("%s: borrowed Speed native at tier %d, want %d", label, provider.Tier, want)
						}
					default:
						if !slices.Contains(lentByFallback, provider.ClientId) {
							t.Fatalf("%s: place %d is not one the Online fallback lends", label, i)
						}
						if provider.Tier != 2*offset {
							t.Fatalf("%s: place %d at tier %d, want the Online fallback's %d", label, i, provider.Tier, 2*offset)
						}
					}
				}
			}
		})
	})
}

// A Speed native past Quality's cutoffs whose record carries no online flag,
// as an older export's native records may not, is still an online member.
// Quality's Online fallback lends it although no page it reads offers it as
// online, so the answer keeps the place the borrowed draw used to give it.
func TestBackfillQualityLendsSpeedPastItsCutoffsWithoutOnlineFlag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		location := server.NewId()
		native := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		within := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
		pastCutoffs := nativeTestScore(RankModeSpeed, ipFamilyFacetV4Only)
		pastCutoffs.Online = false
		pastCutoffs.Tiers = map[string]int{RankModeQuality: ClientScoreCutoffTier, RankModeSpeed: ClientScoreCutoffTier}
		nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {native}})
		nativeTestPublishLocation(t, location, RankModeSpeed, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {within, pastCutoffs}})

		offset := egressTestBackfillOffset()
		offlineTestForEachReader(t, func(nativeReader bool) {
			providers := egressTestFind(t.Context(), t, []*ProviderSpec{{LocationId: &location}}, RankModeQuality, 3, false, server.NewId())
			want := []*FindProvidersProvider{
				{ClientId: native.ClientId, Tier: 0},
				{ClientId: within.ClientId, Tier: offset},
				{ClientId: pastCutoffs.ClientId, Tier: 2 * offset},
			}
			if len(providers) != len(want) {
				t.Fatalf("reader=%t: answered %d providers, want %d", nativeReader, len(providers), len(want))
			}
			for i, provider := range providers {
				if provider.ClientId != want[i].ClientId || provider.Tier != want[i].Tier {
					t.Fatalf("reader=%t: place %d at tier %d, want tier %d", nativeReader, i, provider.Tier, want[i].Tier)
				}
			}
		})
	})
}

// The requests that must not change, on the same location. Speed draws its
// eight natives at their Speed tiers and has nothing to borrow from Quality,
// whose natives are its own. force_minimum answers its own forced pool, which
// has no minimums, and borrows nothing: Speed all of it, Quality what the
// live read admits, without the ARIN non-quality providers. Online answers
// every online member at one tier. Named, a live Speed native past Quality's
// cutoffs is returned by Quality and Speed alike: a named Quality request needs
// a live connection, not ARIN quality (FP2FIX.md "Named Quality requests").
func TestBackfillQualityCutoffsLeaveOtherRequestsUnchanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newCutoffTestFixture(ctx, t, "Cutoff Unchanged")
		offset := egressTestBackfillOffset()
		everyone := slices.Concat(fixture.natives, fixture.within, fixture.past, []server.Id{fixture.onlineOnly})
		offlineTestForEachReader(t, func(nativeReader bool) {
			speed := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeSpeed, 10, false, server.NewId())
			assertEgressTestNoRepeats(t, speed)
			assertEgressTestTiersKeepOrder(t, speed)
			if !slices.Equal(onlineTestSorted(egressTestIds(speed)...), onlineTestSorted(everyone...)) {
				t.Fatalf("reader=%t: Speed answered %d providers, want 9", nativeReader, len(speed))
			}
			for _, provider := range speed {
				want := fixture.published[provider.ClientId].Tiers[RankModeSpeed]
				if provider.ClientId == fixture.onlineOnly {
					want = 2 * offset
				}
				if provider.Tier != want {
					t.Fatalf("reader=%t: Speed answered tier %d, want %d", nativeReader, provider.Tier, want)
				}
			}

			for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
				forcedPool := egressTestCachedScores(ctx, t, fixture.city, mode, true)
				want := slices.Collect(maps.Keys(forcedPool))
				if mode == RankModeQuality {
					want = append(slices.Clone(fixture.natives), fixture.onlineOnly)
				}
				forced := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), mode, 10, true, server.NewId())
				if !slices.Equal(onlineTestSorted(egressTestIds(forced)...), onlineTestSorted(want...)) {
					t.Fatalf("reader=%t: forced %s answered %d providers, want %d", nativeReader, mode, len(forced), len(want))
				}
				for _, provider := range forced {
					if provider.Tier != forcedPool[provider.ClientId].Tiers[mode] {
						t.Fatalf("reader=%t: forced %s answered a borrowed tier", nativeReader, mode)
					}
				}
			}

			online := egressTestFind(ctx, t, egressTestLocationSpec(fixture.city), RankModeOnline, 10, false, server.NewId())
			if !slices.Equal(onlineTestSorted(egressTestIds(online)...), onlineTestSorted(everyone...)) {
				t.Fatalf("reader=%t: online changed", nativeReader)
			}
			for _, provider := range online {
				if provider.Tier != 0 {
					t.Fatalf("reader=%t: online answered a tier", nativeReader)
				}
			}

			named := []*ProviderSpec{{ClientId: &fixture.past[0]}}
			if providers := egressTestFind(ctx, t, named, RankModeQuality, 10, false, server.NewId()); !slices.Equal(egressTestIds(providers), []server.Id{fixture.past[0]}) {
				t.Fatalf("reader=%t: named Quality changed", nativeReader)
			}
			if providers := egressTestFind(ctx, t, named, RankModeSpeed, 10, false, server.NewId()); !slices.Equal(egressTestIds(providers), []server.Id{fixture.past[0]}) {
				t.Fatalf("reader=%t: named Speed changed", nativeReader)
			}
		})
	})
}
