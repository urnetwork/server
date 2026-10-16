// Delayed and overlapping score exports retain cleanup metadata until every
// payload they can publish has expired, with no persistent Redis records.
package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Refuses only a final manifest publication, leaving ordinary payload writes
// and reads untouched. Each test owns its Redis pool and this hook.
type clientScoreManifestFailureHook struct {
	fail atomic.Bool
}

// Preserves the fixture's guarded dialer.
func (self *clientScoreManifestFailureHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

// The manifest is one direct script call, outside all payload pipelines.
func (self *clientScoreManifestFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		args := cmd.Args()
		if self.fail.Load() && len(args) > 1 && args[1] == clientScoreTargetManifestWriteScript {
			err := errors.New("test: final manifest write refused")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

// Payload pipelines retain their normal behavior.
func (self *clientScoreManifestFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Advances one queued command past its deadline without sleeping. The direct
// newer publication uses the caller's client before the pipeline acquires a
// connection; only pipeline commands are intercepted, so it cannot recurse.
type clientScoreQueuedExpiryHook struct {
	key           string
	expiresAt     time.Time
	expiredAt     time.Time
	beforeExecute func() error
	intercepted   bool
}

// Preserves the fixture's guarded dialer.
func (self *clientScoreQueuedExpiryHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

// Direct publication of the newer value and subsequent reads are unchanged.
func (self *clientScoreQueuedExpiryHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

// Translates only the queued deadline into the past, equivalent to Redis time
// advancing between the client's preflight check and the actual operation.
func (self *clientScoreQueuedExpiryHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			args := cmd.Args()
			expiryIndex := -1
			switch cmd.Name() {
			case "eval":
				if len(args) == 6 && args[3] == self.key {
					expiryIndex = 5
				}
			case "set":
				// Retain the actual pre-correction raw SET as a RED control.
				if len(args) == 5 && args[1] == self.key && args[3] == "PXAT" {
					expiryIndex = 4
				}
			}
			if expiryIndex < 0 {
				continue
			}
			if self.intercepted || args[expiryIndex] != self.expiresAt.UnixMilli() {
				return fmt.Errorf("test: unexpected queued export deadline %v", args[expiryIndex])
			}
			if err := self.beforeExecute(); err != nil {
				return err
			}
			self.intercepted = true
			args[expiryIndex] = self.expiredAt.UnixMilli()
		}
		return next(ctx, cmds)
	}
}

// A newer export publishes after an older command is queued. Expiring that
// queued command must refuse both union and native-alias writes without
// deleting or replacing the newer payload or changing its finite deadline.
func TestClientScoreExportExpiryQueuedWritePreservesNewerValue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		expiresAt := now.Add(time.Hour)
		newerExpiresAt := now.Add(2 * time.Hour)
		exportCtx := context.WithValue(ctx, clientScoreExportExpiryContextKey{}, expiresAt)
		keys := newClientScoreTargetKeys(false, RankModeQuality, clientScoreTarget{id: server.NewId()})
		unionKey := keys.counts(server.NewId())
		for _, key := range []string{unionKey, clientScoreNativeKey(unionKey)} {
			server.Redis(ctx, func(r server.RedisClient) {
				hook := &clientScoreQueuedExpiryHook{
					key:       key,
					expiresAt: expiresAt,
					expiredAt: now.Add(-time.Hour),
					beforeExecute: func() error {
						return r.Do(ctx, "SET", key, "newer-export", "PXAT", newerExpiresAt.UnixMilli()).Err()
					},
				}
				r.AddHook(hook)
				err := writeClientScoreRedisStream(exportCtx, r, time.Hour, func(emit func(clientScoreRedisSet) error) error {
					return emit(clientScoreRedisSet{key: key, value: []byte("expired-export")})
				})
				if !hook.intercepted {
					t.Fatalf("fixture: %s did not queue its absolute export deadline", key)
				}
				if value, readErr := r.Get(ctx, key).Result(); readErr != nil || value != "newer-export" {
					t.Fatalf("expired queued write changed the newer value at %s: %q, %v", key, value, readErr)
				}
				if expiry, readErr := r.PExpireTime(ctx, key).Result(); readErr != nil || expiry.Milliseconds() != newerExpiresAt.UnixMilli() {
					t.Fatalf("expired queued write changed the newer deadline at %s: %v, %v", key, expiry, readErr)
				}
				if err == nil || !strings.Contains(err.Error(), "client score export expired") {
					t.Fatalf("expired queued write returned %v, want Redis deadline refusal", err)
				}
			})
		}
	})
}

// Uses the real streaming writer, target fanout and native publisher. Only a
// final producer error is injected, after every preceding batch committed.
func publishClientScoreExpiryTargets(ctx context.Context, r server.RedisClient, ttl time.Duration, targets []clientScoreTarget, callers []server.Id, finishErr error) (map[string]bool, error) {
	keysWritten := map[string]bool{}
	err := writeClientScoreRedisStream(ctx, r, ttl, func(emit func(clientScoreRedisSet) error) error {
		record := func(set clientScoreRedisSet) error {
			if !set.flush {
				keysWritten[set.key] = true
			}
			return emit(set)
		}
		for _, target := range targets {
			for _, forceMinimum := range []bool{false, true} {
				for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
					keys := newClientScoreTargetKeys(forceMinimum, mode, target)
					var native *clientScoreNativeFanout
					if !forceMinimum {
						native = newClientScoreNativeFanout(ctx, r, keys.counts, ttl, nil, record)
						keysWritten[clientScoreNativeKey(keys.counts(server.Id{}))] = true
					}
					if err := emitClientScoreTargetFanout(callers, target.clientScores, nil, keys, func(scores map[server.Id]*ClientScore) clientScoreExportPayload {
						return exportClientScores(forceMinimum, mode, scores)
					}, false, false, record, native); err != nil {
						return err
					}
				}
			}
		}
		if err := emit(clientScoreRedisSet{flush: true}); err != nil {
			return err
		}
		return finishErr
	})
	return keysWritten, err
}

