package router

// handler_cache_wait_test.go pins what a miss does while another request's
// lease covers a fill in progress. The apps send one GET over several
// connections at once and take the first answer, so these misses are the
// normal case for a cold key. Answered "503 Cached response is temporarily
// unavailable" at once, they were 9% of GET /network/reliability and a third
// of GET /transfer/stats on 2026-11-11. Every test runs on a synctest
// bubble's clock: fills and polls take no real time.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Counts the reads a waiting miss makes.
type handlerCacheCountingStore struct {
	*handlerCacheExpiringTestStore
	reads atomic.Int64
}

func (self *handlerCacheCountingStore) get(ctx context.Context, key string) (string, bool, error) {
	self.reads.Add(1)
	return self.handlerCacheExpiringTestStore.get(ctx, key)
}

// Overlapping misses get the owner's value as soon as it is published, one
// poll late at most, and make no fill of their own: the database still sees
// one query per cold key.
func TestHandlerCacheOverlappingMissesWaitForTheFillInProgress(t *testing.T) {
	// a reliability fill, a typical and a slow transfer stats fill
	for _, fillTime := range []time.Duration{5 * time.Millisecond, 400 * time.Millisecond, handlerCacheFillWaitLimit - time.Millisecond} {
		t.Run(fillTime.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newHandlerCacheExpiringTestStore()
				ctx := context.Background()
				key := "synthetic-overlapping"
				ttl := 10 * time.Second
				var fills atomic.Int64
				started := make(chan struct{})
				owner := make(chan handlerCacheTestCallResult, 1)
				go func() {
					value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
						fills.Add(1)
						close(started)
						time.Sleep(fillTime)
						return &handlerCacheTestResult{Message: "owner"}, nil
					})
					owner <- handlerCacheTestCallResult{value, err}
				}()
				<-started
				start := time.Now()
				const misses = 3
				var wg sync.WaitGroup
				results := make([]handlerCacheTestCallResult, misses)
				answered := make([]time.Duration, misses)
				for i := range misses {
					wg.Add(1)
					go func() {
						defer wg.Done()
						value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
							fills.Add(1)
							return &handlerCacheTestResult{Message: "duplicate"}, nil
						})
						results[i] = handlerCacheTestCallResult{value, err}
						answered[i] = time.Since(start)
					}()
				}
				wg.Wait()
				if result := <-owner; result.err != nil || result.value == nil || result.value.Message != "owner" {
					t.Fatalf("owner = (%v, %v), want its value", result.value, result.err)
				}
				for i, result := range results {
					if result.err != nil || result.value == nil || result.value.Message != "owner" {
						t.Fatalf("overlapping miss %d of a %v fill = (%v, %v), want the owner's value", i, fillTime, result.value, result.err)
					}
					if answered[i] < fillTime || fillTime+handlerCacheFillWaitMaxPoll < answered[i] {
						t.Fatalf("overlapping miss %d of a %v fill answered after %v, want within one poll of the publication", i, fillTime, answered[i])
					}
				}
				if got := fills.Load(); got != 1 {
					t.Fatalf("fills = %d, want the owner's only", got)
				}
			})
		})
	}
}

