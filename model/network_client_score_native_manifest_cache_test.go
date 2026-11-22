package model

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// Counts the manifest reads (HMGET of a slot's "m") made through it, and
// passes every command through unchanged. Used by one test goroutine.
type nativeManifestReadCounter struct {
	server.RedisClient
	manifestReads int
}

func (self *nativeManifestReadCounter) HMGet(ctx context.Context, key string, fields ...string) *redis.SliceCmd {
	if slices.Contains(fields, "m") {
		self.manifestReads += 1
	}
	return self.RedisClient.HMGet(ctx, key, fields...)
}

// The family snapshot's pages of one location's v4-only facet, read through a
// counter, and the manifest reads the read made.
func nativeManifestTestPages(t testing.TB, location server.Id) (clientScoreNativePages, int) {
	t.Helper()
	ctx := t.Context()
	var pages clientScoreNativePages
	manifestReads := 0
	server.Redis(ctx, func(r server.RedisClient) {
		counter := &nativeManifestReadCounter{RedisClient: r}
		var err error
		pages, err = clientScoreNativeTargetPages(ctx, counter, clientScoreNativeFamilySnapshot, RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, []ipFamilyFacet{ipFamilyFacetV4Only})
		if err != nil || !pages.available {
			t.Fatalf("native target pages not read: available=%t err=%v", pages.available, err)
		}
		manifestReads = counter.manifestReads
	})
	return pages, manifestReads
}

// The native v4-only scores of one location, and the publication's client ids.
func nativeManifestTestPublish(t testing.TB, location server.Id, count int) map[server.Id]bool {
	t.Helper()
	scores := map[ipFamilyFacet][]*ClientScore{}
	clientIds := map[server.Id]bool{}
	for range count {
		score := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
		clientIds[score.ClientId] = true
	}
	nativeTestPublishLocation(t, location, RankModeQuality, scores)
	return clientIds
}

func nativeManifestTestLoad(t testing.TB, location server.Id) (map[server.Id]*ClientScore, bool, error) {
	t.Helper()
	scores, _, available, err := loadNativeClientScoresWithCursor(t.Context(), RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetV4Only}, nil)
	return scores, available, err
}

// Every hosted window build and refill reads its pool's snapshots again, and
// each read of a snapshot read and verified its manifest again: one more
// sequential round trip per snapshot, two per selector-mode pool, on processes
// where every round trip waits for the scheduler. A pointer names its manifest
// by slot, generation and checksum, so a second read of an unchanged pointer
// reads no manifest and finds the same pages.
func TestNativeManifestReadOncePerPointer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		location := server.NewId()
		published := nativeManifestTestPublish(t, location, 2*ClientScoreSampleCount+50)

		first, firstReads := nativeManifestTestPages(t, location)
		second, secondReads := nativeManifestTestPages(t, location)
		if firstReads != 1 {
			t.Fatalf("the first read of the pointer made %d manifest reads, want 1", firstReads)
		}
		if secondReads != 0 {
			t.Fatalf("the second read of an unchanged pointer made %d manifest reads, want 0", secondReads)
		}
		if len(first.otherByFacet) != 1 || len(first.otherByFacet[0]) != 3 {
			t.Fatalf("fixture pages: %v", first.otherByFacet)
		}
		if !maps.EqualFunc(first.otherByFacet[0], second.otherByFacet[0], func(a clientScorePage, b clientScorePage) bool {
			return a.key == b.key && a.expectedCount == b.expectedCount && a.nativeKey == b.nativeKey &&
				a.nativeGeneration == b.nativeGeneration && a.nativeChecksum == b.nativeChecksum
		}) {
			t.Fatal("the cached manifest named other pages than the slot's manifest")
		}

		// the cached manifest's pages are read and verified as before
		scores, available, err := nativeManifestTestLoad(t, location)
		if err != nil || !available || len(scores) != len(published) {
			t.Fatalf("native read through the cached manifest: count=%d available=%t err=%v", len(scores), available, err)
		}
		for clientId := range scores {
			if !published[clientId] {
				t.Fatal("native read through the cached manifest served a score it did not publish")
			}
		}
	})
}

// A new publication is a new pointer: its manifest is read once, and only
// its own scores are served, also when it reuses an earlier publication's
// slot.
func TestNativeManifestRepublicationReadsTheNewManifest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		location := server.NewId()
		// the third publication reuses the first's slot
		for publication := range 3 {
			published := nativeManifestTestPublish(t, location, ClientScoreSampleCount+50)
			if _, manifestReads := nativeManifestTestPages(t, location); manifestReads != 1 {
				t.Fatalf("publication %d: %d manifest reads, want 1", publication, manifestReads)
			}
			if _, manifestReads := nativeManifestTestPages(t, location); manifestReads != 0 {
				t.Fatalf("publication %d: %d manifest reads of an unchanged pointer, want 0", publication, manifestReads)
			}
			scores, available, err := nativeManifestTestLoad(t, location)
			if err != nil || !available || len(scores) != len(published) {
				t.Fatalf("publication %d: count=%d available=%t err=%v", publication, len(scores), available, err)
			}
			for clientId := range scores {
				if !published[clientId] {
					t.Fatalf("publication %d: served an earlier publication's score", publication)
				}
			}
		}
	})
}

