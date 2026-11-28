package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// Counts the native page reads of one target's slots and passes every command
// through: a body read is an HMGET of the slot's generation and a page field,
// a generation read an HMGET of the generation alone. The manifest read
// (generation and "m") is neither. It counts only while active.
type nativePageReadCounterHook struct {
	active          atomic.Bool
	slotKeyPrefix   string
	bodyReads       atomic.Int64
	generationReads atomic.Int64
}

func (self *nativePageReadCounterHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *nativePageReadCounterHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		self.count(command)
		return next(ctx, command)
	}
}

func (self *nativePageReadCounterHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		for _, command := range commands {
			self.count(command)
		}
		return next(ctx, commands)
	}
}

func (self *nativePageReadCounterHook) count(command redis.Cmder) {
	if !self.active.Load() || command.Name() != "hmget" {
		return
	}
	args := command.Args()
	if len(args) < 3 || args[2] != "g" {
		return
	}
	if key, _ := args[1].(string); !strings.HasPrefix(key, self.slotKeyPrefix) {
		return
	}
	switch {
	case len(args) == 3:
		self.generationReads.Add(1)
	case len(args) == 4 && args[3] != "m":
		self.bodyReads.Add(1)
	}
}

// The body and generation reads since the previous call.
func (self *nativePageReadCounterHook) next() (bodyReads int64, generationReads int64) {
	return self.bodyReads.Swap(0), self.generationReads.Swap(0)
}

// Attaches a page read counter for one location's native Quality slots to the
// test process's Redis client, active.
func nativePageReadCounterForTest(t testing.TB, location server.Id) *nativePageReadCounterHook {
	t.Helper()
	key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
	hook := &nativePageReadCounterHook{slotKeyPrefix: key + ":"}
	server.Redis(t.Context(), func(r server.RedisClient) {
		r.AddHook(hook)
	})
	hook.active.Store(true)
	t.Cleanup(func() {
		hook.active.Store(false)
	})
	return hook
}

// The location's native Quality pointer key, its pointer and the slot it names.
func nativePageTestSlot(t testing.TB, location server.Id) (key string, rawPointer string, slotKey string) {
	t.Helper()
	ctx := t.Context()
	key = clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
	server.Redis(ctx, func(r server.RedisClient) {
		rawPointer = r.Get(ctx, key).Val()
		pointer, err := parseClientScoreNativePointer(rawPointer)
		if err != nil {
			t.Fatal(err)
		}
		slotKey = clientScoreNativeSlotKey(key, pointer.slot)
	})
	return
}

// Reads the location's native pages once, so the process holds their decodes.
func nativePageTestHold(t testing.TB, location server.Id, published map[server.Id]bool) {
	t.Helper()
	scores, available, err := nativeManifestTestLoad(t, location)
	if err != nil || !available || len(scores) != len(published) {
		t.Fatalf("published pages not read: count=%d available=%t err=%v", len(scores), available, err)
	}
}

// Expects the location's native read refused as unavailable, with no score.
func nativePageTestRefused(t testing.TB, location server.Id, why string) {
	t.Helper()
	scores, available, err := nativeManifestTestLoad(t, location)
	if !errors.Is(err, errClientScoreNativeUnavailable) || !available || len(scores) != 0 {
		t.Fatalf("%s was not refused: count=%d available=%t err=%v", why, len(scores), available, err)
	}
}

// Hosted discovery reads the same native pages request after request, and
// every read transferred and hashed each page again although the process held
// its decode. A page whose manifest checksum the process holds decoded is not
// read again: only its slot's generation is, once per slot, and the decode
// answers.
func TestClientScorePageCacheHitReadsOnlyTheSlotGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		location := server.NewId()
		published := nativeManifestTestPublish(t, location, 2*ClientScoreSampleCount+50)
		hook := nativePageReadCounterForTest(t, location)
		counter := newClientScorePageCacheCounter()
		for read := range 2 {
			scores, available, err := nativeManifestTestLoad(t, location)
			if err != nil || !available || len(scores) != len(published) {
				t.Fatalf("read %d: count=%d available=%t err=%v", read, len(scores), available, err)
			}
			for clientId := range scores {
				if !published[clientId] {
					t.Fatalf("read %d served a score it did not publish", read)
				}
			}
			bodyReads, generationReads := hook.next()
			hits, misses := counter.next()
			if read == 0 && (bodyReads != 3 || generationReads != 0 || misses != 3) {
				t.Fatalf("first read: body reads=%d generation reads=%d misses=%d, want 3, 0, 3", bodyReads, generationReads, misses)
			}
			if read == 1 && (bodyReads != 0 || generationReads != 1 || hits != 3 || misses != 0) {
				t.Fatalf("a read of held pages: body reads=%d generation reads=%d hits=%d misses=%d, want 0, 1, 3, 0", bodyReads, generationReads, hits, misses)
			}
		}
	})
}

