package controller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Real Redis ownership scripts fence eight independent logical Taskworkers.
// The winner executes the real PostgreSQL hourly-window reader after the
// barrier; seven followers return before that source read is released.
func TestStatsContractHourCacheNativeEightCollectors(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		created := time.Now().Add(-time.Minute)
		testStatsContract(ctx, created, false, false)
		testStatsContract(ctx, created, false, true)
		testStatsContract(ctx, created, true, false)
		keys := statsContractHourKeys("native-" + server.NewId().String())
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		var fills atomic.Int32
		fill := func(ctx context.Context, now time.Time) (model.ContractHourCounts, error) {
			if fills.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
				return model.CountContractHourWindow(ctx, now), nil
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
			collectors[i] = statsContractHourCache{redisStatsProviderEgressCacheStore{}, keys, time.Now, func() string { return server.NewId().String() }, fill}
			workers.Add(1)
			go func(cache statsContractHourCache) {
				defer workers.Done()
				snapshot, err := cache.get(ctx)
				results <- result{snapshot, err}
			}(collectors[i])
		}
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("native owner did not reach source")
		}
		for range len(collectors) - 1 {
			select {
			case result := <-results:
				if result.snapshot != nil || !errors.Is(result.err, errStatsContractHourBusy) {
					t.Fatal("native follower did not remain unavailable")
				}
			case <-ctx.Done():
				t.Fatal("native follower blocked on source")
			}
		}
		if fills.Load() != 1 {
			t.Fatal("native fleet repeated the source read")
		}
		once.Do(func() { close(release) })
		var winner *statsContractHourSnapshot
		select {
		case result := <-results:
			if result.err != nil || result.snapshot == nil {
				t.Fatalf("native owner did not publish real SQL result: unavailable=%t invalid=%t busy=%t", errors.Is(result.err, errStatsContractHourUnavailable), errors.Is(result.err, errStatsContractHourInvalid), errors.Is(result.err, errStatsContractHourBusy))
			}
			winner = result.snapshot
		case <-ctx.Done():
			t.Fatal("native SQL/publication did not complete")
		}
		workers.Wait()
		if winner.Counts != (model.ContractHourCounts{Contracts: 3, WithExtender: 1, Disputes: 1}) {
			t.Fatal("native shared result lost populated exact counts")
		}
		if stats := model.Testing_ContractHourCacheStats(); stats.LiveQueries != 2 {
			t.Fatal("native winner did not run both live range counts")
		}
		var before, after time.Duration
		server.Redis(ctx, func(client server.RedisClient) { before = client.PTTL(ctx, keys.value).Val() })
		for _, cache := range collectors {
			snapshot, err := cache.get(ctx)
			if err != nil || !statsContractHourSnapshotsEqual(snapshot, winner) {
				t.Fatal("native hit changed source window/counts")
			}
		}
		server.Redis(ctx, func(client server.RedisClient) { after = client.PTTL(ctx, keys.value).Val() })
		if fills.Load() != 1 || before <= 0 || before > statsContractHourCacheTTL || after <= 0 || after > before {
			t.Fatal("native hit repeated read or renewed expiry")
		}
		if _, exists, err := collectors[0].store.get(ctx, keys.fill); err != nil || exists {
			t.Fatal("native publication retained fill owner")
		}
	})
}

func TestStatsContractHourCacheNativeFences(t *testing.T) {
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		store := redisStatsProviderEgressCacheStore{}
		cache := statsContractHourCache{store, statsContractHourKeys("native-failed-" + server.NewId().String()), time.Now, func() string { return server.NewId().String() }, nil}
		var fills int
		cache.fill = func(context.Context, time.Time) (model.ContractHourCounts, error) {
			fills++
			return model.ContractHourCounts{}, errors.New("controlled")
		}
		if snapshot, err := cache.get(ctx); snapshot != nil || !errors.Is(err, errStatsContractHourUnavailable) {
			t.Fatal("failed native source published")
		}
		if snapshot, err := cache.get(ctx); snapshot != nil || !errors.Is(err, errStatsContractHourBusy) || fills != 1 {
			t.Fatal("failed native source lost its retry fence")
		}
		cache.keys = statsContractHourKeys("native-replaced-" + server.NewId().String())
		cache.fill = func(context.Context, time.Time) (model.ContractHourCounts, error) {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, cache.keys.fill, "replacement", statsContractHourLeaseTTL).Err())
			})
			return model.ContractHourCounts{}, nil
		}
		if snapshot, err := cache.get(ctx); snapshot != nil || !errors.Is(err, errStatsContractHourUnavailable) {
			t.Fatal("replaced native owner published")
		}
		if owner, exists, err := store.get(ctx, cache.keys.fill); err != nil || !exists || owner != "replacement" {
			t.Fatal("late owner mutated successor")
		}
		if _, exists, err := store.get(ctx, cache.keys.value); err != nil || exists {
			t.Fatal("replaced owner left a completed value")
		}
	})
}