// The write-ahead record is four hours old before payload publication begins.
// No sleep is involved: the fixed deadline represents that delayed export.
// A failure after publication leaves every location/group/alias/native key
// bounded by the original manifest, rather than a fresh five-hour payload ttl.
func TestClientScoreExportExpiryBoundsDelayedPayloads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		ttl := clientScoreExportTestTtl
		startedAt := server.NowUtc().Truncate(time.Second).Add(-4*time.Hour + 750*time.Millisecond)
		expiresAt := startedAt.Add(ttl)
		exportCtx := context.WithValue(ctx, clientScoreExportExpiryContextKey{}, expiresAt)
		fixture := newClientScoreEmptyTargetFixture()
		for _, target := range fixture.targetMaps.targets() {
			for _, score := range target.clientScores {
				score.IpFamilies = ClientScoreIpFamilyV4
				score.PassesMinimums = map[string]bool{RankModeQuality: true, RankModeSpeed: true}
				score.ScaledWeights = map[string]float32{RankModeQuality: 1, RankModeSpeed: 1}
				score.ReliabilityWeight = 1
			}
		}
		_, writeAhead := fixture.targetMaps.departedTargets(nil, startedAt, ttl)
		if err := writeClientScoreTargetManifest(exportCtx, writeAhead, ttl); err != nil {
			t.Fatal(err)
		}
		lateFailure := errors.New("test: a later export batch failed")
		finalWriteHook := &clientScoreManifestFailureHook{}
		server.Redis(ctx, func(r server.RedisClient) {
			r.AddHook(finalWriteHook)
			keys, err := publishClientScoreExpiryTargets(exportCtx, r, ttl, fixture.targetMaps.targets(), fixture.clientLocationIds, lateFailure)
			if !errors.Is(err, lateFailure) {
				t.Fatalf("partial export returned %v, want its injected late failure", err)
			}
			keys[clientScoreTargetManifestKey] = true
			for key := range keys {
				expiry, err := r.PExpireTime(ctx, key).Result()
				if err != nil || expiry.Milliseconds() != expiresAt.UnixMilli() {
					t.Fatalf("delayed key %s expires at %d (%v), want write-ahead deadline %d", key, expiry.Milliseconds(), err, expiresAt.UnixMilli())
				}
			}
			// Native staging and active slots have the same deadline as their
			// pointer; check actual Lua publication, not just its arguments.
			for _, target := range fixture.targetMaps.targets() {
				for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
					keys := newClientScoreTargetKeys(false, mode, target)
					key := clientScoreNativeKey(keys.counts(server.Id{}))
					pointer, err := r.Get(ctx, key).Result()
					if err != nil {
						t.Fatal(err)
					}
					_, slotKey, err := readClientScoreNativeManifest(ctx, r, key, pointer)
					if err != nil {
						t.Fatal(err)
					}
					if expiry, err := r.PExpireTime(ctx, slotKey).Result(); err != nil || expiry.Milliseconds() != expiresAt.UnixMilli() {
						t.Fatalf("native slot outlives its manifest: %s, %v", expiry, err)
					}
				}
			}
		})
		finalWriteHook.fail.Store(true)
		finalErr := writeClientScoreTargetManifest(exportCtx, fixture.targetMaps.targetManifest(server.NowUtc()), ttl)
		finalWriteHook.fail.Store(false)
		if finalErr == nil {
			t.Fatal("fixture: final manifest publication did not fail")
		}
		previous, err := readClientScoreTargetManifest(ctx)
		if err != nil || previous == nil {
			t.Fatalf("failed export lost its write-ahead manifest: %v", err)
		}
		empty := newClientScoreTargetMaps(nil, nil, func(server.Id) bool { return false })
		before, _ := empty.departedTargets(previous, expiresAt.Add(-time.Millisecond), ttl)
		after, _ := empty.departedTargets(previous, expiresAt, ttl)
		if len(before) != len(fixture.targetMaps.targets()) || len(after) != 0 {
			t.Fatalf("expiry boundary remembers %d targets before and %d after", len(before), len(after))
		}
		// A retry before that deadline clears every departed target through
		// the real union/native publishers and caller alias readers.
		retryAt := server.NowUtc()
		retryCtx := context.WithValue(ctx, clientScoreExportExpiryContextKey{}, retryAt.Add(ttl))
		departed, retryManifest := empty.departedTargets(previous, retryAt, ttl)
		if err := writeClientScoreTargetManifest(retryCtx, retryManifest, ttl); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if _, err := publishClientScoreExpiryTargets(retryCtx, r, ttl, departed, fixture.clientLocationIds, nil); err != nil {
				t.Fatal(err)
			}
			for _, target := range departed {
				locations, groups := map[server.Id]bool{}, map[server.Id]bool{}
				if target.locationGroup {
					groups[target.id] = true
				} else {
					locations[target.id] = true
				}
				for _, caller := range fixture.clientLocationIds {
					for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
						locationReads, groupReads := newClientScoreTargetReads(false, mode, locations, groups, caller, ipFamilyFacets, func(key string) *redis.StringCmd { return r.Get(ctx, key) })
						counts, incomplete, err := clientScoreTargetPageCounts(false, mode, caller, ipFamilyFacets, locationReads, groupReads, &findProviders2LoadObservation{})
						pageCount := 0
						for _, group := range counts {
							pageCount += len(group)
						}
						if err != nil || incomplete || pageCount != 0 {
							t.Fatalf("departed union still serves pages: %v, %v", counts, err)
						}
						keys := newClientScoreTargetKeys(false, mode, target)
						key := clientScoreNativeKey(keys.counts(caller))
						pointer, err := r.Get(ctx, key).Result()
						if err != nil {
							t.Fatal(err)
						}
						if pointer == clientScoreNativeBaseline {
							key = clientScoreNativeKey(keys.counts(server.Id{}))
							pointer, err = r.Get(ctx, key).Result()
							if err != nil {
								t.Fatal(err)
							}
						}
						manifest, _, err := readClientScoreNativeManifest(ctx, r, key, pointer)
						if err != nil {
							t.Fatal(err)
						}
						for _, pages := range manifest.Facets {
							if len(pages) != 0 {
								t.Fatal("departed native target still serves providers")
							}
						}
					}
				}
			}
		})
	})
}

