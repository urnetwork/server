package model

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The score pages the export last published for a target: every union and
// facet sample of every caller, and every page of the native snapshots the
// pointers name, by key.
func clientScorePagesForTest(t testing.TB, locationId server.Id) map[string][]byte {
	t.Helper()
	ctx := t.Context()
	pages := map[string][]byte{}
	server.Redis(ctx, func(r server.RedisClient) {
		sampleKeys, err := r.Keys(ctx, fmt.Sprintf("{cs_*_%s}s_*", locationId)).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range sampleKeys {
			pages[key] = []byte(r.Get(ctx, key).Val())
		}
		pointerKeys, err := r.Keys(ctx, fmt.Sprintf("{cs_*_%s}c_l:native*", locationId)).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, pointerKey := range pointerKeys {
			if strings.HasSuffix(pointerKey, ":0") || strings.HasSuffix(pointerKey, ":1") {
				continue
			}
			value := r.Get(ctx, pointerKey).Val()
			if value == clientScoreNativeBaseline {
				continue
			}
			pointer, err := parseClientScoreNativePointer(value)
			if err != nil {
				t.Fatal(err)
			}
			slotKey := clientScoreNativeSlotKey(pointerKey, pointer.slot)
			for field, page := range r.HGetAll(ctx, slotKey).Val() {
				if field == "g" || field == "m" || strings.HasPrefix(field, "#") {
					continue
				}
				if len(page) < sha256.Size || sha256.Sum256([]byte(page[sha256.Size:])) != [sha256.Size]byte([]byte(page[:sha256.Size])) {
					t.Fatalf("native page %s %s fails its checksum", slotKey, field)
				}
				pages[slotKey+" "+field] = []byte(page[sha256.Size:])
			}
		}
	})
	return pages
}

// One real export over the database writes gob score pages, union and
// native, while typed_score_pages is off, as by default, and every one reads
// with a reader from before CPUOPT8. Set, the next export writes every page
// typed, which such a reader refuses, and the readers load the same providers
// from them.
func TestUpdateClientScoresWritesTypedPagesOnlyWithTypedScorePages(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)
		clientIds := testing_connectQualifyingProviders(ctx, t, city, 3)
		slices.SortFunc(clientIds, server.Id.Cmp)

		loadedClientIds := func() []server.Id {
			scores, err := loadClientScores(true, RankModeQuality, ctx, map[server.Id]bool{city.LocationId: true}, map[server.Id]bool{}, server.Id{}, 100, []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only})
			if err != nil {
				t.Fatal(err)
			}
			loaded := slices.Collect(maps.Keys(scores))
			slices.SortFunc(loaded, server.Id.Cmp)
			return loaded
		}

		for _, c := range []struct {
			config string
			typed  bool
		}{
			{config: "enable_egress_test: false\n", typed: false},
			{config: "enable_egress_test: false\negress_index:\n  typed_score_pages: true\n", typed: true},
		} {
			// only this export's documents: the first export also writes
			// every caller's duplicate payloads, which outlive it
			server.Redis(ctx, func(r server.RedisClient) {
				keys, err := r.Keys(ctx, fmt.Sprintf("{cs_*_%s}*", city.LocationId)).Result()
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range keys {
					server.Raise(r.Del(ctx, key).Err())
				}
			})
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(c.config))
			connect.AssertEqual(t, UpdateClientScores(ctx, time.Hour, 1), nil)
			pop()

			pages := clientScorePagesForTest(t, city.LocationId)
			providerPages := 0
			for key, page := range pages {
				if isTypedClientScorePage(page) != c.typed {
					t.Fatalf("typed=%t: %s is typed=%t", c.typed, key, isTypedClientScorePage(page))
				}
				oldSample, oldErr := clientScorePageOldReaderDecode(page)
				sample, err := decodeClientScoreSample(page)
				if err != nil {
					t.Fatalf("typed=%t: %s does not decode: %v", c.typed, key, err)
				}
				if c.typed {
					if oldErr == nil {
						t.Fatalf("a reader before CPUOPT8 decoded the typed page %s", key)
					}
				} else if oldErr != nil || !reflect.DeepEqual(oldSample, sample) {
					t.Fatalf("a reader before CPUOPT8 cannot read the gob page %s: %v", key, oldErr)
				}
				if 0 < len(sample) {
					providerPages += 1
				}
			}
			if providerPages == 0 {
				t.Fatalf("typed=%t: the export published no page with providers", c.typed)
			}
			if loaded := loadedClientIds(); !reflect.DeepEqual(loaded, clientIds) {
				t.Fatalf("typed=%t: loaded %v, want %v", c.typed, loaded, clientIds)
			}
		}
	})
}

