package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Swaps in a cache of maxBytes for the test. Model tests run one at a time.
func withClientScorePageCacheForTest(t testing.TB, maxBytes int64) *clientScorePageCache {
	t.Helper()
	cache := newClientScorePageCache(maxBytes)
	previous := clientScorePages
	clientScorePages = cache
	t.Cleanup(func() {
		clientScorePages = previous
	})
	return cache
}

// The cache hits and page decodes since the previous call.
type clientScorePageCacheCounter struct {
	hits   float64
	misses float64
}

func newClientScorePageCacheCounter() *clientScorePageCacheCounter {
	counter := &clientScorePageCacheCounter{}
	counter.next()
	return counter
}

func (self *clientScorePageCacheCounter) next() (hits int, misses int) {
	allHits, allMisses := testutil.ToFloat64(clientScorePageCacheHits), testutil.ToFloat64(clientScorePageCacheMisses)
	hits, misses = int(allHits-self.hits), int(allMisses-self.misses)
	self.hits, self.misses = allHits, allMisses
	return
}

func clientScorePageCacheTestSession(t testing.TB) *session.ClientSession {
	return testingCreateProviderSearchSession(t.Context(), session.NewByJwt(server.NewId(), server.NewId(), "page-cache-test", false, false))
}

// Every score page payload of the target in redis, by the sha256 the cache
// keys it by: a union value whole, a native page field without its checksum.
func clientScorePagePayloadsForTest(t testing.TB, locationId server.Id) map[[sha256.Size]byte][]byte {
	t.Helper()
	ctx := t.Context()
	payloads := map[[sha256.Size]byte][]byte{}
	server.Redis(ctx, func(r server.RedisClient) {
		keys, err := r.Keys(ctx, fmt.Sprintf("{cs_*_%s}*", locationId)).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			values := []string{}
			switch r.Type(ctx, key).Val() {
			case "string":
				values = append(values, r.Get(ctx, key).Val())
			case "hash":
				for _, value := range r.HGetAll(ctx, key).Val() {
					if sha256.Size < len(value) && sha256.Sum256([]byte(value[sha256.Size:])) == [sha256.Size]byte([]byte(value[:sha256.Size])) {
						values = append(values, value[sha256.Size:])
					}
				}
			}
			for _, value := range values {
				payloads[sha256.Sum256([]byte(value))] = []byte(value)
			}
		}
	})
	return payloads
}

// The cache keeps the most recently used pages within its byte bound, never a
// page larger than the bound, and the first decode of a page.
func TestClientScorePageCacheBoundsBytesLeastRecentlyUsed(t *testing.T) {
	cache := newClientScorePageCache(300)
	checksums := [][sha256.Size]byte{}
	samples := [][]*ClientScore{}
	for i := range 5 {
		checksums = append(checksums, sha256.Sum256([]byte{byte(i)}))
		samples = append(samples, []*ClientScore{{ClientId: server.NewId()}})
	}
	for i := range 3 {
		cache.put(checksums[i], samples[i], 100)
	}
	if sample, ok := cache.get(checksums[0]); !ok || sample[0] != samples[0][0] {
		t.Fatal("a retained page was not found")
	}
	evictions := testutil.ToFloat64(clientScorePageCacheEvictions)
	// page 1 is now the least recently used
	cache.put(checksums[3], samples[3], 100)
	if _, ok := cache.get(checksums[1]); ok {
		t.Fatal("the least recently used page outlived the bound")
	}
	for _, i := range []int{0, 2, 3} {
		if _, ok := cache.get(checksums[i]); !ok {
			t.Fatalf("page %d was evicted before the least recently used", i)
		}
	}
	if pages, bytes := cache.size(); pages != 3 || bytes != 300 || testutil.ToFloat64(clientScorePageCacheEvictions)-evictions != 1 {
		t.Fatalf("bound not kept: pages=%d bytes=%d", pages, bytes)
	}
	cache.put(checksums[4], samples[4], 301)
	if _, ok := cache.get(checksums[4]); ok {
		t.Fatal("a page larger than the bound was retained")
	}
	// another reader's decode of the same bytes keeps the first
	cache.put(checksums[0], samples[4], 100)
	if sample, _ := cache.get(checksums[0]); sample[0] != samples[0][0] {
		t.Fatal("a later decode replaced a retained page")
	}
	if pages, bytes := cache.size(); pages != 3 || bytes != 300 {
		t.Fatalf("bound not kept: pages=%d bytes=%d", pages, bytes)
	}
}

