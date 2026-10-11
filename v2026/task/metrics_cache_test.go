// Independent collectors share only the storage protocol. Explicit clocks and
// held source reads force duplicate-production, expiry, and late-owner races.
package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Models atomic admission/publication and server-side expiry without sleeps.
type taskMetricsTestStore struct {
	stateLock    sync.Mutex
	clock        time.Time
	owner        string
	ownerExpires time.Time
	value        string
	valueExpires time.Time
	tokens       atomic.Uint64
	admissions   int
	publications int
	acquireErr   error
	publishErr   error
	applyOnError bool
	afterAcquire func()
}

// Uses a fixed synthetic clock independent of real context deadlines.
func newTaskMetricsTestStore() *taskMetricsTestStore {
	return &taskMetricsTestStore{clock: time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)}
}

// Provides the source observation clock to independent collectors.
func (self *taskMetricsTestStore) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.clock
}

// Moves expiry boundaries explicitly instead of depending on scheduler timing.
func (self *taskMetricsTestStore) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clock = self.clock.Add(duration)
}

// Reads the previous value in the same critical section as the ownership attempt.
func (self *taskMetricsTestStore) acquire(_ context.Context, token string) (string, bool, error) {
	self.stateLock.Lock()
	if !self.clock.Before(self.valueExpires) {
		self.value = ""
	}
	value, err, hook := self.value, self.acquireErr, self.afterAcquire
	available := !self.clock.Before(self.ownerExpires)
	if available && (err == nil || self.applyOnError) {
		self.owner, self.ownerExpires = token, self.clock.Add(taskMetricsRefreshInterval)
		self.admissions++
	}
	self.stateLock.Unlock()
	if hook != nil {
		hook()
	}
	return value, available && err == nil, err
}

// Publication consumes no new interval and never releases its admission fence.
func (self *taskMetricsTestStore) publish(_ context.Context, token, value string, ttl time.Duration) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.owner != token || !self.clock.Before(self.ownerExpires) {
		return false, nil
	}
	if self.publishErr == nil || self.applyOnError {
		self.value, self.valueExpires = value, self.clock.Add(ttl)
		self.publications++
	}
	return self.publishErr == nil, self.publishErr
}

// Every cache has distinct state and a distinct ownership token, as processes do.
func taskMetricsTestCache(store *taskMetricsTestStore, load func(context.Context, time.Time) (taskQueueMetricsSnapshot, error)) taskQueueMetricsCache {
	return taskQueueMetricsCache{
		store: store, now: store.now,
		newToken: func() string { return fmt.Sprintf("synthetic-owner-%d", store.tokens.Add(1)) },
		load:     load,
	}
}

// Only the owning source call is held; seven followers must finish before it.
func TestTaskQueueMetricsEightCollectorsShareOneHeldQuery(t *testing.T) {
	store := newTaskMetricsTestStore()
	original := store.now()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var queries atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	load := func(ctx context.Context, observedAt time.Time) (taskQueueMetricsSnapshot, error) {
		if queries.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			store.advance(2 * time.Second)
			return uniformTaskQueueSnapshot(7), nil
		case <-ctx.Done():
			return taskQueueMetricsSnapshot{}, ctx.Err()
		}
	}
	type result struct {
		sample *taskQueueMetricsSample
		err    error
	}
	collectors := make([]taskQueueMetricsCache, 8)
	results := make(chan result, len(collectors))
	var workers sync.WaitGroup
	defer workers.Wait()
	defer releaseOnce.Do(func() { close(release) })
	for i := range collectors {
		collectors[i] = taskMetricsTestCache(store, load)
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
		t.Fatal("no owner reached the held source read")
	}
	for range len(collectors) - 1 {
		select {
		case result := <-results:
			if result.err != nil || result.sample != nil {
				t.Fatal("cold follower invented a snapshot or treated admission refusal as failure")
			}
		case <-ctx.Done():
			t.Fatal("a follower waited for the source read")
		}
	}
	if queries.Load() != 1 {
		t.Fatalf("eight collectors issued %d source reads while the first was held", queries.Load())
	}
	releaseOnce.Do(func() { close(release) })
	var winner result
	select {
	case winner = <-results:
	case <-ctx.Done():
		t.Fatal("owner did not finish publication")
	}
	workers.Wait()
	if winner.err != nil || winner.sample == nil || !winner.sample.observedAt.Equal(original) {
		t.Fatal("source completion advanced the original observation clock", winner.err)
	}
	expires := store.valueExpires
	for _, cache := range collectors {
		sample, err := cache.get(ctx)
		if err != nil || sample == nil || sample.snapshot != winner.sample.snapshot || !sample.observedAt.Equal(original) {
			t.Fatal("shared read changed the source values or timestamp", err)
		}
	}
	if queries.Load() != 1 || store.admissions != 1 || store.publications != 1 ||
		!expires.Equal(original.Add(taskMetricsRetention)) || !store.valueExpires.Equal(expires) {
		t.Fatal("cache hits repeated source work, released admission, or extended source freshness")
	}
}

