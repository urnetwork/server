// Native controls exercise the actual queue aggregate and Redis scripts. The
// held source query and explicit key expiry force ownership without sleeps.
package task

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Eight process-equivalent collectors reach one real source aggregate. The
// fixture includes future work, active leases, release-boundary work, and errors.
func TestTaskQueueMetricsNativeEightCollectors(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		now := server.NowUtc().Truncate(time.Second)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task`))
			for _, row := range []struct {
				runAt, releaseTime time.Time
				failure            any
			}{
				{runAt: now.Add(-10 * time.Second), releaseTime: time.Time{}},
				{runAt: now.Add(-3 * time.Second), releaseTime: time.Time{}, failure: "synthetic available failure"},
				{runAt: now.Add(-time.Hour), releaseTime: now.Add(30 * time.Second), failure: "synthetic claimed failure"},
				{runAt: now.Add(time.Hour), releaseTime: time.Time{}},
				{runAt: now.Add(-time.Hour), releaseTime: now},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO pending_task (
				 task_id,function_name,args_json,run_at,run_priority,run_max_time_seconds,
				 claim_time,release_time,reschedule_error
				) VALUES($1,'synthetic.queue.metrics','{}',$2,0,30,$3,$4,$5)`,
					server.NewId(), row.runAt, time.Time{}, row.releaseTime, row.failure))
			}
		})
		want := taskQueueMetricsSnapshot{Total: 5, Available: 2, Claimed: 1, RescheduleError: 2, OldestOverdueSecond: 10}
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		var queries atomic.Int32
		load := func(ctx context.Context, observedAt time.Time) (taskQueueMetricsSnapshot, error) {
			if queries.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
				return loadTaskQueueMetricsSnapshot(ctx, observedAt)
			case <-ctx.Done():
				return taskQueueMetricsSnapshot{}, ctx.Err()
			}
		}
		type result struct {
			sample *taskQueueMetricsSample
			err    error
		}
		results := make(chan result, 8)
		collectors := make([]taskQueueMetricsCache, 8)
		var workers sync.WaitGroup
		defer workers.Wait()
		defer releaseOnce.Do(func() { close(release) })
		for i := range collectors {
			collectors[i] = taskQueueMetricsCache{
				store: redisTaskQueueMetricsStore{}, now: func() time.Time { return now },
				newToken: func() string { return server.NewId().String() }, load: load,
			}
			workers.Add(1)
			go func(cache taskQueueMetricsCache) {
				defer workers.Done()
				sample, err := cache.get(ctx)
				results <- result{sample: sample, err: err}
			}(collectors[i])
		}
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("native owner did not reach the source boundary")
		}
		for range 7 {
			select {
			case result := <-results:
				if result.sample != nil || result.err != nil {
					t.Fatal("native cold follower invented a complete observation", result.err)
				}
			case <-ctx.Done():
				t.Fatal("native follower waited on the owning query")
			}
		}
		if queries.Load() != 1 {
			t.Fatal("native collectors duplicated the source query", queries.Load())
		}
		releaseOnce.Do(func() { close(release) })
		select {
		case result := <-results:
			if result.err != nil || result.sample == nil || result.sample.snapshot != want || !result.sample.observedAt.Equal(now) {
				t.Fatal("native snapshot lost overlapping state, availability boundary, or source clock", result.err)
			}
		case <-ctx.Done():
			t.Fatal("native owner did not publish")
		}
		workers.Wait()
		var before, after, owner time.Duration
		server.Redis(ctx, func(client server.RedisClient) {
			before = client.PTTL(ctx, taskMetricsCacheKey).Val()
			owner = client.PTTL(ctx, taskMetricsOwnerKey).Val()
		})
		for _, cache := range collectors {
			sample, err := cache.get(ctx)
			if err != nil || sample == nil || sample.snapshot != want || !sample.observedAt.Equal(now) {
				t.Fatal("native follower failed to read the original complete observation", err)
			}
		}
		server.Redis(ctx, func(client server.RedisClient) { after = client.PTTL(ctx, taskMetricsCacheKey).Val() })
		if queries.Load() != 1 || owner <= 0 || owner > taskMetricsRefreshInterval ||
			before <= 0 || before > taskMetricsRetention || after <= 0 || after > before {
			t.Fatal("native followers renewed freshness or successful publication released admission")
		}
	})
}

// The Redis server expires ownership before the successor is admitted. A
// retired token can neither replace that successor nor create a missing value.
func TestTaskQueueMetricsNativePublicationFenceAndExpiry(t *testing.T) {
	(&server.TestEnv{}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		store := redisTaskQueueMetricsStore{}
		if value, acquired, err := store.acquire(ctx, "synthetic-retired"); err != nil || !acquired || value != "" {
			t.Fatal("native initial admission failed", err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.PExpireAt(ctx, taskMetricsOwnerKey, time.Unix(1, 0)).Err())
		})
		if _, acquired, err := store.acquire(ctx, "synthetic-current"); err != nil || !acquired {
			t.Fatal("native expired orphan prevented successor admission", err)
		}
		if published, err := store.publish(ctx, "synthetic-retired", "synthetic-old", taskMetricsRetention); err != nil || published {
			t.Fatal("native expired owner published under its successor")
		}
		if published, err := store.publish(ctx, "synthetic-current", "synthetic-new", taskMetricsRetention); err != nil || !published {
			t.Fatal("native current owner failed to publish", err)
		}
		if value, acquired, err := store.acquire(ctx, "synthetic-follower"); err != nil || acquired || value != "synthetic-new" {
			t.Fatal("native publication lost its admission fence or generation")
		}
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.PExpireAt(ctx, taskMetricsCacheKey, time.Unix(1, 0)).Err())
		})
		if value, acquired, err := store.acquire(ctx, "synthetic-follower"); err != nil || acquired || value != "" {
			t.Fatal("native snapshot expiry became an invented observation")
		}
	})
}