// Concurrent readers of the same and different pages, with evictions, each
// get the page's scores, and what they do to their copies stays theirs.
func TestClientScorePageCacheConcurrentReaders(t *testing.T) {
	type page struct {
		data      []byte
		checksum  [sha256.Size]byte
		clientIds []server.Id
	}
	pages := []page{}
	for range 3 {
		sample := []*ClientScore{}
		clientIds := []server.Id{}
		for range 10 {
			score := &ClientScore{ClientId: server.NewId(), ScaledWeights: map[string]float32{RankModeQuality: 0.5}}
			sample = append(sample, score)
			clientIds = append(clientIds, score.ClientId)
		}
		data := gobEncodeForTest(t, sample)
		pages = append(pages, page{data: data, checksum: sha256.Sum256(data), clientIds: clientIds})
	}
	// room for any two of the three pages
	maxBytes := int64(-1)
	for _, page := range pages {
		maxBytes += clientScorePageDecodedBytesPerByte * int64(len(page.data))
	}
	cache := withClientScorePageCacheForTest(t, maxBytes)
	var wg sync.WaitGroup
	for reader := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				page := pages[(reader+i)%len(pages)]
				scores, err := decodeClientScorePage(page.data, page.checksum)
				if err != nil || len(scores) != len(page.clientIds) {
					t.Errorf("read count=%d err=%v", len(scores), err)
					return
				}
				for j, score := range scores {
					if score.ClientId != page.clientIds[j] || score.ScaledWeights[RankModeQuality] != 0.5 {
						t.Error("a reader got another page's or a changed score")
						return
					}
					score.Online = true
					score.ScaledWeights = map[string]float32{RankModeQuality: 1}
				}
			}
		}()
	}
	wg.Wait()
	if cached, bytes := cache.size(); cached != 2 || maxBytes < bytes {
		t.Fatalf("bound not kept: pages=%d bytes=%d", cached, bytes)
	}
}

// Hosted discovery reads the same pages call after call. A second call over
// unchanged pages decodes none of them.
func TestClientScorePageCacheSecondFindProviders2DecodesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		location := server.NewId()
		scores := map[ipFamilyFacet][]*ClientScore{}
		for range 2*ClientScoreSampleCount + 50 {
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], nativeTestScore(RankModeQuality, ipFamilyFacetV4Only))
		}
		nativeTestPublishLocation(t, location, RankModeQuality, scores)
		counter := newClientScorePageCacheCounter()
		for call := range 2 {
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}}, clientScorePageCacheTestSession(t))
			if err != nil || result == nil || len(result.Providers) != 20 {
				t.Fatalf("call %d: result=%v err=%v", call, result, err)
			}
			hits, misses := counter.next()
			if call == 0 && misses == 0 {
				t.Fatal("the first call decoded no page")
			}
			if call == 1 && (misses != 0 || hits == 0) {
				t.Fatalf("the second call over unchanged pages decoded %d pages (hits=%d)", misses, hits)
			}
		}
		loaded, _, err := loadPreferredClientScoresWithPartition(true, false, RankModeQuality, t.Context(), map[server.Id]bool{location: true}, nil, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, clientScorePartitionOff)
		if hits, misses := counter.next(); err != nil || len(loaded) != len(scores[ipFamilyFacetV4Only]) || hits != 3 || misses != 0 {
			t.Fatalf("native pages read again: count=%d hits=%d misses=%d err=%v", len(loaded), hits, misses, err)
		}
	})
}

// A republished page is new bytes: a new generation in the native slot it
// shares with an older one, or a rewritten union key. Each read decodes the
// bytes it read, never an earlier page that had the same place.
func TestClientScorePageCacheDecodesChangedPages(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		location := server.NewId()
		facets := []ipFamilyFacet{ipFamilyFacetV4Only}
		counter := newClientScorePageCacheCounter()
		// the third publication reuses the first's native slot and fields
		for publication := range 3 {
			scores := map[ipFamilyFacet][]*ClientScore{}
			published := map[server.Id]bool{}
			for range ClientScoreSampleCount + 50 {
				score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
				scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
				published[score.ClientId] = true
			}
			nativeTestPublishLocation(t, location, RankModeQuality, scores)
			native, _, available, err := loadNativeClientScoresWithCursor(ctx, RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, 1000, facets, nil)
			if err != nil || !available {
				t.Fatalf("publication %d: native read available=%t err=%v", publication, available, err)
			}
			union, err := loadClientScores(false, RankModeQuality, ctx, map[server.Id]bool{location: true}, nil, server.Id{}, 1000, facets)
			if err != nil {
				t.Fatalf("publication %d: union read err=%v", publication, err)
			}
			for source, loaded := range map[string]map[server.Id]*ClientScore{"native": native, "union": union} {
				if len(loaded) != len(published) {
					t.Fatalf("publication %d: %s read %d scores, want %d", publication, source, len(loaded), len(published))
				}
				for clientId := range loaded {
					if !published[clientId] {
						t.Fatalf("publication %d: %s read served an earlier publication's page", publication, source)
					}
				}
			}
			if hits, misses := counter.next(); hits != 0 || misses != 4 {
				t.Fatalf("publication %d: hits=%d misses=%d, want each changed page decoded", publication, hits, misses)
			}
		}
	})
}

