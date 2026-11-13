package model

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func subscriberCacheTestClock() (*atomic.Int64, func() time.Time) {
	clock := &atomic.Int64{}
	clock.Store(time.Second.Nanoseconds())
	return clock, func() time.Time { return time.Unix(0, clock.Load()) }
}

func TestSubscriberNegativeCacheExpiryStartsBeforeRead(t *testing.T) {
	clock, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	calls := 0
	read := func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		calls++
		if calls == 1 {
			clock.Add((900 * time.Millisecond).Nanoseconds())
			return map[server.Id]bool{id: true}, nil
		}
		return nil, nil // The formerly unknown connection is now verified.
	}
	for range 2 {
		got, err := c.lookup(t.Context(), []server.Id{id}, read)
		if err != nil || !got[id] {
			t.Fatal("fresh negative result missing", err)
		}
	}
	if calls != 1 {
		t.Fatal("repeated negative performed another read")
	}
	clock.Add((100 * time.Millisecond).Nanoseconds())
	got, err := c.lookup(t.Context(), []server.Id{id}, read)
	if err != nil || got[id] || calls != 2 {
		t.Fatal("negative outlived one second from read start", err)
	}
	got, err = c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		return map[server.Id]bool{id: true}, nil // A new unknown/risky live connection.
	})
	if err != nil || !got[id] {
		t.Fatal("positive decision was cached across new risk", err)
	}
}

func TestSubscriberNegativeCacheSlowReadAndErrorsNeverPoison(t *testing.T) {
	clock, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	want := errors.New("synthetic read failure")
	if _, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		return map[server.Id]bool{id: true}, want
	}); !errors.Is(err, want) || len(c.negative) != 0 || len(c.flights) != 0 {
		t.Fatal("partial failure became a cached fact")
	}
	_, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		clock.Add((2 * time.Second).Nanoseconds())
		return map[server.Id]bool{id: true}, nil
	})
	if err != nil || len(c.negative) != 0 {
		t.Fatal("slow query extended negative freshness", err)
	}
}

func TestSubscriberNegativeCacheConcurrentNegativeBatchCoalesces(t *testing.T) {
	_, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(1024, now)
	ids := make([]server.Id, 256)
	negative := make(map[server.Id]bool)
	for i := range ids {
		ids[i] = server.Id{byte(i), 1}
		negative[ids[i]] = true
	}
	var calls atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	read := func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return negative, nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.lookup(t.Context(), ids, read)
			if err == nil && len(got) != len(ids) {
				err = errors.New("incomplete negative batch")
			}
			errs <- err
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || len(c.flights) != 0 {
		t.Fatal("identical negative callers multiplied database work", calls.Load())
	}
}

func TestSubscriberNegativeCachePositiveFlightFollowersReadFresh(t *testing.T) {
	_, clock := subscriberCacheTestClock()
	entered := make(chan struct{})
	var ticks atomic.Int64
	c := newSubscriberNegativeCache(8, func() time.Time {
		if ticks.Add(1) == 3 { // Owner claim/start, then follower claim under mu.
			close(entered)
		}
		return clock()
	})
	id := server.Id{1}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	read := func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return nil, nil // Earlier snapshot: all live connections verified.
		}
		return map[server.Id]bool{id: true}, nil // New unknown connection.
	}
	owner := make(chan map[server.Id]bool, 1)
	go func() { got, _ := c.lookup(t.Context(), []server.Id{id}, read); owner <- got }()
	<-started
	follower := make(chan map[server.Id]bool, 1)
	go func() { got, _ := c.lookup(t.Context(), []server.Id{id}, read); follower <- got }()
	<-entered
	c.mu.Lock()
	c.mu.Unlock()
	close(release)
	if (<-owner)[id] || !(<-follower)[id] || calls.Load() != 2 {
		t.Fatal("follower reused a positive result after connection change")
	}
}

func TestSubscriberNegativeCacheResetDiscardsFactsAndOldFlights(t *testing.T) {
	_, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	read := func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		return map[server.Id]bool{id: true}, nil
	}
	_, _ = c.lookup(t.Context(), []server.Id{id}, read)
	c.reset() // Policy disabled/unavailable; re-enabling must read again.
	if len(c.negative) != 0 || len(c.expiry) != 0 {
		t.Fatal("policy transition retained a negative fact")
	}
	_, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		c.reset()
		return map[server.Id]bool{id: true}, nil
	})
	if err != nil || len(c.negative) != 0 || len(c.flights) != 0 {
		t.Fatal("old policy flight repopulated the cache", err)
	}
}

