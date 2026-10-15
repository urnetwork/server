// A Quality request that borrows from the Speed bucket takes only the Speed
// natives within Quality's own latency and throughput cutoffs
// (connect/GEOMAP.md §10.1): their Quality tier is below
// ClientScoreCutoffTier. Those past the cutoffs are online members, as every
// native is, and only the Online fallback lends them, so no answer gets
// shorter. Speed, force_minimum, online and named requests are unchanged.
package model

import (
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

// The real export path publishes two verified natives, three Speed natives
// that Quality refuses within its cutoffs, three it refuses past them (two by
// latency, one by throughput) and one online member of neither bucket. A
// Quality request for ten borrows the three within the cutoffs at their Speed
// tier plus the offset, and lends the three past them only from the Online
// fallback, at twice the offset with the online member: all nine, as before,
// where the Speed natives past the cutoffs were borrowed at the borrowed tier.
// With one place left after the borrowed, the fallback fills it.
func TestBackfillQualityBorrowsSpeedWithinItsCutoffs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Cutoff City", "Cutoff Region", "United States", "us")
		natives := []server.Id{}
		for range 2 {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestProbed(ctx, provider, city, 0, "us")
			natives = append(natives, provider.clientId)
		}
		within := []server.Id{}
		for range 3 {
			within = append(within, egressTestOverLine(ctx, t, city, egressTestFast).clientId)
		}
		past := []server.Id{}
		for _, performance := range []egressTestPerformance{egressTestPastQualityLatency, egressTestPastQualityLatency, egressTestPastQualityThroughput} {
			past = append(past, egressTestOverLine(ctx, t, city, performance).clientId)
		}
		onlineOnly := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestHealth(ctx, onlineOnly.clientId, server.NowUtc(), 3, 1)
		egressTestPasses(ctx, t)

		published := egressTestCachedScores(ctx, t, city, RankModeQuality, false)
		member := func(clientId server.Id, quality bool, speed bool, pastCutoffs bool) {
			score := published[clientId]
			if score == nil || !score.Online || score.PassesMinimums[RankModeQuality] != quality || score.PassesMinimums[RankModeSpeed] != speed ||
				(score.Tiers[RankModeQuality] == ClientScoreCutoffTier) != pastCutoffs {
				t.Fatal("fixture bucket membership or Quality tier changed")
			}
		}
		for _, clientId := range natives {
			member(clientId, true, true, false)
		}
		for _, clientId := range within {
			member(clientId, false, true, false)
		}
		for _, clientId := range past {
			member(clientId, false, true, true)
		}
		member(onlineOnly.clientId, false, false, false)

		offset := egressTestBackfillOffset()
		lentByFallback := append(slices.Clone(past), onlineOnly.clientId)
		offlineTestForEachReader(t, func(nativeReader bool) {
			for _, count := range []int{10, 6} {
				label := fmt.Sprintf("reader=%t count=%d", nativeReader, count)
				providers := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeQuality, count, false, server.NewId())
				assertEgressTestNoRepeats(t, providers)
				assertEgressTestTiersKeepOrder(t, providers)
				// the location's nine providers, or the six places asked for
				if len(providers) != min(count, 9) {
					t.Fatalf("%s: answered %d providers, want %d", label, len(providers), min(count, 9))
				}
				for i, provider := range providers {
					switch {
					case i < 2:
						if !slices.Contains(natives, provider.ClientId) || provider.Tier != 0 {
							t.Fatalf("%s: place %d is not a native at tier 0", label, i)
						}
					case i < 5:
						if !slices.Contains(within, provider.ClientId) {
							t.Fatalf("%s: place %d is not a Speed native within Quality's cutoffs", label, i)
						}
						if want := published[provider.ClientId].Tiers[RankModeSpeed] + offset; provider.Tier != want {
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

			// Speed borrows from Quality as before; its own natives come first.
			speed := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, 10, false, server.NewId())
			if len(speed) != 9 {
				t.Fatalf("reader=%t: Speed answered %d providers, want 9", nativeReader, len(speed))
			}
			for _, provider := range speed {
				want := published[provider.ClientId].Tiers[RankModeSpeed]
				if provider.ClientId == onlineOnly.clientId {
					want = 2 * offset
				}
				if provider.Tier != want {
					t.Fatalf("reader=%t: Speed answered tier %d, want %d", nativeReader, provider.Tier, want)
				}
			}
		})

		// force_minimum answers its own forced pool, which has no minimums,
		// and borrows nothing: Speed all of it, Quality what the live read
		// admits, which leaves out the ARIN non-quality providers.
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			forcedPool := egressTestCachedScores(ctx, t, city, mode, true)
			want := slices.Collect(maps.Keys(forcedPool))
			if mode == RankModeQuality {
				want = append(slices.Clone(natives), onlineOnly.clientId)
			}
			forced := egressTestFind(ctx, t, egressTestLocationSpec(city), mode, 10, true, server.NewId())
			if !slices.Equal(onlineTestSorted(egressTestIds(forced)...), onlineTestSorted(want...)) {
				t.Fatalf("forced %s changed: answered %d providers, want %d", mode, len(forced), len(want))
			}
			for _, provider := range forced {
				if provider.Tier != forcedPool[provider.ClientId].Tiers[mode] {
					t.Fatalf("forced %s answered a borrowed tier", mode)
				}
			}
		}
		// Online answers every online member at one tier.
		online := egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId())
		everyone := slices.Concat(natives, within, past, []server.Id{onlineOnly.clientId})
		if !slices.Equal(onlineTestSorted(egressTestIds(online)...), onlineTestSorted(everyone...)) {
			t.Fatal("online changed")
		}
		for _, provider := range online {
			if provider.Tier != 0 {
				t.Fatal("online answered a tier")
			}
		}
		// Named, a Speed native past Quality's cutoffs answers as before:
		// Quality refuses it as ARIN non-quality, Speed returns it.
		named := []*ProviderSpec{{ClientId: &past[0]}}
		if providers := egressTestFind(ctx, t, named, RankModeQuality, 10, false, server.NewId()); len(providers) != 0 {
			t.Fatal("named Quality changed")
		}
		if providers := egressTestFind(ctx, t, named, RankModeSpeed, 10, false, server.NewId()); !slices.Equal(egressTestIds(providers), []server.Id{past[0]}) {
			t.Fatal("named Speed changed")
		}
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
