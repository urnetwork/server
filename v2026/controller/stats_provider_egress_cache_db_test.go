package controller

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Eight independent collector objects use the production Redis adapter. Only
// the source count is controlled, so the lease/publication scripts and native
// socket deadline pool remain the same as production.
func TestStatsProviderEgressCacheNativeEightCollectors(t *testing.T) {
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		store := redisStatsProviderEgressCacheStore{}
		policy := "native-eight-" + server.NewId().String()
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
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
		type result struct {
			snapshot *statsProviderEgressSnapshot
			err      error
		}
		collectors := make([]statsProviderEgressCache, 8)
		results := make(chan result, len(collectors))
		var workers sync.WaitGroup
		defer workers.Wait()
		// Release precedes the join even when an assertion terminates this test.
		defer once.Do(func() { close(release) })
		for i := range collectors {
			collectors[i] = statsProviderEgressCache{store, time.Now, func() string { return server.NewId().String() }, fill}
			workers.Add(1)
			go func(cache statsProviderEgressCache) {
				defer workers.Done()
				snapshot, err := cache.get(ctx, policy)
				results <- result{snapshot, err}
			}(collectors[i])
		}
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("native lease owner did not reach source fill")
		}
		for range len(collectors) - 1 {
			select {
			case result := <-results:
				if result.snapshot != nil || !errors.Is(result.err, errStatsProviderEgressBusy) {
					t.Fatal("native follower did not remain explicitly unavailable")
				}
			case <-ctx.Done():
				t.Fatal("native follower did not complete before source release")
			}
		}
		if fills.Load() != 1 {
			t.Fatalf("native eight-collector source fills=%d, want one", fills.Load())
		}
		once.Do(func() { close(release) })
		var winner *statsProviderEgressSnapshot
		select {
		case result := <-results:
			if result.err != nil || result.snapshot == nil {
				t.Fatal("native owner did not publish a complete snapshot")
			}
			winner = result.snapshot
		case <-ctx.Done():
			t.Fatal("native owner did not finish publication")
		}
		workers.Wait()
		keys := statsProviderEgressKeys(policy)
		var before, after time.Duration
		server.Redis(ctx, func(client server.RedisClient) { before = client.PTTL(ctx, keys.value).Val() })
		for _, collector := range collectors {
			snapshot, err := collector.get(ctx, policy)
			if err != nil || snapshot == nil || !reflect.DeepEqual(snapshot.Counts, winner.Counts) || snapshot.StartedAt.UnixMilli() != winner.StartedAt.UnixMilli() || snapshot.CompletedAt.UnixMilli() != winner.CompletedAt.UnixMilli() {
				t.Fatal("native shared result changed counts or original source times")
			}
		}
		server.Redis(ctx, func(client server.RedisClient) { after = client.PTTL(ctx, keys.value).Val() })
		if fills.Load() != 1 || before <= 0 || before > statsProviderEgressCacheTTL || after <= 0 || after > before {
			t.Fatal("native cache hits repeated the source or renewed expiry")
		}
		if _, exists, err := store.get(ctx, keys.fill); err != nil || exists {
			t.Fatal("native successful publication did not consume its ownership lease")
		}
	})
}

func TestStatsProviderEgressCacheNativeFences(t *testing.T) {
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		store := redisStatsProviderEgressCacheStore{}
		failedPolicy := "native-failed-" + server.NewId().String()
		var fills int
		cache := statsProviderEgressCache{store, time.Now, func() string { return server.NewId().String() }, func(context.Context) (*model.ProviderEgressCounts, error) {
			fills++
			return nil, errors.New("controlled source failure")
		}}
		if snapshot, err := cache.get(ctx, failedPolicy); snapshot != nil || !errors.Is(err, errStatsProviderEgressUnavailable) {
			t.Fatal("failed native source was published")
		}
		if snapshot, err := cache.get(ctx, failedPolicy); snapshot != nil || !errors.Is(err, errStatsProviderEgressBusy) || fills != 1 {
			t.Fatal("failed native source lost its retry fence")
		}
		replacedPolicy := "native-replaced-" + server.NewId().String()
		keys := statsProviderEgressKeys(replacedPolicy)
		cache.fill = func(context.Context) (*model.ProviderEgressCounts, error) {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, keys.fill, "replacement-owner", statsProviderEgressLeaseTTL).Err())
			})
			return statsProviderEgressTestCounts(), nil
		}
		if snapshot, err := cache.get(ctx, replacedPolicy); snapshot != nil || !errors.Is(err, errStatsProviderEgressUnavailable) {
			t.Fatal("replaced native owner published a result")
		}
		if owner, exists, err := store.get(ctx, keys.fill); err != nil || !exists || owner != "replacement-owner" {
			t.Fatal("late publisher modified another native owner")
		}
		if _, exists, err := store.get(ctx, keys.value); err != nil || exists {
			t.Fatal("replaced native owner left a completed value")
		}
	})
}