// An aligned follower sees the previous generation while the next owner fills.
func TestTaskQueueMetricsFollowerRetainsSnapshotDuringNextInterval(t *testing.T) {
	store := newTaskMetricsTestStore()
	cache := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
		return uniformTaskQueueSnapshot(3), nil
	})
	first, err := cache.get(t.Context())
	if err != nil || first == nil {
		t.Fatal("initial generation failed", err)
	}
	store.advance(taskMetricsRefreshInterval)
	var follower *taskQueueMetricsSample
	cache.load = func(ctx context.Context, observedAt time.Time) (taskQueueMetricsSnapshot, error) {
		peer := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
			t.Error("follower repeated an in-progress aggregate")
			return taskQueueMetricsSnapshot{}, nil
		})
		var err error
		follower, err = peer.get(ctx)
		return uniformTaskQueueSnapshot(4), err
	}
	second, err := cache.get(t.Context())
	if err != nil || second == nil || follower == nil || follower.snapshot != first.snapshot ||
		!follower.observedAt.Equal(first.observedAt) || second.snapshot.Total != 4 ||
		!second.observedAt.Equal(first.observedAt.Add(taskMetricsRefreshInterval)) {
		t.Fatal("aligned follower starved or freshened the previous generation", err)
	}
}

// A lost admission reply remains a bounded orphan, with no database fallback.
func TestTaskQueueMetricsAmbiguousAdmissionWaitsForExpiry(t *testing.T) {
	store := newTaskMetricsTestStore()
	store.acquireErr, store.applyOnError = errors.New("synthetic lost reply"), true
	queries := 0
	cache := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
		queries++
		return taskQueueMetricsSnapshot{}, nil
	})
	if sample, err := cache.get(t.Context()); sample != nil || err == nil || queries != 0 {
		t.Fatal("uncertain admission reached PostgreSQL")
	}
	store.acquireErr = nil
	store.advance(taskMetricsRefreshInterval - time.Nanosecond)
	if sample, err := cache.get(t.Context()); sample != nil || err != nil || queries != 0 {
		t.Fatal("orphaned ownership was retried before its expiry")
	}
	store.advance(time.Nanosecond)
	if sample, err := cache.get(t.Context()); sample == nil || err != nil || queries != 1 || sample.snapshot != (taskQueueMetricsSnapshot{}) {
		t.Fatal("orphan expiry failed to recover a legitimate empty queue", err)
	}
}

// Failure retains one interval of admission and the last completed source time.
func TestTaskQueueMetricsFailedQueryCannotCauseFollowerStampede(t *testing.T) {
	store := newTaskMetricsTestStore()
	queries := 0
	cache := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
		queries++
		return uniformTaskQueueSnapshot(2), nil
	})
	first, err := cache.get(t.Context())
	if err != nil || first == nil {
		t.Fatal("initial observation failed", err)
	}
	store.advance(taskMetricsRefreshInterval)
	cache.load = func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
		queries++
		return taskQueueMetricsSnapshot{}, errors.New("synthetic query failure")
	}
	sample, err := cache.get(t.Context())
	if err == nil || sample == nil || sample.snapshot != first.snapshot || !sample.observedAt.Equal(first.observedAt) {
		t.Fatal("failed query became a fresh zero or discarded retained evidence")
	}
	for range 7 {
		if sample, err := cache.get(t.Context()); err != nil || sample == nil || !sample.observedAt.Equal(first.observedAt) {
			t.Fatal("failed owner did not retain the original observation for followers")
		}
	}
	if queries != 2 || store.publications != 1 {
		t.Fatal("followers repeated a failed source query")
	}
	store.advance(taskMetricsRetention - taskMetricsRefreshInterval)
	if sample, err := cache.get(t.Context()); sample != nil || err == nil {
		t.Fatal("expired retained evidence became a fresh observation")
	}
}

