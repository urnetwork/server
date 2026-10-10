package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Fixture encoding uses the same bounded page contract as the publisher;
// callers choose facets explicitly to make late-page order deterministic.
func nativeTestFacets(t testing.TB, scores map[ipFamilyFacet][]*ClientScore) map[ipFamilyFacet]clientScoreFacetPayload {
	t.Helper()
	result := map[ipFamilyFacet]clientScoreFacetPayload{}
	for _, facet := range ipFamilyFacets {
		pages := [][]byte{}
		counts := []int{}
		for start := 0; start < len(scores[facet]); start += ClientScoreSampleCount {
			page := scores[facet][start:min(start+ClientScoreSampleCount, len(scores[facet]))]
			pages = append(pages, gobEncodeForTest(t, page))
			counts = append(counts, len(page))
		}
		result[facet] = clientScoreFacetPayload{
			counts: counts, countsBytes: gobEncodeForTest(t, counts),
			encodeSample: func(index int) []byte { return pages[index] },
		}
	}
	return result
}

func nativeTestScore(mode RankMode, facet ipFamilyFacet) *ClientScore {
	score := onlineBackfillScore(true, 1)
	validUntil := server.NowUtc().Add(time.Hour)
	score.EgressValidUntil = &validUntil
	score.PassesMinimums = map[RankMode]bool{mode: true}
	if facet == ipFamilyFacetDualstack {
		score.IpFamilies = ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
	}
	return score
}

// Publish both schemas. The old union deliberately includes online rows;
// native pages contain only the requested mode's admitted native members.
func nativeTestPublishLocation(t testing.TB, location server.Id, mode RankMode, scores map[ipFamilyFacet][]*ClientScore) {
	t.Helper()
	ctx := t.Context()
	natives := map[ipFamilyFacet][]*ClientScore{}
	for facet, members := range scores {
		writeSubscriberFactsForScores(ctx, members)
		for _, score := range members {
			if score.PassesMinimums[mode] {
				natives[facet] = append(natives[facet], score)
			}
		}
	}
	server.Redis(ctx, func(r server.RedisClient) {
		legacy := nativeTestFacets(t, scores)
		pipe := r.Pipeline()
		for _, facet := range ipFamilyFacets {
			payload := legacy[facet]
			for index := range payload.counts {
				pipe.Set(ctx, clientScoreLocationFacetSampleKey(false, mode, location, server.Id{}, facet, index), payload.encodeSample(index), time.Hour)
			}
			pipe.Set(ctx, clientScoreLocationFacetCountsKey(false, mode, location, server.Id{}, facet), payload.countsBytes, time.Hour)
		}
		for _, caller := range countryCodeLocationIds() {
			pipe.Set(ctx, clientScoreLocationAliasKey(false, mode, location, caller), clientScoreAliasBaselineValue, time.Hour)
		}
		pipe.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember)
		if _, err := pipe.Exec(ctx); err != nil {
			t.Fatal(err)
		}
		key := clientScoreNativeKey(clientScoreLocationCountsKey(false, mode, location, server.Id{}))
		if err := writeClientScoreNativeSnapshot(ctx, r, key, time.Hour, nativeTestFacets(t, natives)); err != nil {
			t.Fatal(err)
		}
		for _, caller := range countryCodeLocationIds() {
			if caller != (server.Id{}) {
				if err := r.Set(ctx, clientScoreNativeKey(clientScoreLocationCountsKey(false, mode, location, caller)), clientScoreNativeBaseline, time.Hour).Err(); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

// Online-only preferred-facet rows cannot end native discovery. Both modes
// have the same deterministic late-native boundary and no shuffled escape.
func TestNativeFindProvidersSkipsOnlineDilution(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			location := server.NewId()
			scores := map[ipFamilyFacet][]*ClientScore{}
			for range 6000 {
				score := onlineBackfillScore(true, 1)
				score.IpFamilies = ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6
				scores[ipFamilyFacetDualstack] = append(scores[ipFamilyFacetDualstack], score)
			}
			allowed := map[server.Id]bool{}
			for range 20 {
				score := nativeTestScore(mode, ipFamilyFacetV4Only)
				allowed[score.ClientId] = true
				scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
			}
			for _, publishedMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				nativeTestPublishLocation(t, location, publishedMode, scores)
			}
			clientSession := testingCreateProviderSearchSession(t.Context(), session.NewByJwt(server.NewId(), server.NewId(), "native-page-test", false, false))
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: mode}, clientSession)
			if err != nil || result == nil || len(result.Providers) != 20 {
				t.Fatalf("native quota was not filled: mode=%s result=%v err=%v", mode, result, err)
			}
			for _, provider := range result.Providers {
				if !allowed[provider.ClientId] || provider.Tier != 0 {
					t.Fatalf("online dilution hid a later native: mode=%s tier=%d", mode, provider.Tier)
				}
			}
			assertEgressTestNoRepeats(t, result.Providers)
			loaded, cursor, available, err := loadNativeClientScoresWithCursor(t.Context(), mode, map[server.Id]bool{location: true}, nil, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, nil)
			if err != nil || !available || len(loaded) != 20 || cursor.readCount != 20 || cursor.hasMore() {
				t.Fatalf("native read scanned the online union: available=%t count=%d cursor=%+v err=%v", available, len(loaded), cursor, err)
			}
		}
	})
}