// A waiting miss reads at a backoff to a ceiling, so the misses of a slow or
// stalled fill add a bounded read rate per miss to the key's Redis node.
func TestHandlerCacheWaitingMissReadsAtABackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &handlerCacheCountingStore{handlerCacheExpiringTestStore: newHandlerCacheExpiringTestStore()}
		ctx := context.Background()
		key := "synthetic-stalled"
		ttl := time.Minute
		started, finish := make(chan struct{}), make(chan struct{})
		owner := make(chan handlerCacheTestCallResult, 1)
		go func() {
			value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
				close(started)
				<-finish
				return &handlerCacheTestResult{Message: "owner"}, nil
			})
			owner <- handlerCacheTestCallResult{value, err}
		}()
		<-started
		before := store.reads.Load()
		asked := time.Now()
		value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
			t.Fatal("a miss filled under another request's lease")
			return nil, nil
		})
		if value != nil || !isHandlerCacheUnavailable(err) {
			t.Fatalf("miss of a stalled fill = (%v, %v), want 503", value, err)
		}
		if waited := time.Since(asked); waited != handlerCacheFillWaitLimit {
			t.Fatalf("miss of a stalled fill answered after %v, want after the %v wait limit", waited, handlerCacheFillWaitLimit)
		}
		// first look: value, lease, value; then a value and a lease read per poll
		polls := int64(0)
		for elapsed, poll := time.Duration(0), handlerCacheFillWaitFirstPoll; elapsed < handlerCacheFillWaitLimit; poll = min(2*poll, handlerCacheFillWaitMaxPoll) {
			elapsed += poll
			polls += 1
		}
		if reads, most := store.reads.Load()-before, 3+2*polls; reads > most {
			t.Fatalf("a %v wait made %d reads, want at most %d", handlerCacheFillWaitLimit, reads, most)
		}
		close(finish)
		<-owner
	})
}

// A waiting miss stops with its caller and never fills.
func TestHandlerCacheWaitingMissStopsWithItsCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newHandlerCacheExpiringTestStore()
		key := "synthetic-caller-stop"
		ttl := time.Minute
		started, finish := make(chan struct{}), make(chan struct{})
		owner := make(chan handlerCacheTestCallResult, 1)
		go func() {
			value, err := cacheJson(context.Background(), store, key, ttl, func() (*handlerCacheTestResult, error) {
				close(started)
				<-finish
				return &handlerCacheTestResult{Message: "owner"}, nil
			})
			owner <- handlerCacheTestCallResult{value, err}
		}()
		<-started
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(300 * time.Millisecond)
			cancel()
		}()
		asked := time.Now()
		value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
			t.Fatal("a stopped miss filled")
			return nil, nil
		})
		if value != nil || !isHandlerCacheUnavailable(err) || time.Since(asked) != 300*time.Millisecond {
			t.Fatalf("stopped miss = (%v, %v) after %v, want 503 when its caller stopped", value, err, time.Since(asked))
		}
		close(finish)
		if result := <-owner; result.err != nil || result.value == nil {
			t.Fatalf("owner = (%v, %v), want its value", result.value, result.err)
		}
	})
}

// A lease that lapses with no value, as when its owner outlives it, leaves
// the waiting miss to take a new lease and fill, as a miss after the lapse
// would. The lapsed owner cannot overwrite that value.
func TestHandlerCacheWaitingMissTakesOverALapsedLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newHandlerCacheExpiringTestStore()
		ctx := context.Background()
		key := "synthetic-lapsed"
		ttl := 2 * time.Second
		lease := handlerCacheFillLeaseTtl(ttl)
		started, finish := make(chan struct{}), make(chan struct{})
		owner := make(chan handlerCacheTestCallResult, 1)
		go func() {
			value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
				close(started)
				<-finish
				return &handlerCacheTestResult{Message: "lapsed"}, nil
			})
			owner <- handlerCacheTestCallResult{value, err}
		}()
		<-started
		asked := time.Now()
		value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
			return &handlerCacheTestResult{Message: "takeover"}, nil
		})
		if err != nil || value == nil || value.Message != "takeover" {
			t.Fatalf("miss of a lapsed lease = (%v, %v), want its own fill", value, err)
		}
		if waited := time.Since(asked); waited < lease || lease+handlerCacheFillWaitMaxPoll < waited {
			t.Fatalf("miss of a lapsed lease filled after %v, want within one poll of the %v lapse", waited, lease)
		}
		close(finish)
		if result := <-owner; result.err != nil || result.value == nil || result.value.Message != "takeover" {
			t.Fatalf("lapsed owner = (%v, %v), want the takeover's published value", result.value, result.err)
		}
	})
}