// Publication uncertainty never triggers a duplicate source query. A successful
// but unacknowledged write can be recovered only by reading the stored generation.
func TestTaskQueueMetricsPublicationFailurePreservesUnknown(t *testing.T) {
	for _, applied := range []bool{false, true} {
		store := newTaskMetricsTestStore()
		store.publishErr, store.applyOnError = errors.New("synthetic publication failure"), applied
		queries := 0
		cache := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
			queries++
			return uniformTaskQueueSnapshot(8), nil
		})
		if sample, err := cache.get(t.Context()); sample != nil || err == nil {
			t.Fatalf("publication uncertainty became a local fresh value, applied=%v", applied)
		}
		store.publishErr = nil
		sample, err := cache.get(t.Context())
		if err != nil || (sample != nil) != applied || queries != 1 {
			t.Fatalf("ambiguous publication was recomputed or invented, applied=%v", applied)
		}
	}
}

// A paused query ignores cancellation until released, forcing a later owner to
// finish first. The retired query must not replace that completed generation.
func TestTaskQueueMetricsLateOwnerCannotReplaceNewerSnapshot(t *testing.T) {
	store := newTaskMetricsTestStore()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	old := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
		close(entered)
		<-release
		return uniformTaskQueueSnapshot(1), nil
	})
	done := make(chan struct{})
	var oldSample *taskQueueMetricsSample
	var oldErr error
	go func() { defer close(done); oldSample, oldErr = old.get(ctx) }()
	defer func() { releaseOnce.Do(func() { close(release) }); <-done }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("old owner did not reach source read")
	}
	store.advance(taskMetricsRefreshInterval)
	current := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
		return uniformTaskQueueSnapshot(9), nil
	})
	newSample, err := current.get(ctx)
	if err != nil || newSample == nil {
		t.Fatal("new owner could not publish after the orphan expired", err)
	}
	releaseOnce.Do(func() { close(release) })
	<-done
	if oldSample != nil || oldErr == nil || store.publications != 1 {
		t.Fatal("late query was published after its ownership interval")
	}
	retained, err := current.get(ctx)
	if err != nil || retained == nil || retained.snapshot != newSample.snapshot || !retained.observedAt.Equal(newSample.observedAt) {
		t.Fatal("retired owner replaced the newer snapshot")
	}
}

// Delayed admission must leave enough time for the bounded source read.
func TestTaskQueueMetricsDelayedAdmissionDoesNotStartExpiredWork(t *testing.T) {
	store := newTaskMetricsTestStore()
	store.afterAcquire = func() { store.advance(taskMetricsRefreshInterval - taskMetricsQueryTimeout) }
	cache := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
		t.Error("delayed owner began work that could outlive its admission")
		return taskQueueMetricsSnapshot{}, nil
	})
	if sample, err := cache.get(t.Context()); sample != nil || err == nil {
		t.Fatal("delayed admission did not remain unknown")
	}
}