// Post-filter native exhaustion is not the old exclusion-row cap. Every
// rejected row lies in the earlier facet, so this requires real extra pages.
func TestNativeFindProvidersRefillsPastFilteredAllowance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		for _, cause := range []string{"network", "hard", "explicit", "duplicate"} {
			location, callerNetwork := server.NewId(), server.NewId()
			scores := map[ipFamilyFacet][]*ClientScore{}
			excluded := []server.Id{}
			hard := []any{}
			allowed := map[server.Id]bool{}
			for range 2800 {
				score := nativeTestScore(RankModeQuality, ipFamilyFacetDualstack)
				switch cause {
				case "network":
					score.NetworkOnly = true
				case "hard":
					hard = append(hard, score.ClientId.String())
				case "explicit":
					excluded = append(excluded, score.ClientId)
				case "duplicate":
					if len(scores[ipFamilyFacetDualstack]) != 0 {
						score = scores[ipFamilyFacetDualstack][0]
					}
					allowed[score.ClientId] = true
				}
				scores[ipFamilyFacetDualstack] = append(scores[ipFamilyFacetDualstack], score)
			}
			for range 20 {
				score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
				allowed[score.ClientId] = true
				scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
			}
			for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
				nativeTestPublishLocation(t, location, mode, scores)
			}
			if 0 < len(hard) {
				server.Redis(t.Context(), func(r server.RedisClient) {
					server.Raise(r.SAdd(t.Context(), providerHardExclusionsKey, hard...).Err())
				})
			}
			clientSession := testingCreateProviderSearchSession(t.Context(), session.NewByJwt(callerNetwork, server.NewId(), "native-filter-test", false, false))
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, ExcludeClientIds: excluded}, clientSession)
			if err != nil || result == nil || len(result.Providers) != 20 {
				t.Fatalf("%s native refill stopped at the legacy allowance: result=%v err=%v", cause, result, err)
			}
			for _, provider := range result.Providers {
				if !allowed[provider.ClientId] || provider.Tier != 0 {
					t.Fatalf("%s native refill changed eligibility or tier", cause)
				}
			}
			assertEgressTestNoRepeats(t, result.Providers)
		}
	})
}

// An explicitly complete empty native source permits online fallback even
// when its online union is much larger than the native compatibility cap.
func TestNativeFindProvidersEmptyNativesKeepOnlineFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		location := server.NewId()
		scores := map[ipFamilyFacet][]*ClientScore{}
		for range 6000 {
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], onlineBackfillScore(true, 1))
		}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocation(t, location, mode, scores)
		}
		clientSession := testingCreateProviderSearchSession(t.Context(), session.NewByJwt(server.NewId(), server.NewId(), "native-empty-test", false, false))
		result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}}, clientSession)
		if err != nil || result == nil || len(result.Providers) != 20 {
			t.Fatalf("proven empty native sources lost online availability: result=%v err=%v", result, err)
		}
		for _, provider := range result.Providers {
			if provider.Tier != 2*egressTestBackfillOffset() {
				t.Fatal("online fallback lost its lower-priority tier")
			}
		}
		assertEgressTestNoRepeats(t, result.Providers)
	})
}

