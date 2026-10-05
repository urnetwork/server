package controller

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

type statsProviderEgressTestEntry struct {
	value   string
	expires time.Time
}

// Independent logical collectors share only this store, as separate processes do.
// Expiry and ambiguous replies are explicit transitions, never timing sleeps.
type statsProviderEgressTestStore struct {
	mu      sync.Mutex
	clock   time.Time
	entries map[string]statsProviderEgressTestEntry
	tokens  atomic.Uint64

	getErr, acquireErr, publishErr, releaseErr error
	acquireApplied, publishApplied             bool
	afterAcquire                               func()
	afterGet                                   func()
	acquisitions, publications, releases       int
	acquireTTL, publishTTL                     time.Duration
}

func newStatsProviderEgressTestStore() *statsProviderEgressTestStore {
	return &statsProviderEgressTestStore{
		clock:   time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC),
		entries: map[string]statsProviderEgressTestEntry{},
	}
}

func (store *statsProviderEgressTestStore) now() time.Time {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.clock
}

func (store *statsProviderEgressTestStore) advance(duration time.Duration) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.clock = store.clock.Add(duration)
}

func (store *statsProviderEgressTestStore) retained(key string) (statsProviderEgressTestEntry, bool) {
	entry, exists := store.entries[key]
	if exists && !store.clock.Before(entry.expires) {
		delete(store.entries, key)
		return statsProviderEgressTestEntry{}, false
	}
	return entry, exists
}

func (store *statsProviderEgressTestStore) get(_ context.Context, key string) (string, bool, error) {
	store.mu.Lock()
	entry, exists := store.retained(key)
	err, hook := store.getErr, store.afterGet
	store.mu.Unlock()
	if hook != nil {
		hook()
	}
	return entry.value, exists, err
}

func (store *statsProviderEgressTestStore) acquire(_ context.Context, key, token string, ttl time.Duration) (bool, error) {
	store.mu.Lock()
	store.acquisitions++
	store.acquireTTL = ttl
	_, exists := store.retained(key)
	err, hook := store.acquireErr, store.afterAcquire
	acquired := !exists && err == nil
	if !exists && (err == nil || store.acquireApplied) {
		store.entries[key] = statsProviderEgressTestEntry{token, store.clock.Add(ttl)}
	}
	store.mu.Unlock()
	if hook != nil {
		hook()
	}
	return acquired, err
}

func (store *statsProviderEgressTestStore) publish(_ context.Context, keys statsProviderEgressCacheKeys, token, value string, ttl time.Duration) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.publications++
	store.publishTTL = ttl
	owner, exists := store.retained(keys.fill)
	if !exists || owner.value != token {
		return false, nil
	}
	if store.publishErr == nil || store.publishApplied {
		store.entries[keys.value] = statsProviderEgressTestEntry{value, store.clock.Add(ttl)}
		delete(store.entries, keys.fill)
	}
	return store.publishErr == nil, store.publishErr
}

func (store *statsProviderEgressTestStore) release(_ context.Context, key, token string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.releases++
	if store.releaseErr != nil {
		return store.releaseErr
	}
	if owner, exists := store.retained(key); exists && owner.value == token {
		delete(store.entries, key)
	}
	return nil
}

func (store *statsProviderEgressTestStore) put(key, value string, ttl time.Duration) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.entries[key] = statsProviderEgressTestEntry{value, store.clock.Add(ttl)}
}

func statsProviderEgressTestCounts() *model.ProviderEgressCounts {
	counts := &model.ProviderEgressCounts{MaxIndex: 6, BucketIndexCounts: map[string]map[string]int64{}, ReasonCounts: map[string]int64{}}
	for _, bucket := range model.ProviderEgressBuckets {
		counts.BucketIndexCounts[bucket] = map[string]int64{"0": 0, "1": 7, "32767": 2, model.ProviderEgressIndexNone: 0}
	}
	for _, reason := range model.ProviderExcludedReasons {
		counts.ReasonCounts[reason] = 0
	}
	return counts
}

func statsProviderEgressTestCache(store *statsProviderEgressTestStore, fill func(context.Context) (*model.ProviderEgressCounts, error)) statsProviderEgressCache {
	return statsProviderEgressCache{
		store: store, now: store.now,
		newToken: func() string { return "token-" + strconv.FormatUint(store.tokens.Add(1), 10) },
		fill:     fill,
	}
}