func TestStatsProviderEgressCacheNativeGaugeFreshness(t *testing.T) {
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		oldAvailable, oldStarted, oldCompleted := statsProviderEgressAvailableGauge, statsProviderEgressStartedGauge, statsProviderEgressCompletedGauge
		oldIndexes, oldReasons := statsProviderEgressIndexGauge, statsProviderExcludedGauge
		statsProviderEgressAvailableGauge = newStatsGauge("test_egress_available", "test")
		statsProviderEgressStartedGauge = newStatsGauge("test_egress_started", "test")
		statsProviderEgressCompletedGauge = newStatsGauge("test_egress_completed", "test")
		statsProviderEgressIndexGauge = newStatsGaugeVec("test_egress_indexes", "test", "bucket", "index")
		statsProviderExcludedGauge = newStatsGaugeVec("test_egress_reasons", "test", "reason")
		defer func() {
			for _, gauge := range []*statsGauge{statsProviderEgressAvailableGauge, statsProviderEgressStartedGauge, statsProviderEgressCompletedGauge} {
				prometheus.DefaultRegisterer.Unregister(gauge.gauge)
			}
			prometheus.DefaultRegisterer.Unregister(statsProviderEgressIndexGauge.gauge)
			prometheus.DefaultRegisterer.Unregister(statsProviderExcludedGauge.gauge)
			statsProviderEgressAvailableGauge, statsProviderEgressStartedGauge, statsProviderEgressCompletedGauge = oldAvailable, oldStarted, oldCompleted
			statsProviderEgressIndexGauge, statsProviderExcludedGauge = oldIndexes, oldReasons
		}()
		// Pin the fractional source second that exposed the old nanosecond
		// conversion, while keeping the absolute window fresh for this fixture.
		now := time.Now().Truncate(time.Second).Add(2 * time.Millisecond)
		started, completed := now.Add(-2*time.Minute), now.Add(-time.Minute)
		counts := statsProviderEgressTestCounts()
		value := statsProviderEgressTestWire(t, counts, started, completed)
		key := statsProviderEgressKeys(model.ProviderEgressCountsPolicyKey()).value
		set := func(value string) {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, key, value, statsProviderEgressCacheTTL).Err())
			})
		}
		set(value)
		statsRefreshProviderEgress(ctx)
		wantStarted := statsSourceTimestampSeconds(started)
		wantCompleted := statsSourceTimestampSeconds(completed)
		if testStatsGaugeValue(t, statsProviderEgressAvailableGauge) != 1 || testStatsGaugeValue(t, statsProviderEgressStartedGauge) != wantStarted || testStatsGaugeValue(t, statsProviderEgressCompletedGauge) != wantCompleted {
			t.Fatal("cache hit did not preserve original source times and availability")
		}
		if testutil.ToFloat64(statsProviderEgressIndexGauge.gauge.WithLabelValues(model.RankModeQuality, "32767")) != 2 {
			t.Fatal("native publication dropped a historical index label")
		}
		set("invalid-snapshot")
		panicked := false
		func() {
			defer func() { panicked = recover() != nil }()
			statsRefreshProviderEgress(ctx)
		}()
		if !panicked || testStatsGaugeValue(t, statsProviderEgressAvailableGauge) != 0 || testStatsGaugeValue(t, statsProviderEgressStartedGauge) != wantStarted || testStatsGaugeValue(t, statsProviderEgressCompletedGauge) != wantCompleted || testutil.ToFloat64(statsProviderEgressIndexGauge.gauge.WithLabelValues(model.RankModeQuality, "32767")) != 2 {
			t.Fatal("unavailable refresh changed prior counts or source times")
		}
		for _, indexes := range counts.BucketIndexCounts {
			for label := range indexes {
				indexes[label] = 0
			}
		}
		set(statsProviderEgressTestWire(t, counts, started, completed))
		statsRefreshProviderEgress(ctx)
		if testStatsGaugeValue(t, statsProviderEgressAvailableGauge) != 1 || testutil.ToFloat64(statsProviderEgressIndexGauge.gauge.WithLabelValues(model.RankModeQuality, "32767")) != 0 || testStatsGaugeValue(t, statsProviderEgressCompletedGauge) != wantCompleted {
			t.Fatal("legitimate complete zero counts were treated as missing or renewed")
		}
	})
}