// Each export merges instead of overwriting another generation's targets or
// shortening its deadline. A final manifest failure has the same safe state
// as a crash immediately before that write: the write-ahead union remains.
func TestClientScoreTargetManifestPreservesOverlappingExpiryHorizons(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Millisecond)
		firstId, sharedId, secondId := server.NewId(), server.NewId(), server.NewId()
		longExpiry, shortExpiry := now.Add(2*time.Hour), now.Add(time.Hour)
		first := newClientScoreTargetManifest()
		first.locationIds[firstId], first.locationIds[sharedId] = now.Unix(), now.Unix()
		second := newClientScoreTargetManifest()
		second.locationIds[sharedId], second.locationGroupIds[secondId] = now.Unix(), now.Unix()
		firstCtx := context.WithValue(ctx, clientScoreExportExpiryContextKey{}, longExpiry)
		secondCtx := context.WithValue(ctx, clientScoreExportExpiryContextKey{}, shortExpiry)
		if err := writeClientScoreTargetManifest(firstCtx, first, 2*time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := writeClientScoreTargetManifest(secondCtx, second, time.Hour); err != nil {
			t.Fatal(err)
		}
		// An older exporter completes last with its smaller target set.
		if err := writeClientScoreTargetManifest(firstCtx, first, 2*time.Hour); err != nil {
			t.Fatal(err)
		}
		manifest, err := readClientScoreTargetManifest(ctx)
		if err != nil || manifest == nil {
			t.Fatalf("manifest missing: %v", err)
		}
		if !maps.Equal(manifest.locationExpireMillis, map[server.Id]int64{firstId: longExpiry.UnixMilli(), sharedId: longExpiry.UnixMilli()}) || !maps.Equal(manifest.locationGroupExpireMillis, map[server.Id]int64{secondId: shortExpiry.UnixMilli()}) {
			t.Fatalf("overlap lost a target or shortened its deadline: %+v", manifest)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if expiry, err := r.PExpireTime(ctx, clientScoreTargetManifestKey).Result(); err != nil || expiry.Milliseconds() != longExpiry.UnixMilli() {
				t.Fatalf("manifest expiry %s (%v), want longest retained deadline", expiry, err)
			}
			// Advance the manifest's pruning clock explicitly. Expired targets
			// disappear without waiting for wall-clock time or another export.
			value, err := encodeClientScoreTargetManifest(newClientScoreTargetManifest())
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Eval(ctx, clientScoreTargetManifestWriteScript, []string{clientScoreTargetManifestKey}, value, longExpiry.UnixMilli(), time.Hour.Milliseconds(), 0).Err(); err != nil {
				t.Fatal(err)
			}
			if count, err := r.Exists(ctx, clientScoreTargetManifestKey).Result(); err != nil || count != 0 {
				t.Fatalf("expired manifest retained Redis residue: %d, %v", count, err)
			}
		})
	})
}