func statsProviderEgressTestWire(t testing.TB, counts *model.ProviderEgressCounts, started, completed time.Time) string {
	t.Helper()
	value, err := encodeStatsProviderEgressSnapshot(&statsProviderEgressSnapshot{counts, started, completed}, completed)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestStatsProviderEgressCacheEightCollectors(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var fills atomic.Int32
	fill := func(ctx context.Context) (*model.ProviderEgressCounts, error) {
		if fills.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return statsProviderEgressTestCounts(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	collectors := make([]statsProviderEgressCache, 8)
	for index := range collectors {
		collectors[index] = statsProviderEgressTestCache(store, fill)
	}
	type result struct {
		snapshot *statsProviderEgressSnapshot
		err      error
	}
	winner := make(chan result, 1)
	go func() { snapshot, err := collectors[0].get(t.Context(), "policy"); winner <- result{snapshot, err} }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first collector did not start its fill")
	}
	followers := make(chan result, 7)
	for index := 1; index < len(collectors); index++ {
		go func(index int) {
			snapshot, err := collectors[index].get(t.Context(), "policy")
			followers <- result{snapshot, err}
		}(index)
	}
	for range 7 {
		select {
		case result := <-followers:
			if result.snapshot != nil || !errors.Is(result.err, errStatsProviderEgressBusy) {
				t.Fatalf("cold follower reported a complete result: %v", result.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cold follower blocked on the winning query")
		}
	}
	if fills.Load() != 1 {
		t.Fatalf("eight collectors began %d fills", fills.Load())
	}
	releaseOnce.Do(func() { close(release) })
	var first result
	select {
	case first = <-winner:
	case <-time.After(3 * time.Second):
		t.Fatal("winner did not finish publication")
	}
	if first.err != nil || first.snapshot == nil {
		t.Fatalf("winner unavailable: %v", first.err)
	}
	// These are per-collector sinks, with no shared package-global gauge writes.
	sinks := make([]*statsProviderEgressSnapshot, 8)
	for index := range collectors {
		snapshot, err := collectors[index].get(t.Context(), "policy")
		if err != nil || !reflect.DeepEqual(snapshot.Counts, first.snapshot.Counts) || !snapshot.CompletedAt.Equal(first.snapshot.CompletedAt) {
			t.Fatalf("collector %d did not read the original complete snapshot: %v", index, err)
		}
		sinks[index] = snapshot
	}
	sinks[0].Counts.ReasonCounts[model.ProviderExcludedTls] = 100
	if sinks[1].Counts.ReasonCounts[model.ProviderExcludedTls] != 0 || fills.Load() != 1 {
		t.Fatal("readers shared mutable maps or re-executed the query")
	}
	if store.acquireTTL != statsProviderEgressLeaseTTL || store.publishTTL != statsProviderEgressCacheTTL {
		t.Fatal("lease or completed snapshot TTL changed")
	}
}

func TestStatsProviderEgressCacheRechecksAfterWinning(t *testing.T) {
	for _, releaseFails := range []bool{false, true} {
		t.Run(strconv.FormatBool(releaseFails), func(t *testing.T) {
			store := newStatsProviderEgressTestStore()
			keys := statsProviderEgressKeys("policy")
			value := statsProviderEgressTestWire(t, statsProviderEgressTestCounts(), store.now(), store.now())
			store.afterAcquire = func() { store.put(keys.value, value, statsProviderEgressCacheTTL) }
			if releaseFails {
				store.releaseErr = errors.New("untrusted backend detail")
			}
			cache := statsProviderEgressTestCache(store, func(context.Context) (*model.ProviderEgressCounts, error) {
				t.Error("racing publication was recomputed")
				return nil, nil
			})
			snapshot, err := cache.get(t.Context(), "policy")
			if releaseFails {
				if snapshot != nil || err != errStatsProviderEgressUnavailable {
					t.Fatalf("release failure became healthy: %v", err)
				}
			} else if err != nil || snapshot == nil || store.releases != 1 {
				t.Fatalf("post-acquisition cache recheck failed: %v", err)
			}
		})
	}
}

func TestStatsProviderEgressCacheRedisFaults(t *testing.T) {
	for _, scenario := range []string{"get", "acquire", "acquire_lost_reply", "publish", "publish_lost_reply"} {
		t.Run(scenario, func(t *testing.T) {
			store := newStatsProviderEgressTestStore()
			privateErr := errors.New("untrusted backend detail")
			wantSuccess, wantFills := false, 0
			switch scenario {
			case "get":
				store.getErr = privateErr
			case "acquire":
				store.acquireErr = privateErr
			case "acquire_lost_reply":
				store.acquireErr, store.acquireApplied, wantSuccess, wantFills = privateErr, true, true, 1
			case "publish":
				store.publishErr, wantFills = privateErr, 1
			case "publish_lost_reply":
				store.publishErr, store.publishApplied, wantSuccess, wantFills = privateErr, true, true, 1
			}
			fills := 0
			cache := statsProviderEgressTestCache(store, func(context.Context) (*model.ProviderEgressCounts, error) {
				fills++
				return statsProviderEgressTestCounts(), nil
			})
			snapshot, err := cache.get(t.Context(), "policy")
			if wantSuccess {
				if err != nil || snapshot == nil {
					t.Fatalf("authoritative readback failed: %v", err)
				}
			} else if snapshot != nil || err != errStatsProviderEgressUnavailable {
				t.Fatalf("Redis fault became a result or leaked its error: %v", err)
			}
			if fills != wantFills {
				t.Fatalf("fills=%d, want %d", fills, wantFills)
			}
			if scenario == "publish" {
				if snapshot, err = cache.get(t.Context(), "policy"); snapshot != nil || err != errStatsProviderEgressBusy || fills != 1 {
					t.Fatal("failed publication lost its retry fence")
				}
			}
		})
	}
}

func TestStatsProviderEgressCacheFailedFillRetainsFence(t *testing.T) {
	for _, scenario := range []string{"error", "panic", "nil", "cancel", "late"} {
		t.Run(scenario, func(t *testing.T) {
			store := newStatsProviderEgressTestStore()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fills := 0
			cache := statsProviderEgressTestCache(store, func(fillCtx context.Context) (*model.ProviderEgressCounts, error) {
				fills++
				deadline, ok := fillCtx.Deadline()
				if !ok || time.Until(deadline) > statsProviderEgressFillTime {
					t.Error("fill has no bounded context")
				}
				switch scenario {
				case "error":
					return nil, errors.New("untrusted query detail")
				case "panic":
					panic("untrusted query detail")
				case "nil":
					return nil, nil
				case "cancel":
					cancel()
				case "late":
					store.advance(statsProviderEgressFillTime + time.Second)
				}
				return statsProviderEgressTestCounts(), nil
			})
			if snapshot, err := cache.get(ctx, "policy"); snapshot != nil || (err != errStatsProviderEgressUnavailable && err != errStatsProviderEgressInvalid) {
				t.Fatalf("failed fill published success: %v", err)
			}
			if store.publications != 0 {
				t.Fatal("failed fill attempted publication")
			}
			if snapshot, err := cache.get(t.Context(), "policy"); snapshot != nil || err != errStatsProviderEgressBusy || fills != 1 {
				t.Fatal("failed fill lost its retry fence")
			}
		})
	}
}

func TestStatsProviderEgressCacheLateOwnerCannotPublish(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(strconv.FormatBool(expired), func(t *testing.T) {
			store := newStatsProviderEgressTestStore()
			keys := statsProviderEgressKeys("policy")
			var replacement string
			cache := statsProviderEgressTestCache(store, func(context.Context) (*model.ProviderEgressCounts, error) {
				if expired {
					store.advance(statsProviderEgressLeaseTTL + time.Second)
				}
				counts := statsProviderEgressTestCounts()
				counts.ReasonCounts[model.ProviderExcludedTls] = 19
				replacement = statsProviderEgressTestWire(t, counts, store.now(), store.now())
				store.put(keys.fill, "replacement-owner", statsProviderEgressLeaseTTL)
				store.put(keys.value, replacement, statsProviderEgressCacheTTL)
				return statsProviderEgressTestCounts(), nil
			})
			snapshot, err := cache.get(t.Context(), "policy")
			if expired {
				if snapshot != nil || err != errStatsProviderEgressUnavailable || store.publications != 0 {
					t.Fatal("expired query was accepted")
				}
			} else if err != nil || snapshot == nil || snapshot.Counts.ReasonCounts[model.ProviderExcludedTls] != 19 {
				t.Fatalf("replaced owner did not recover the authoritative value: %v", err)
			}
			if value, _, _ := store.get(t.Context(), keys.value); value != replacement {
				t.Fatal("late owner overwrote its successor")
			}
			if token, _, _ := store.get(t.Context(), keys.fill); token != "replacement-owner" {
				t.Fatal("late owner deleted its successor lease")
			}
		})
	}
}

func TestStatsProviderEgressCacheDelayedLeaseCannotStartFill(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	store.afterAcquire = func() { store.advance(statsProviderEgressLeaseTTL - statsProviderEgressFillTime) }
	cache := statsProviderEgressTestCache(store, func(context.Context) (*model.ProviderEgressCounts, error) {
		t.Error("delayed acquisition started a query beyond its remaining lease budget")
		return nil, nil
	})
	if snapshot, err := cache.get(t.Context(), "policy"); snapshot != nil || err != errStatsProviderEgressUnavailable || store.publications != 0 {
		t.Fatal("delayed lease was accepted")
	}
}

func TestStatsProviderEgressCacheCompletionAgeAndNoRenewal(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	cache := statsProviderEgressTestCache(store, func(context.Context) (*model.ProviderEgressCounts, error) {
		store.advance(90 * time.Second)
		return statsProviderEgressTestCounts(), nil
	})
	snapshot, err := cache.get(t.Context(), "policy")
	if err != nil || snapshot.CompletedAt.Sub(snapshot.StartedAt) != 90*time.Second {
		t.Fatalf("long bounded fill failed: %v", err)
	}
	keys := statsProviderEgressKeys("policy")
	store.mu.Lock()
	original := store.entries[keys.value]
	store.mu.Unlock()
	store.advance(299 * time.Second)
	hit, err := cache.get(t.Context(), "policy")
	if err != nil || !hit.CompletedAt.Equal(snapshot.CompletedAt) || !hit.StartedAt.Equal(snapshot.StartedAt) {
		t.Fatal("completion-age cache hit failed")
	}
	store.mu.Lock()
	after := store.entries[keys.value]
	store.mu.Unlock()
	if !original.expires.Equal(after.expires) || store.publications != 1 {
		t.Fatal("cache hit extended the original expiry")
	}
	if _, fresh, err := decodeStatsProviderEgressSnapshot(original.value, snapshot.CompletedAt.Add(statsProviderEgressCacheTTL)); err != nil || fresh {
		t.Fatal("five-minute-old completion stayed fresh")
	}
}

func TestStatsProviderEgressCacheCanceledReadAndEmptyPolicy(t *testing.T) {
	for _, scenario := range []string{"canceled", "canceled_read", "empty_policy", "large_policy"} {
		t.Run(scenario, func(t *testing.T) {
			store := newStatsProviderEgressTestStore()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			policy := "policy"
			switch scenario {
			case "canceled":
				cancel()
			case "canceled_read":
				store.afterGet = cancel
			case "empty_policy":
				policy = ""
			case "large_policy":
				policy = strings.Repeat("x", 4097)
			}
			cache := statsProviderEgressTestCache(store, func(context.Context) (*model.ProviderEgressCounts, error) {
				t.Error("invalid call executed a query")
				return nil, nil
			})
			if snapshot, err := cache.get(ctx, policy); snapshot != nil || err != errStatsProviderEgressUnavailable || store.acquisitions != 0 {
				t.Fatal("invalid call reached lease or query")
			}
		})
	}
}

func TestStatsProviderEgressCacheWireValidation(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	valid := statsProviderEgressTestWire(t, statsProviderEgressTestCounts(), store.now(), store.now())
	for _, scenario := range []string{"extra", "version", "null_counts", "null_index", "unknown_bucket", "unknown_reason", "negative", "leading_zero", "index_bound", "max_index", "future", "reversed", "long_fill"} {
		t.Run(scenario, func(t *testing.T) {
			var wire map[string]any
			if err := json.Unmarshal([]byte(valid), &wire); err != nil {
				t.Fatal(err)
			}
			counts := wire["n"].(map[string]any)
			buckets, reasons := counts["b"].(map[string]any), counts["r"].(map[string]any)
			switch scenario {
			case "extra":
				wire["private_field"] = "untrusted"
			case "version":
				wire["v"] = 2
			case "null_counts":
				wire["n"] = nil
			case "null_index":
				counts["m"] = nil
			case "unknown_bucket":
				buckets["untrusted"] = map[string]any{}
			case "unknown_reason":
				reasons["untrusted"] = 0
			case "negative":
				reasons[model.ProviderExcludedTls] = -1
			case "leading_zero":
				buckets[model.RankModeQuality].(map[string]any)["01"] = 1
			case "index_bound":
				buckets[model.RankModeQuality].(map[string]any)["32768"] = 1
			case "max_index":
				counts["m"] = 32768
			case "future":
				wire["s"], wire["c"] = store.now().Add(6*time.Second).UnixMilli(), store.now().Add(6*time.Second).UnixMilli()
			case "reversed":
				wire["c"] = store.now().Add(-time.Second).UnixMilli()
			case "long_fill":
				wire["s"] = store.now().Add(-126 * time.Second).UnixMilli()
			}
			encoded, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			store.put(statsProviderEgressKeys("policy").value, string(encoded), statsProviderEgressCacheTTL)
			cache := statsProviderEgressTestCache(store, func(context.Context) (*model.ProviderEgressCounts, error) {
				t.Error("invalid cache value became a SQL miss")
				return nil, nil
			})
			if snapshot, err := cache.get(t.Context(), "policy"); snapshot != nil || err != errStatsProviderEgressInvalid || store.acquisitions != 0 {
				t.Fatalf("malformed snapshot admitted: %v", err)
			}
		})
	}
	for _, value := range []string{"", "null", valid + "{}", strings.Repeat("x", statsProviderEgressWireBytes+1)} {
		if _, _, err := decodeStatsProviderEgressSnapshot(value, store.now()); err != errStatsProviderEgressInvalid {
			t.Fatal("invalid wire envelope admitted")
		}
	}
}

func TestStatsProviderEgressCachePreservesAllZeroAndSmallintLabels(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	counts := statsProviderEgressTestCounts()
	for _, indexes := range counts.BucketIndexCounts {
		for key := range indexes {
			indexes[key] = 0
		}
	}
	value := statsProviderEgressTestWire(t, counts, store.now(), store.now())
	decoded, fresh, err := decodeStatsProviderEgressSnapshot(value, store.now())
	if err != nil || !fresh || !reflect.DeepEqual(decoded.Counts, counts) {
		t.Fatal("authoritative zero counts changed")
	}
	counts.MaxIndex = statsProviderEgressMaxIndex
	for _, indexes := range counts.BucketIndexCounts {
		for index := 0; index <= statsProviderEgressMaxIndex; index++ {
			indexes[strconv.Itoa(index)] = math.MaxInt64
		}
	}
	value = statsProviderEgressTestWire(t, counts, store.now(), store.now())
	decoded, fresh, err = decodeStatsProviderEgressSnapshot(value, store.now())
	if err != nil || !fresh || !reflect.DeepEqual(decoded.Counts, counts) || len(value) > statsProviderEgressWireBytes {
		t.Fatal("complete smallint label domain was truncated")
	}
	t.Logf("complete three-bucket smallint wire bytes=%d", len(value))
}

func TestStatsProviderEgressCacheKeysSharePolicySlot(t *testing.T) {
	keys := statsProviderEgressKeys("private-policy-text")
	other := statsProviderEgressKeys("other-policy")
	tag := func(key string) string { return strings.SplitN(strings.SplitN(key, "{", 2)[1], "}", 2)[0] }
	if tag(keys.value) != tag(keys.fill) || tag(keys.value) == tag(other.value) || strings.Contains(keys.value, "private-policy-text") {
		t.Fatal("cache keys lost slot or policy isolation")
	}
}