// A cached decode never stands in for page bytes that fail their checksum.
func TestClientScorePageCacheVerifiesPageBytesBeforeAHit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		location := server.NewId()
		score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		nativeTestPublishLocation(t, location, RankModeQuality, map[ipFamilyFacet][]*ClientScore{ipFamilyFacetV4Only: {score}})
		read := func() (map[server.Id]*ClientScore, error) {
			scores, _, _, err := loadNativeClientScoresWithCursor(ctx, RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, 20, []ipFamilyFacet{ipFamilyFacetV4Only}, nil)
			return scores, err
		}
		if scores, err := read(); err != nil || len(scores) != 1 || scores[score.ClientId] == nil {
			t.Fatalf("published page not read: count=%d err=%v", len(scores), err)
		}
		// another page's bytes under the cached page's recorded checksum
		server.Redis(ctx, func(r server.RedisClient) {
			key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
			pointer, err := parseClientScoreNativePointer(r.Get(ctx, key).Val())
			if err != nil {
				t.Fatal(err)
			}
			slot, field := clientScoreNativeSlotKey(key, pointer.slot), clientScoreNativePageField(ipFamilyFacetV4Only, 0)
			recorded := r.HGet(ctx, slot, field).Val()[:sha256.Size]
			other := gobEncodeForTest(t, []*ClientScore{nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)})
			server.Raise(r.HSet(ctx, slot, field, recorded+string(other)).Err())
		})
		if scores, err := read(); !errors.Is(err, errClientScoreNativeUnavailable) || len(scores) != 0 {
			t.Fatalf("a cached decode stood in for changed page bytes: count=%d err=%v", len(scores), err)
		}
	})
}

// Readers work on their own copies. After discovery in every mode that
// changes scores (expired evidence, Online, force_minimum, intermediary
// weights) and a reader that clears every field of its copies, each cached
// page still equals a fresh decode of its bytes in redis.
func TestClientScorePageCacheReadersCannotChangeCachedPages(t *testing.T) {
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
			// weights every changing reader sets or scales
			score.ScaledWeights = map[string]float32{RankModeQuality: 0.25, RankModeSpeed: 0.75}
			if i%3 == 0 {
				expired := server.NowUtc().Add(-time.Minute)
				score.EgressValidUntil = &expired
			}
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
		}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			nativeTestPublishLocation(t, location, mode, scores)
		}
		writeOnlineBackfillSample(ctx, t, location, RankModeQuality, true, scores[ipFamilyFacetV4Only])
		// a current Quality native
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
		if len(cache.entries) < 4 {
			t.Fatalf("only %d pages cached", len(cache.entries))
		}
		for checksum, element := range cache.entries {
			payload, ok := payloads[checksum]
			if !ok {
				t.Fatal("a cached page has no bytes in redis")
			}
			fresh, err := decodeClientScoreSample(payload)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(element.Value.(*clientScorePageCacheItem).sample, fresh) {
				t.Fatal("a reader changed a cached page")
			}
		}
	})
}

// The cache changes no answer: the same scores load with it off, on a miss
// and on a hit, and discovery answers the same providers in the same order of
// family and tier bands (the order within a band is a weighted draw).
func TestClientScorePageCacheKeepsLoadedScoresAndAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		nativeTestEnableReader(t)
		ctx := t.Context()
		location := server.NewId()
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
		nativeTestPublishLocation(t, location, RankModeQuality, scores)
		load := func() map[server.Id]*ClientScore {
			loaded, _, err := loadPreferredClientScoresWithPartition(true, false, RankModeQuality, ctx, map[server.Id]bool{location: true}, nil, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, clientScorePartitionOff)
			if err != nil || len(loaded) != 20 {
				t.Fatalf("load count=%d err=%v", len(loaded), err)
			}
			return loaded
		}
		find := func() (map[server.Id]FindProvidersProvider, []string) {
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

		withClientScorePageCacheForTest(t, 0)
		uncachedScores := load()
		uncachedAnswer, uncachedBands := find()
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		counter := newClientScorePageCacheCounter()
		for _, lookup := range []string{"miss", "hit"} {
			if !reflect.DeepEqual(load(), uncachedScores) {
				t.Fatalf("%s: the cache changed the loaded scores", lookup)
			}
			if answer, bands := find(); !reflect.DeepEqual(answer, uncachedAnswer) || !reflect.DeepEqual(bands, uncachedBands) {
				t.Fatalf("%s: the cache changed the answer: %v, uncached %v", lookup, bands, uncachedBands)
			}
			hits, misses := counter.next()
			if lookup == "miss" && misses == 0 || lookup == "hit" && (misses != 0 || hits == 0) {
				t.Fatalf("%s: hits=%d misses=%d", lookup, hits, misses)
			}
		}
	})
}