// Missing fields, incompatible schema, invalid counts, and non-finite ages must
// not decode into plausible fresh zeros. Genuine zeros and original time survive.
func TestTaskQueueMetricsWireRejectsIncompleteOrInvalidSnapshots(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
	valid := fmt.Sprintf(`{"v":1,"t":%d,"n":[0,0,0,0],"o":0}`, now.UnixNano())
	for _, value := range []string{
		`{}`, `null`, `[]`, valid + `{}`, strings.Repeat(" ", taskMetricsWireBytes+1),
		strings.Replace(valid, `"v":1`, `"v":2`, 1),
		strings.Replace(valid, `"t":`+fmt.Sprint(now.UnixNano()), `"t":0`, 1),
		strings.Replace(valid, `"t":`+fmt.Sprint(now.UnixNano()), `"t":`+fmt.Sprint(now.Add(taskMetricsClockSkew+time.Nanosecond).UnixNano()), 1),
		strings.Replace(valid, `"n":[0,0,0,0]`, `"n":[0,0,0]`, 1),
		strings.Replace(valid, `"n":[0,0,0,0]`, `"n":[0,null,0,0]`, 1),
		strings.Replace(valid, `"n":[0,0,0,0]`, `"n":[0,0,0,0,0]`, 1),
		strings.Replace(valid, `"n":[0,0,0,0]`, `"n":[0,1,0,0]`, 1),
		strings.Replace(valid, `"n":[0,0,0,0]`, `"n":[0,0,-1,0]`, 1),
		strings.Replace(valid, `,"o":0`, ``, 1), strings.Replace(valid, `"o":0`, `"o":null`, 1),
		strings.Replace(valid, `"o":0`, `"o":-1`, 1), strings.Replace(valid, `"o":0`, `"o":1e1000`, 1),
		strings.Replace(valid, `"o":0`, `"o":1`, 1), strings.Replace(valid, `"o":0`, `"o":0,"unknown":0`, 1),
	} {
		if sample, err := decodeTaskQueueMetricsSample(value, now); sample != nil || err == nil {
			t.Fatal("incomplete or invalid observation decoded into business metrics")
		}
	}
	sample, err := decodeTaskQueueMetricsSample(valid, now.Add(time.Second))
	if err != nil || sample == nil || sample.snapshot != (taskQueueMetricsSnapshot{}) || !sample.observedAt.Equal(now) {
		t.Fatal("valid zero or nanosecond source clock was lost")
	}
}

// Readiness returns while the first query is held, and cancellation joins that
// observer without ever exporting default business gauges.
func TestTaskQueueMetricsStartupDoesNotWaitForObserver(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	guard, stopGuard := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopGuard()
	collector := newTaskQueueMetricsCollector()
	entered := make(chan struct{})
	returned := make(chan (<-chan struct{}), 1)
	go func() {
		returned <- startTaskQueueMetrics(ctx, func(ctx context.Context) (*taskQueueMetricsSample, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}, collector, func(error) {})
	}()
	select {
	case <-entered:
	case <-guard.Done():
		t.Fatal("observer never started")
	}
	var done <-chan struct{}
	select {
	case done = <-returned:
	case <-guard.Done():
		t.Fatal("worker startup waited for the observer read")
	}
	cancel()
	<-done
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil || len(families) != 0 {
		t.Fatal("missing observation became fresh zero metrics", err)
	}
}

// The collector lifecycle can publish retained evidence on error only with its
// original timestamp; a preexisting generation is not replaced with false zero.
func TestTaskQueueMetricsRefreshFailureRetainsSourceTimestamp(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	collector := newTaskQueueMetricsCollector()
	observedAt := time.Unix(1_800_000_000, 0)
	collector.publish(uniformTaskQueueSnapshot(6), observedAt)
	errorCount := 0
	done := startTaskQueueMetrics(ctx, func(context.Context) (*taskQueueMetricsSample, error) {
		return &taskQueueMetricsSample{snapshot: uniformTaskQueueSnapshot(6), observedAt: observedAt}, errTaskQueueMetricsUnavailable
	}, collector, func(error) { errorCount++; cancel() })
	<-done
	metrics := make(chan prometheus.Metric, 6)
	collector.Collect(metrics)
	close(metrics)
	values := []prometheus.Metric{}
	for metric := range metrics {
		values = append(values, metric)
	}
	requireTaskQueueGeneration(t, values, 6, observedAt)
	if errorCount != 1 {
		t.Fatal("refresh failure was not observed exactly once")
	}
}

// Malformed coordination and canceled callers remain unknown without admission
// retries or an independent database scan.
func TestTaskQueueMetricsInvalidStoreAndCancellationNeverFallBack(t *testing.T) {
	store := newTaskMetricsTestStore()
	store.value, store.valueExpires = `{}`, store.now().Add(taskMetricsRetention)
	cache := taskMetricsTestCache(store, func(context.Context, time.Time) (taskQueueMetricsSnapshot, error) {
		t.Error("unknown shared state triggered a database fallback")
		return taskQueueMetricsSnapshot{}, nil
	})
	if sample, err := cache.get(t.Context()); sample != nil || err == nil {
		t.Fatal("malformed shared state became a fresh zero")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if sample, err := cache.get(ctx); sample != nil || err == nil || store.admissions != 1 {
		t.Fatal("canceled observer acquired storage admission")
	}
}