// What a held decode is trusted for: the bytes of its page within the slot
// generation its manifest names, which no writer changes once the generation
// is published. A reader that holds the decode answers it without reading the
// bytes, so bytes written under that generation out of protocol never reach
// its answer; a reader without the decode reads them and refuses them.
func TestClientScorePageCacheHeldDecodeStandsInForItsGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		location := server.NewId()
		published := nativeManifestTestPublish(t, location, 5)
		nativePageTestHold(t, location, published)
		// another page's bytes under the page's recorded checksum and generation
		_, _, slotKey := nativePageTestSlot(t, location)
		server.Redis(ctx, func(r server.RedisClient) {
			field := clientScoreNativePageField(ipFamilyFacetV4Only, 0)
			recorded := r.HGet(ctx, slotKey, field).Val()[:sha256.Size]
			other := gobEncodeForTest(t, []*ClientScore{nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)})
			server.Raise(r.HSet(ctx, slotKey, field, recorded+string(other)).Err())
		})
		hook := nativePageReadCounterForTest(t, location)
		scores, available, err := nativeManifestTestLoad(t, location)
		if err != nil || !available || len(scores) != len(published) {
			t.Fatalf("the held decode did not answer its generation: count=%d available=%t err=%v", len(scores), available, err)
		}
		for clientId := range scores {
			if !published[clientId] {
				t.Fatal("a reader holding the decode served bytes it did not verify")
			}
		}
		if bodyReads, generationReads := hook.next(); bodyReads != 0 || generationReads != 1 {
			t.Fatalf("held page: body reads=%d generation reads=%d, want 0, 1", bodyReads, generationReads)
		}
		// a reader without the decode reads the bytes, and refuses them
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		nativePageTestRefused(t, location, "bytes that are not the manifest's")
		if bodyReads, generationReads := hook.next(); bodyReads != 1 || generationReads != 0 {
			t.Fatalf("page not held: body reads=%d generation reads=%d, want 1, 0", bodyReads, generationReads)
		}
	})
}

// A reader that still holds a pointer whose slot another publication has
// reused since holds that pointer's decodes too. The slot's generation refuses
// them, without a read of the page.
func TestClientScorePageCacheHeldDecodeRefusesAReusedSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		location := server.NewId()
		published := nativeManifestTestPublish(t, location, 5)
		key, firstPointer, _ := nativePageTestSlot(t, location)
		nativePageTestHold(t, location, published)
		// the second publication takes the other slot, the third reuses the first's
		nativeManifestTestPublish(t, location, 5)
		nativeManifestTestPublish(t, location, 5)
		if _, thirdPointer, _ := nativePageTestSlot(t, location); thirdPointer[:2] != firstPointer[:2] || thirdPointer == firstPointer {
			t.Fatalf("fixture did not reuse the slot: %s after %s", thirdPointer, firstPointer)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, firstPointer, time.Hour).Err())
		})
		hook := nativePageReadCounterForTest(t, location)
		nativePageTestRefused(t, location, "a reused slot")
		if bodyReads, generationReads := hook.next(); bodyReads != 0 || generationReads != 1 {
			t.Fatalf("reused slot: body reads=%d generation reads=%d, want 0, 1", bodyReads, generationReads)
		}
	})
}