func TestSubscriberNegativeCacheCapacityAndCanceledCaller(t *testing.T) {
	_, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(2, now)
	events := map[string]int{}
	c.observe = func(event string, count int) { events[event] += count }
	ids := []server.Id{{1}, {2}, {3}}
	read := func(_ context.Context, requested []server.Id, _ func()) (map[server.Id]bool, error) {
		if len(c.flights) > 2 {
			t.Fatal("in-flight cache allocation exceeded its bound")
		}
		negative := make(map[server.Id]bool)
		for _, id := range requested {
			negative[id] = true
		}
		return negative, nil
	}
	got, err := c.lookup(t.Context(), ids, read)
	if err != nil || len(got) != 3 || len(c.negative) != 2 || len(c.expiry) != 2 || events["capacity_bypass"] != 1 || events["negative_miss"] != 3 {
		t.Fatal("capacity limited the result instead of only cache memory", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.lookup(ctx, ids, read); !errors.Is(err, context.Canceled) {
		t.Fatal("cached response ignored cancellation", err)
	}
}

func TestSubscriberNegativeCachePanicReleasesFlight(t *testing.T) {
	_, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	started, release, entered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c.observe = func(event string, _ int) {
		if event == "coalesced_wait" {
			close(entered)
		}
	}
	panicked := make(chan bool, 1)
	go func() {
		defer func() {
			panicked <- recover() != nil
		}()
		_, _ = c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
			close(started)
			<-release
			panic("synthetic acquisition failure")
		})
	}()
	<-started
	follower := make(chan error, 1)
	go func() {
		_, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
			return nil, errors.New("unexpected follower read")
		})
		follower <- err
	}()
	<-entered
	close(release)
	if !<-panicked {
		t.Fatal("read panic was swallowed")
	}
	select {
	case err := <-follower:
		if !errors.Is(err, errSubscriberReadIncomplete) {
			t.Fatal("panic did not fail the waiting reader", err)
		}
	case <-time.After(time.Second):
		t.Fatal("panic left a waiting reader blocked")
	}
	if len(c.flights) != 0 || len(c.negative) != 0 {
		t.Fatal("panic left a live flight or negative fact")
	}
}

func TestSubscriberNegativeCacheCanceledOwnerReleasesFlight(t *testing.T) {
	_, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(8, now)
	id := server.Id{1}
	started, entered := make(chan struct{}), make(chan struct{})
	c.observe = func(event string, _ int) {
		if event == "coalesced_wait" {
			close(entered)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	owner, follower := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := c.lookup(ctx, []server.Id{id}, func(ctx context.Context, _ []server.Id, _ func()) (map[server.Id]bool, error) {
			close(started)
			<-ctx.Done()
			return map[server.Id]bool{id: true}, ctx.Err()
		})
		owner <- err
	}()
	<-started
	go func() {
		_, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
			return nil, errors.New("unexpected follower read")
		})
		follower <- err
	}()
	<-entered
	cancel()
	for _, done := range []chan error{owner, follower} {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("canceled read did not release its waiter", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation left a waiting reader blocked")
		}
	}
	if len(c.flights) != 0 || len(c.negative) != 0 {
		t.Fatal("canceled partial result entered the cache")
	}
	got, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) { return nil, nil })
	if err != nil || got[id] {
		t.Fatal("canceled owner poisoned a subsequent eligible read", err)
	}
}

func TestSubscriberNegativeCacheCanceledFollowerDoesNotCancelOwner(t *testing.T) {
	_, clock := subscriberCacheTestClock()
	entered := make(chan struct{})
	var ticks atomic.Int64
	c := newSubscriberNegativeCache(8, func() time.Time {
		if ticks.Add(1) == 3 {
			close(entered)
		}
		return clock()
	})
	id := server.Id{1}
	started, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	read := func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		close(started)
		<-release
		return map[server.Id]bool{id: true}, nil
	}
	go func() { _, _ = c.lookup(t.Context(), []server.Id{id}, read); close(ownerDone) }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	follower := make(chan error, 1)
	go func() { _, err := c.lookup(ctx, []server.Id{id}, read); follower <- err }()
	<-entered
	c.mu.Lock()
	c.mu.Unlock()
	cancel()
	if err := <-follower; !errors.Is(err, context.Canceled) {
		t.Fatal("waiting caller ignored cancellation", err)
	}
	close(release)
	<-ownerDone
	got, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id, func()) (map[server.Id]bool, error) {
		t.Fatal("canceled follower discarded owner's completed refusal")
		return nil, nil
	})
	if err != nil || !got[id] {
		t.Fatal("owner was canceled by follower", err)
	}
}