// TestClientScorePageCacheReadersCannotChangeCachedPages over typed pages,
// whose equal maps are one map: after discovery in every mode that changes
// scores, beside a gob online page of the same target, each cached page still
// equals a fresh decode of its bytes.
func TestClientScorePageCacheReadersCannotChangeCachedTypedPages(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		cache := withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		location := server.NewId()
		scores := map[ipFamilyFacet][]*ClientScore{}
		for i := range 60 {
			mode := RankModeQuality
			if i%2 == 1 {
				mode = RankModeSpeed
			}
			score := nativeTestScore(mode, ipFamilyFacetV4Only)
			// equal maps across scores, so a typed page shares them
			score.ScaledWeights = map[string]float32{RankModeQuality: 0.25, RankModeSpeed: 0.75}
			if i%3 == 0 {
				expired := server.NowUtc().Add(-time.Minute)
				score.EgressValidUntil = &expired
			}
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
		}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocationInFormat(t, location, mode, scores, clientScorePageFormatTyped)
		}
		writeOnlineBackfillSample(ctx, t, location, RankModeQuality, true, scores[ipFamilyFacetV4Only])
		intermediary := scores[ipFamilyFacetV4Only][2].ClientId
		for _, args := range []*FindProviders2Args{
			{RankMode: RankModeQuality},
			{RankMode: RankModeSpeed},
			{RankMode: RankModeOnline},
			{RankMode: RankModeQuality, ForceMinimum: true},
			{RankMode: RankModeQuality, ExcludeDestinations: [][]server.Id{{intermediary, server.NewId()}}},
		} {
			args.Specs = []*ProviderSpec{{LocationId: &location}}
			if result, err := FindProviders2(args, clientScorePageCacheTestSession(t)); err != nil || len(result.Providers) == 0 {
				t.Fatalf("rank=%s force=%t: result=%v err=%v", args.RankMode, args.ForceMinimum, result, err)
			}
		}
		loaded, err := loadClientScores(false, RankModeQuality, ctx, map[server.Id]bool{location: true}, nil, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetV4Only})
		if err != nil || len(loaded) == 0 {
			t.Fatalf("union read count=%d err=%v", len(loaded), err)
		}
		for _, score := range loaded {
			*score = ClientScore{}
		}

		payloads := clientScorePagePayloadsForTest(t, location)
		cache.lock.Lock()
		defer cache.lock.Unlock()
		typedPages := 0
		for checksum, element := range cache.entries {
			payload, ok := payloads[checksum]
			if !ok {
				t.Fatal("a cached page has no bytes in redis")
			}
			if isTypedClientScorePage(payload) {
				typedPages += 1
			}
			fresh, err := decodeClientScoreSample(payload)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(element.Value.(*clientScorePageCacheItem).sample, fresh) {
				t.Fatal("a reader changed a cached page")
			}
		}
		if len(cache.entries) < 4 || typedPages < 2 {
			t.Fatalf("only %d pages cached, %d of them typed", len(cache.entries), typedPages)
		}
	})
}

// The same scores published as gob pages, then as typed pages, load as equal
// scores and give the same answer in the same family and tier bands. A
// request over a gob target and a typed target reads both.
func TestClientScorePageCacheTypedPagesKeepLoadedScoresAndAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		scoresOf := func() map[ipFamilyFacet][]*ClientScore {
			scores := map[ipFamilyFacet][]*ClientScore{}
			for i := range 20 {
				facet := ipFamilyFacetV4Only
				if i%4 == 0 {
					facet = ipFamilyFacetDualstack
				}
				score := nativeTestScore(RankModeQuality, facet)
				score.Scores = map[string]int{RankModeQuality: 20 * (i % 3), RankModeSpeed: 20 * (2 - i%3)}
				score.Tiers = map[string]int{RankModeQuality: i % 3, RankModeSpeed: 2 - i%3}
				score.ScaledWeights = map[string]float32{RankModeQuality: float32(i+1) / 20}
				score.MaxBytesPerSecond = ByteCount(i) * Mib
				score.HasSpeedTest = i%2 == 0
				score.ReputationFailedNames = fmt.Sprintf("vendor-%d", i%5)
				scores[facet] = append(scores[facet], score)
			}
			return scores
		}
		load := func(locations map[server.Id]bool, n int) map[server.Id]*ClientScore {
			loaded, _, err := loadPreferredClientScoresWithPartition(true, false, RankModeQuality, ctx, locations, nil, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, clientScorePartitionOff)
			if err != nil || len(loaded) != n {
				t.Fatalf("load count=%d err=%v, want %d", len(loaded), err, n)
			}
			return loaded
		}
		find := func(location server.Id) (map[server.Id]FindProvidersProvider, []string) {
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}, Count: 20, ForceCount: true}, clientScorePageCacheTestSession(t))
			if err != nil || result == nil || len(result.Providers) != 20 {
				t.Fatalf("result=%v err=%v", result, err)
			}
			answer := map[server.Id]FindProvidersProvider{}
			bands := []string{}
			for _, provider := range result.Providers {
				answer[provider.ClientId] = *provider
				bands = append(bands, fmt.Sprintf("%s/%d", provider.IpFamily, provider.Tier))
			}
			return answer, bands
		}

		location := server.NewId()
		scores := scoresOf()
		nativeTestPublishLocationInFormat(t, location, RankModeQuality, scores, clientScorePageFormatGob)
		gobScores := load(map[server.Id]bool{location: true}, 20)
		gobAnswer, gobBands := find(location)
		nativeTestPublishLocationInFormat(t, location, RankModeQuality, scores, clientScorePageFormatTyped)
		counter := newClientScorePageCacheCounter()
		if typedScores := load(map[server.Id]bool{location: true}, 20); !reflect.DeepEqual(typedScores, gobScores) {
			t.Fatal("typed pages loaded other scores than gob pages")
		}
		if _, misses := counter.next(); misses == 0 {
			t.Fatal("the typed pages were not read")
		}
		if typedAnswer, typedBands := find(location); !reflect.DeepEqual(typedAnswer, gobAnswer) || !reflect.DeepEqual(typedBands, gobBands) {
			t.Fatalf("typed pages changed the answer: %v, gob %v", typedBands, gobBands)
		}

		// one target of each format, in one read
		gobLocation, typedLocation := server.NewId(), server.NewId()
		nativeTestPublishLocationInFormat(t, gobLocation, RankModeQuality, scoresOf(), clientScorePageFormatGob)
		nativeTestPublishLocationInFormat(t, typedLocation, RankModeQuality, scoresOf(), clientScorePageFormatTyped)
		load(map[server.Id]bool{gobLocation: true, typedLocation: true}, 40)
	})
}