// Expired contexts and queued native operations refuse publication, while
// direct helper callers retain their ordinary positive relative ttl.
func TestClientScoreExportExpiryRefusesExpiredWritesAndKeepsDirectWriters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		expiredAt := server.NowUtc().Add(-time.Hour)
		expiredCtx := context.WithValue(ctx, clientScoreExportExpiryContextKey{}, expiredAt)
		key := "{score-expiry-test}:payload"
		nativeKey := "{score-expiry-test}:native"
		manifest := newClientScoreTargetManifest()
		manifest.locationIds[server.NewId()] = expiredAt.Unix()
		if err := writeClientScoreTargetManifest(expiredCtx, manifest, time.Hour); err == nil {
			t.Fatal("expired export published cleanup authority")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			produce := func(emit func(clientScoreRedisSet) error) error {
				return emit(clientScoreRedisSet{key: key, value: []byte("test")})
			}
			if err := writeClientScoreRedisStream(expiredCtx, r, time.Hour, produce); err == nil {
				t.Fatal("expired export published a union payload")
			}
			facets := nativeTestFacets(t, nil)
			if err := writeClientScoreNativeSnapshot(expiredCtx, r, nativeKey, time.Hour, facets); err == nil {
				t.Fatal("expired export published a native snapshot")
			}
			// Force the server-side boundary as though an already queued
			// command crossed expiry after its local context check.
			publication := &clientScoreNativePublication{pointerKey: nativeKey, slotKey: clientScoreNativeSlotKey(nativeKey, 0), generation: server.NewId().String(), ttl: time.Hour, expiresAt: expiredAt}
			if err := publication.begin(ctx, r); err == nil {
				t.Fatal("expired native staging reached Redis")
			}
			if err := publication.writePages(ctx, r, nil); err == nil {
				t.Fatal("expired native pages reached Redis")
			}
			nativeManifest := clientScoreNativeManifest{Generation: publication.generation, Facets: map[ipFamilyFacet][]clientScoreNativePageInfo{}}
			for _, facet := range ipFamilyFacets {
				nativeManifest.Facets[facet] = nil
			}
			if err := publication.commit(ctx, r, nativeManifest); err == nil {
				t.Fatal("expired native commit reached Redis")
			}
			if count, err := r.Exists(ctx, key, nativeKey, publication.slotKey).Result(); err != nil || count != 0 {
				t.Fatalf("expired writes created keys: %d, %v", count, err)
			}
			if err := writeClientScoreRedisStream(ctx, r, time.Hour, produce); err != nil {
				t.Fatal(err)
			}
			if err := writeClientScoreNativeSnapshot(ctx, r, nativeKey, time.Hour, facets); err != nil {
				t.Fatal(err)
			}
			for _, directKey := range []string{key, nativeKey} {
				if ttl, err := r.PTTL(ctx, directKey).Result(); err != nil || ttl <= 0 || time.Hour < ttl {
					t.Fatalf("standalone writer lost its relative ttl: %s, %v", ttl, err)
				}
			}
		})
	})
}