func BenchmarkSubscriberNegativeCacheWarm256(b *testing.B) {
	_, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(1024, now)
	ids := make([]server.Id, 256)
	for i := range ids {
		ids[i] = server.Id{byte(i), 1}
	}
	var calls atomic.Int64
	read := func(_ context.Context, requested []server.Id, _ func()) (map[server.Id]bool, error) {
		calls.Add(1)
		negative := make(map[server.Id]bool)
		for _, id := range requested {
			negative[id] = true
		}
		return negative, nil
	}
	_, _ = c.lookup(b.Context(), ids, read)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := c.lookup(b.Context(), ids, read); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.StopTimer()
	if calls.Load() != 1 {
		b.Fatal("warm negatives reached the reader")
	}
	b.ReportMetric(float64(calls.Load()-1)/float64(b.N), "reads/op")
}

func subscriberCachePopulationId(n int) server.Id {
	var id server.Id
	binary.LittleEndian.PutUint64(id[:8], uint64(n+1))
	return id
}

func TestSubscriberNegativeCacheRandomOverlapEvictionAndExpiry(t *testing.T) {
	clock, now := subscriberCacheTestClock()
	c := newSubscriberNegativeCache(4096, now)
	events := map[string]int{}
	c.observe = func(event string, count int) { events[event] += count }
	rng := rand.New(rand.NewSource(1))
	reads, readCandidates, requestedCandidates := 0, 0, 0
	read := func(_ context.Context, ids []server.Id, _ func()) (map[server.Id]bool, error) {
		reads++
		readCandidates += len(ids)
		negative := make(map[server.Id]bool)
		for _, id := range ids {
			// Some candidates remain eligible; they must always be read again.
			if binary.LittleEndian.Uint64(id[:8])%1000 != 0 {
				negative[id] = true
			}
		}
		return negative, nil
	}
	for batch := range 512 {
		if batch%128 == 0 {
			clock.Add(time.Second.Nanoseconds())
		}
		ids := make([]server.Id, 256)
		unique := map[server.Id]bool{}
		for i := range ids {
			population := 85000
			if i%2 == 0 {
				population = 1024 // An overlapping hot subset plus broad cold work.
			}
			ids[i] = subscriberCachePopulationId(rng.Intn(population))
			unique[ids[i]] = true
		}
		requestedCandidates += len(unique)
		got, err := c.lookup(t.Context(), ids, read)
		if err != nil {
			t.Fatal(err)
		}
		for id := range unique {
			if got[id] != (binary.LittleEndian.Uint64(id[:8])%1000 != 0) {
				t.Fatal("random overlap changed eligibility")
			}
		}
		if len(c.negative) > c.capacity || len(c.expiry) != len(c.negative) || len(c.flights) != 0 {
			t.Fatal("cold population exceeded cache bounds or retained a flight")
		}
	}
	if events["negative_hit"] == 0 || events["negative_miss"] == 0 ||
		events["negative_hit"]+events["negative_miss"] != requestedCandidates ||
		events["negative_miss"] != readCandidates || readCandidates >= requestedCandidates {
		t.Fatal("random overlap counters do not describe candidate reads")
	}
	// A cold member in each request still causes a SQL batch: candidate hits
	// cannot be presented as the same reduction in completed SQL calls.
	if reads != 512 {
		t.Fatal("synthetic cold work unexpectedly avoided a reader batch", reads)
	}
	t.Logf("requests=%d requested_candidates=%d read_candidates=%d hits=%d capacity=%d", reads, requestedCandidates, readCandidates, events["negative_hit"], c.capacity)
}

func BenchmarkSubscriberNegativeCacheRandomPopulation256(b *testing.B) {
	for _, hotHalf := range []bool{false, true} {
		name := "uniform_85000"
		if hotHalf {
			name = "half_hot_1024"
		}
		b.Run(name, func(b *testing.B) {
			clock, now := subscriberCacheTestClock()
			c := newSubscriberNegativeCache(subscriberNegativeCapacity, now)
			rng := rand.New(rand.NewSource(2))
			reads, candidates := 0, 0
			read := func(_ context.Context, ids []server.Id, _ func()) (map[server.Id]bool, error) {
				reads++
				candidates += len(ids)
				negative := make(map[server.Id]bool, len(ids))
				for _, id := range ids {
					negative[id] = true
				}
				return negative, nil
			}
			b.ReportAllocs()
			b.ResetTimer()
			for batch := 0; batch < b.N; batch++ {
				// Thirty batches per process per second approximates a distributed
				// several-hundred-batch fleet. This is a workload control, not a
				// claim about Main's candidate distribution or wall-clock load.
				clock.Add((time.Second / 30).Nanoseconds())
				ids := make([]server.Id, 256)
				for i := range ids {
					population := 85000
					if hotHalf && i%2 == 0 {
						population = 1024
					}
					ids[i] = subscriberCachePopulationId(rng.Intn(population))
				}
				if _, err := c.lookup(b.Context(), ids, read); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(reads)/float64(b.N), "reads/op")
			b.ReportMetric(float64(candidates)/float64(b.N), "candidates/op")
		})
	}
}
