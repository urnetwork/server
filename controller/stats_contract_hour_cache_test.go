package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

func statsContractHourTestCache(store *statsProviderEgressTestStore, fill func(context.Context, time.Time) (model.ContractHourCounts, error)) statsContractHourCache {
	return statsContractHourCache{
		store: store, keys: statsContractHourKeys("test"), now: store.now,
		newToken: func() string { return "hour-" + strconv.FormatUint(store.tokens.Add(1), 10) }, fill: fill,
	}
}

func statsContractHourTestCounts(context.Context, time.Time) (model.ContractHourCounts, error) {
	return model.ContractHourCounts{Contracts: 7, WithExtender: 3, Disputes: 1}, nil
}

func statsContractHourSnapshotsEqual(a, b *statsContractHourSnapshot) bool {
	return a != nil && b != nil && a.Counts == b.Counts && a.WindowEnd.Equal(b.WindowEnd) && a.CompletedAt.Equal(b.CompletedAt)
}

func TestStatsContractHourCacheEightCollectors(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var fills atomic.Int32
	fill := func(ctx context.Context, now time.Time) (model.ContractHourCounts, error) {
		if fills.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return statsContractHourTestCounts(ctx, now)
		case <-ctx.Done():
			return model.ContractHourCounts{}, ctx.Err()
		}
	}
	type result struct {
		snapshot *statsContractHourSnapshot
		err      error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer once.Do(func() { close(release) })
	collectors := make([]statsContractHourCache, 8)
	for i := range collectors {
		collectors[i] = statsContractHourTestCache(store, fill)
		workers.Add(1)
		go func(cache statsContractHourCache) {
			defer workers.Done()
			snapshot, err := cache.get(t.Context())
			results <- result{snapshot, err}
		}(collectors[i])
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("lease owner did not start")
	}
	for range 7 {
		select {
		case result := <-results:
			if result.snapshot != nil || !errors.Is(result.err, errStatsContractHourBusy) {
				t.Fatal("follower did not remain unavailable")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("follower blocked on source")
		}
	}
	if fills.Load() != 1 {
		t.Fatalf("source reads=%d, want one", fills.Load())
	}
	once.Do(func() { close(release) })
	winner := <-results
	workers.Wait()
	if winner.err != nil || winner.snapshot == nil {
		t.Fatal("owner did not publish")
	}
	store.advance(time.Minute)
	for _, cache := range collectors {
		snapshot, err := cache.get(t.Context())
		if err != nil || !statsContractHourSnapshotsEqual(snapshot, winner.snapshot) {
			t.Fatal("hit changed original source window or counts")
		}
	}
	if fills.Load() != 1 || store.publications != 1 {
		t.Fatal("cache hit renewed or repeated source")
	}
}

func TestStatsContractHourCacheExpiryUsesWindowEnd(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	var fills int
	cache := statsContractHourTestCache(store, func(ctx context.Context, now time.Time) (model.ContractHourCounts, error) {
		fills++
		store.advance(40 * time.Second)
		return statsContractHourTestCounts(ctx, now)
	})
	first, err := cache.get(t.Context())
	if err != nil || store.publishTTL != statsContractHourCacheTTL-40*time.Second {
		t.Fatal("fill duration renewed source age")
	}
	store.advance(statsContractHourCacheTTL - 40*time.Second - time.Millisecond)
	hit, err := cache.get(t.Context())
	if err != nil || !statsContractHourSnapshotsEqual(hit, first) || fills != 1 {
		t.Fatal("fresh boundary missed original value")
	}
	store.advance(time.Millisecond)
	next, err := cache.get(t.Context())
	if err != nil || fills != 2 || !next.WindowEnd.Equal(first.WindowEnd.Add(statsContractHourCacheTTL)) {
		t.Fatal("expired window reused or source clock changed")
	}
}

func TestStatsContractHourCacheFaultFences(t *testing.T) {
	for _, scenario := range []string{"read-error", "acquire-error", "acquire-lost-reply", "publish-error", "publish-lost-reply", "replaced-owner", "panic", "source-error", "cancel", "fill-too-long", "delayed-acquire"} {
		t.Run(scenario, func(t *testing.T) {
			store := newStatsProviderEgressTestStore()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fills := 0
			cache := statsContractHourTestCache(store, func(ctx context.Context, now time.Time) (model.ContractHourCounts, error) {
				fills++
				switch scenario {
				case "panic":
					panic("controlled source failure")
				case "source-error":
					return model.ContractHourCounts{}, errors.New("controlled")
				case "cancel":
					cancel()
				case "fill-too-long":
					store.advance(statsContractHourFillTime + time.Millisecond)
				case "replaced-owner":
					store.put(statsContractHourKeys("test").fill, "replacement", statsContractHourLeaseTTL)
				}
				return statsContractHourTestCounts(ctx, now)
			})
			fault := errors.New("controlled transport failure")
			switch scenario {
			case "read-error":
				store.getErr = fault
			case "acquire-error":
				store.acquireErr = fault
			case "acquire-lost-reply":
				store.acquireErr, store.acquireApplied = fault, true
			case "publish-error":
				store.publishErr = fault
			case "publish-lost-reply":
				store.publishErr, store.publishApplied = fault, true
			case "delayed-acquire":
				store.afterAcquire = func() { store.advance(statsContractHourLeaseTTL - statsContractHourFillTime) }
			}
			snapshot, err := cache.get(ctx)
			if scenario == "acquire-lost-reply" || scenario == "publish-lost-reply" {
				if err != nil || snapshot == nil || fills != 1 {
					t.Fatal("confirmed owner/publication did not recover")
				}
				return
			}
			if snapshot != nil || err == nil {
				t.Fatal("failed owner published a current result")
			}
			if scenario == "read-error" || scenario == "acquire-error" || scenario == "delayed-acquire" {
				if fills != 0 {
					t.Fatal("unavailable coordination triggered database fallback")
				}
				return
			}
			store.getErr, store.acquireErr, store.publishErr, store.afterAcquire = nil, nil, nil, nil
			if snapshot, err := cache.get(t.Context()); snapshot != nil || !errors.Is(err, errStatsContractHourBusy) || fills != 1 {
				t.Fatal("failed/replaced owner lost retry fence")
			}
			if scenario == "replaced-owner" {
				owner, exists, _ := store.get(t.Context(), cache.keys.fill)
				if !exists || owner != "replacement" {
					t.Fatal("late owner modified successor")
				}
			}
		})
	}
}

func TestStatsContractHourCacheMissPublishRace(t *testing.T) {
	for _, releaseFails := range []bool{false, true} {
		store := newStatsProviderEgressTestStore()
		cache := statsContractHourTestCache(store, func(context.Context, time.Time) (model.ContractHourCounts, error) {
			t.Fatal("previous publication was needlessly recounted")
			return model.ContractHourCounts{}, nil
		})
		now := store.now()
		value, _ := encodeStatsContractHourSnapshot(&statsContractHourSnapshot{model.ContractHourCounts{}, now, now}, now)
		store.afterAcquire = func() { store.put(cache.keys.value, value, statsContractHourCacheTTL) }
		if releaseFails {
			store.releaseErr = errors.New("controlled")
		}
		snapshot, err := cache.get(t.Context())
		if releaseFails {
			if snapshot != nil || err == nil {
				t.Fatal("ambiguous release reported success")
			}
		} else if err != nil || snapshot == nil || store.releases != 1 {
			t.Fatal("recheck did not recover prior publication")
		}
	}
}

func TestStatsContractHourCacheClosedWire(t *testing.T) {
	store := newStatsProviderEgressTestStore()
	now := store.now()
	zero := &statsContractHourSnapshot{model.ContractHourCounts{}, now, now}
	value, err := encodeStatsContractHourSnapshot(zero, now)
	if err != nil {
		t.Fatal(err)
	}
	got, fresh, err := decodeStatsContractHourSnapshot(value, now)
	if err != nil || !fresh || !statsContractHourSnapshotsEqual(got, zero) {
		t.Fatal("legitimate zeros did not round trip")
	}
	for _, bad := range []string{
		"", value + "{}", " " + value, strings.Repeat("x", 513),
		strings.Replace(value, `"v":1`, `"v":2`, 1),
		strings.Replace(value, `"v":1`, `"v":1,"v":1`, 1),
		strings.Replace(value, `"contracts":0`, `"contracts":-1`, 1),
		strings.Replace(value, `"disputes":0,`, ``, 1),
		strings.Replace(value, `"disputes":0`, `"unknown":0`, 1),
	} {
		cache := statsContractHourTestCache(store, func(context.Context, time.Time) (model.ContractHourCounts, error) {
			t.Fatal("malformed publication caused a database fallback")
			return model.ContractHourCounts{}, nil
		})
		store.put(cache.keys.value, bad, time.Hour)
		if snapshot, err := cache.get(t.Context()); snapshot != nil || !errors.Is(err, errStatsContractHourInvalid) {
			t.Fatal("malformed shared publication accepted")
		}
	}
	if _, fresh, err := decodeStatsContractHourSnapshot(value, now.Add(statsContractHourCacheTTL)); err != nil || fresh {
		t.Fatal("source expiry was not an ordinary miss")
	}
	if _, _, err := decodeStatsContractHourSnapshot(value, now.Add(-statsContractHourClockSkew-time.Millisecond)); err == nil {
		t.Fatal("future source accepted")
	}
}