func nativeTestReadOne(t testing.TB, location server.Id, want server.Id) {
	t.Helper()
	scores, _, available, err := loadNativeClientScoresWithCursor(t.Context(), RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, 20, []ipFamilyFacet{ipFamilyFacetV4Only}, nil)
	if err != nil || !available || len(scores) != 1 || scores[want] == nil {
		t.Fatalf("last-good native snapshot changed: available=%t count=%d err=%v", available, len(scores), err)
	}
}

// A failed or competing writer cannot replace the last complete snapshot;
// repeated attempts keep exactly two hash slots rather than TTL-sized history.
func TestNativePublicationRetainsLastGoodAndRejectsStaleWriter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, location := t.Context(), server.NewId()
		first := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {first}})
		key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
		server.Redis(ctx, func(r server.RedisClient) {
			before := r.Get(ctx, key).Val()
			stale, err := beginClientScoreNativePublication(ctx, r, key, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			data := gobEncodeForTest(t, []*ClientScore{nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)})
			checksum := sha256.Sum256(data)
			page := clientScoreRedisSet{key: clientScoreNativePageField(ipFamilyFacetV4Only, 0), value: append(checksum[:sha256.Size:sha256.Size], data...)}
			if err := stale.writePages(ctx, r, []clientScoreRedisSet{page}); err != nil {
				t.Fatal(err)
			}
			if r.Get(ctx, key).Val() != before {
				t.Fatal("partial publication changed the active pointer")
			}
			nativeTestReadOne(t, location, first.ClientId)
			for range 5 {
				winner := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
				if err := writeClientScoreNativeSnapshot(ctx, r, key, time.Hour, nativeTestFacets(t, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {winner}})); err != nil {
					t.Fatal(err)
				}
				if err := stale.writePages(ctx, r, []clientScoreRedisSet{page}); err == nil {
					t.Fatal("stale writer modified a published slot")
				}
				nativeTestReadOne(t, location, winner.ClientId)
			}
			keys, err := r.Keys(ctx, key+"*").Result()
			if err != nil || len(keys) != 3 {
				t.Fatalf("native publication storage grew beyond pointer plus two slots: keys=%d err=%v", len(keys), err)
			}
		})
	})
}

// Missing and corrupted pages remain explicit read errors, never a successful
// empty native source. The selector separately owns degraded online fallback.
func TestNativePublicationRejectsMissingAndCorruptPages(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, missing := range []bool{false, true} {
			location := server.NewId()
			score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
			nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {score}})
			server.Redis(t.Context(), func(r server.RedisClient) {
				key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
				pointer, err := parseClientScoreNativePointer(r.Get(t.Context(), key).Val())
				if err != nil {
					t.Fatal(err)
				}
				slot, field := clientScoreNativeSlotKey(key, pointer.slot), clientScoreNativePageField(ipFamilyFacetV4Only, 0)
				if missing {
					server.Raise(r.HDel(t.Context(), slot, field).Err())
				} else {
					server.Raise(r.HSet(t.Context(), slot, field, "synthetic-corrupted-page").Err())
				}
			})
			scores, _, _, err := loadNativeClientScoresWithCursor(t.Context(), RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, 20, []ipFamilyFacet{ipFamilyFacetV4Only}, nil)
			if !errors.Is(err, errClientScoreNativeUnavailable) || len(scores) != 0 {
				t.Fatalf("incomplete native source became an empty success: missing=%t count=%d err=%v", missing, len(scores), err)
			}
		}
	})
}

// Cancellation after staging begins leaves the prior active generation intact.
func TestNativePublicationCancellationRetainsLastGood(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		location := server.NewId()
		score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {score}})
		server.Redis(t.Context(), func(r server.RedisClient) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
			publication, err := beginClientScoreNativePublication(ctx, r, key, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			manifest := clientScoreNativeManifest{Generation: publication.generation, Facets: map[ipFamilyFacet][]clientScoreNativePageInfo{}}
			for _, facet := range ipFamilyFacets {
				manifest.Facets[facet] = []clientScoreNativePageInfo{}
			}
			cancel()
			if err := publication.commit(ctx, r, manifest); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled publication committed: %v", err)
			}
		})
		nativeTestReadOne(t, location, score.ClientId)
	})
}