// Legacy whole-second timestamps round toward safety at fractional boundaries;
// explicit expirations also survive a later export using a shorter ttl.
func TestClientScoreTargetManifestKeepsFractionalAndExplicitExpiry(t *testing.T) {
	start := time.Unix(1_800_000_000, 750_000_000)
	locationId := server.NewId()
	targetMaps := &clientScoreTargetMaps{locationClientScores: map[server.Id]map[server.Id]*ClientScore{locationId: {}}}
	manifest := targetMaps.targetManifest(start)
	empty := newClientScoreTargetMaps(nil, nil, func(server.Id) bool { return false })
	if targets, _ := empty.departedTargets(manifest, start.Add(time.Hour-time.Millisecond), time.Hour); len(targets) != 1 {
		t.Fatal("fractional last-write time was pruned before its payload expired")
	}
	manifest.locationExpireMillis[locationId] = start.Add(2 * time.Hour).UnixMilli()
	encoded, err := encodeClientScoreTargetManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = decodeClientScoreTargetManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if targets, _ := empty.departedTargets(manifest, start.Add(90*time.Minute), time.Hour); len(targets) != 1 {
		t.Fatal("shorter export ttl forgot an earlier generation's live payload")
	}
	if targets, _ := empty.departedTargets(manifest, start.Add(2*time.Hour), time.Hour); len(targets) != 0 {
		t.Fatal("expired target remained in cleanup memory")
	}
}
