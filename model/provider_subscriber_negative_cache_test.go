package model

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
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
	read := func(context.Context, []server.Id) (map[server.Id]bool, error) {
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
	got, err = c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id) (map[server.Id]bool, error) {
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
	if _, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id) (map[server.Id]bool, error) {
		return map[server.Id]bool{id: true}, want
	}); !errors.Is(err, want) || len(c.negative) != 0 || len(c.flights) != 0 {
		t.Fatal("partial failure became a cached fact")
	}
	_, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id) (map[server.Id]bool, error) {
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
	read := func(context.Context, []server.Id) (map[server.Id]bool, error) {
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
	read := func(context.Context, []server.Id) (map[server.Id]bool, error) {
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
	read := func(context.Context, []server.Id) (map[server.Id]bool, error) {
		return map[server.Id]bool{id: true}, nil
	}
	_, _ = c.lookup(t.Context(), []server.Id{id}, read)
	c.reset() // Policy disabled/unavailable; re-enabling must read again.
	if len(c.negative) != 0 || len(c.expiry) != 0 {
		t.Fatal("policy transition retained a negative fact")
	}
	_, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id) (map[server.Id]bool, error) {
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
	ids := []server.Id{{1}, {2}, {3}}
	read := func(_ context.Context, requested []server.Id) (map[server.Id]bool, error) {
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
	if err != nil || len(got) != 3 || len(c.negative) != 2 || len(c.expiry) != 2 {
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
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("read panic was swallowed")
			}
		}()
		_, _ = c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id) (map[server.Id]bool, error) { panic("synthetic") })
	}()
	if len(c.flights) != 0 || len(c.negative) != 0 {
		t.Fatal("panic left a live flight or negative fact")
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
	read := func(context.Context, []server.Id) (map[server.Id]bool, error) {
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
	got, err := c.lookup(t.Context(), []server.Id{id}, func(context.Context, []server.Id) (map[server.Id]bool, error) {
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
	read := func(_ context.Context, requested []server.Id) (map[server.Id]bool, error) {
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