// A manifest that fails its checks is never kept: every read of its pointer
// reads it again and refuses it, until the slot holds the pointer's manifest.
func TestNativeManifestRefusedManifestIsNotKept(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		location := server.NewId()
		nativeManifestTestPublish(t, location, 5)
		key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
		var slotKey, wire string
		server.Redis(ctx, func(r server.RedisClient) {
			pointer, err := parseClientScoreNativePointer(r.Get(ctx, key).Val())
			if err != nil {
				t.Fatal(err)
			}
			slotKey = clientScoreNativeSlotKey(key, pointer.slot)
			wire = r.HGet(ctx, slotKey, "m").Val()
			server.Raise(r.HSet(ctx, slotKey, "m", wire+"synthetic-changed-manifest").Err())
		})
		for attempt := range 2 {
			server.Redis(ctx, func(r server.RedisClient) {
				counter := &nativeManifestReadCounter{RedisClient: r}
				_, err := clientScoreNativeTargetPages(ctx, counter, clientScoreNativeFamilySnapshot, RankModeQuality, map[server.Id]bool{location: true}, nil, server.Id{}, []ipFamilyFacet{ipFamilyFacetV4Only})
				if !errors.Is(err, errClientScoreNativeUnavailable) || counter.manifestReads != 1 {
					t.Fatalf("attempt %d: a changed manifest was kept or accepted: reads=%d err=%v", attempt, counter.manifestReads, err)
				}
			})
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.HSet(ctx, slotKey, "m", wire).Err())
		})
		if _, manifestReads := nativeManifestTestPages(t, location); manifestReads != 1 {
			t.Fatalf("the restored manifest: %d manifest reads, want 1", manifestReads)
		}
		if _, manifestReads := nativeManifestTestPages(t, location); manifestReads != 0 {
			t.Fatalf("the restored manifest read again: %d manifest reads, want 0", manifestReads)
		}
	})
}

// The cache keeps the most recently used manifests within its byte bound,
// never one larger than the bound, and the first one verified for a key.
func TestNativeManifestCacheBoundsBytesLeastRecentlyUsed(t *testing.T) {
	cache := newClientScoreNativeManifestCache(300)
	keys := []string{}
	manifests := []*clientScoreNativeManifest{}
	for i := range 5 {
		keys = append(keys, clientScoreNativeManifestCacheKey("{cs}:native_v1", string(rune('a'+i)), ipFamilyFacets))
		manifests = append(manifests, &clientScoreNativeManifest{Generation: server.NewId().String()})
	}
	for i := range 3 {
		cache.put(keys[i], manifests[i], 100)
	}
	if manifest, ok := cache.get(keys[0]); !ok || manifest != manifests[0] {
		t.Fatal("a retained manifest was not found")
	}
	// key 1 is now the least recently used
	cache.put(keys[3], manifests[3], 100)
	if _, ok := cache.get(keys[1]); ok {
		t.Fatal("the least recently used manifest outlived the bound")
	}
	for _, i := range []int{0, 2, 3} {
		if _, ok := cache.get(keys[i]); !ok {
			t.Fatalf("manifest %d was evicted before the least recently used", i)
		}
	}
	if count, bytes := cache.size(); count != 3 || bytes != 300 {
		t.Fatalf("bound not kept: manifests=%d bytes=%d", count, bytes)
	}
	cache.put(keys[4], manifests[4], 301)
	if _, ok := cache.get(keys[4]); ok {
		t.Fatal("a manifest larger than the bound was retained")
	}
	// another reader's verification of the same pointer keeps the first
	cache.put(keys[0], manifests[4], 100)
	if manifest, _ := cache.get(keys[0]); manifest != manifests[0] {
		t.Fatal("a second verification replaced the kept manifest")
	}
	// the same pointer read for other manifest facets is another entry
	if clientScoreNativeManifestCacheKey("{cs}:native_v1", "a", ipFamilyFacets) == clientScoreNativeManifestCacheKey("{cs}:native_v1", "a", clientScoreCapabilityFacets) {
		t.Fatal("manifest facets do not distinguish cache keys")
	}
	cache.clear()
	if count, bytes := cache.size(); count != 0 || bytes != 0 {
		t.Fatalf("clear kept manifests=%d bytes=%d", count, bytes)
	}
}

// A reader that still holds a pointer whose slot was reused since gets that
// pointer's manifest from memory, and its page read still finds the slot's new
// generation: the source is unavailable, never another publication's pages or
// a false exhaustion.
func TestNativeManifestCachedPointerStillRefusesReusedSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		location := server.NewId()
		nativeManifestTestPublish(t, location, 5)
		key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
		var firstPointer string
		server.Redis(ctx, func(r server.RedisClient) {
			firstPointer = r.Get(ctx, key).Val()
		})
		if _, manifestReads := nativeManifestTestPages(t, location); manifestReads != 1 {
			t.Fatalf("%d manifest reads, want 1", manifestReads)
		}
		// the second publication takes the other slot, the third reuses the first's
		nativeManifestTestPublish(t, location, 5)
		nativeManifestTestPublish(t, location, 5)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, firstPointer, time.Hour).Err())
		})
		scores, available, err := nativeManifestTestLoad(t, location)
		if !errors.Is(err, errClientScoreNativeUnavailable) || !available || len(scores) != 0 {
			t.Fatalf("a reused slot read through an earlier pointer: count=%d available=%t err=%v", len(scores), available, err)
		}
	})
}