// An expired slot has no generation: a held decode of its pages is refused.
func TestClientScorePageCacheHeldDecodeRefusesAnExpiredSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		location := server.NewId()
		published := nativeManifestTestPublish(t, location, 5)
		_, _, slotKey := nativePageTestSlot(t, location)
		nativePageTestHold(t, location, published)
		// an expired slot reads as an absent key; the pointer is still read
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, slotKey).Err())
		})
		hook := nativePageReadCounterForTest(t, location)
		nativePageTestRefused(t, location, "an expired slot")
		if bodyReads, generationReads := hook.next(); bodyReads != 0 || generationReads != 1 {
			t.Fatalf("expired slot: body reads=%d generation reads=%d, want 0, 1", bodyReads, generationReads)
		}
	})
}

// A slot whose generation is no longer its manifest's refuses a held decode.
func TestClientScorePageCacheHeldDecodeRefusesAChangedGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		location := server.NewId()
		published := nativeManifestTestPublish(t, location, 5)
		_, _, slotKey := nativePageTestSlot(t, location)
		nativePageTestHold(t, location, published)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.HSet(ctx, slotKey, "g", server.NewId().String()).Err())
		})
		hook := nativePageReadCounterForTest(t, location)
		nativePageTestRefused(t, location, "a changed generation")
		if bodyReads, generationReads := hook.next(); bodyReads != 0 || generationReads != 1 {
			t.Fatalf("changed generation: body reads=%d generation reads=%d, want 0, 1", bodyReads, generationReads)
		}
	})
}

// A manifest that names another checksum for a page names a page this process
// does not hold: the page is read again and verified, and refused when its
// bytes are not that checksum's. A republication, whose pages all have new
// checksums, has every page read again.
func TestClientScorePageCacheChangedManifestChecksumReadsThePage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
		ctx := t.Context()
		location := server.NewId()
		published := nativeManifestTestPublish(t, location, 5)
		key, rawPointer, slotKey := nativePageTestSlot(t, location)
		nativePageTestHold(t, location, published)
		// the slot's manifest and its pointer name another checksum for the page
		server.Redis(ctx, func(r server.RedisClient) {
			pointer, err := parseClientScoreNativePointer(rawPointer)
			if err != nil {
				t.Fatal(err)
			}
			manifest, _, err := readClientScoreNativeManifest(ctx, r, key, rawPointer)
			if err != nil {
				t.Fatal(err)
			}
			// a copy: a manifest read may be the process's shared one
			changed := *manifest
			changed.ModernPages = maps.Clone(manifest.ModernPages)
			changed.Facets = map[ipFamilyFacet][]clientScoreNativePageInfo{}
			for facet, pages := range manifest.Facets {
				changed.Facets[facet] = slices.Clone(pages)
			}
			changed.Facets[ipFamilyFacetV4Only][0].Checksum = sha256.Sum256([]byte("synthetic-changed-page"))
			wire := gobEncodeForTest(t, changed)
			server.Raise(r.HSet(ctx, slotKey, "m", wire).Err())
			server.Raise(r.Set(ctx, key, fmt.Sprintf("%d:%s:%x", pointer.slot, pointer.generation, sha256.Sum256(wire)), time.Hour).Err())
		})
		hook := nativePageReadCounterForTest(t, location)
		nativePageTestRefused(t, location, "a page whose bytes are not its manifest checksum's")
		if bodyReads, generationReads := hook.next(); bodyReads != 1 || generationReads != 0 {
			t.Fatalf("changed checksum: body reads=%d generation reads=%d, want 1, 0", bodyReads, generationReads)
		}

		// a republication with new pages
		republished := nativeManifestTestPublish(t, location, 2*ClientScoreSampleCount+50)
		scores, available, err := nativeManifestTestLoad(t, location)
		if err != nil || !available || len(scores) != len(republished) {
			t.Fatalf("republication: count=%d available=%t err=%v", len(scores), available, err)
		}
		for clientId := range scores {
			if !republished[clientId] {
				t.Fatal("republication served an earlier publication's score")
			}
		}
		if bodyReads, generationReads := hook.next(); bodyReads != 3 || generationReads != 0 {
			t.Fatalf("republication: body reads=%d generation reads=%d, want 3, 0", bodyReads, generationReads)
		}
	})
}
