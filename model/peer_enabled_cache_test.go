// Controls source-read ownership at the peer admission cache. Every overlap
// is forced at a barrier; virtual time covers the existing decision ttl.
package model

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// Simultaneous residents and announces for one network share its cold scan.
// Another network can complete while that scan is still held outside the lock.
func TestPeersEnabledCacheCoalescesConcurrentNetworkLoads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &peersEnabledCache{entries: map[server.Id]networkPeersEnabledEntry{}}
		networkId, otherNetworkId := server.NewId(), server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseSource := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseSource()
		var reads atomic.Int32
		read := func() bool {
			if reads.Add(1) == 1 {
				close(entered)
			}
			<-release
			return true
		}
		const callerCount = 33
		results := make(chan bool, callerCount)
		go func() { results <- cache.GetOrLoad(t.Context(), networkId, read) }()
		<-entered
		for range callerCount - 1 {
			go func() { results <- cache.GetOrLoad(t.Context(), networkId, read) }()
		}
		otherResult := make(chan bool, 1)
		go func() {
			otherResult <- cache.GetOrLoad(t.Context(), otherNetworkId, func() bool { return false })
		}()
		synctest.Wait()
		if got := reads.Load(); got != 1 {
			t.Errorf("one cold network started %d source reads, want 1", got)
		}
		if len(otherResult) != 1 {
			t.Error("one network's source read blocked another network")
		}
		releaseSource()
		for range callerCount {
			if !<-results {
				t.Error("coalesced caller lost the successful decision")
			}
		}
		if <-otherResult {
			t.Error("unrelated network inherited the positive decision")
		}
		for _, id := range []server.Id{networkId, otherNetworkId} {
			if got := cache.GetOrLoad(t.Context(), id, func() bool {
				t.Error("completed decision started another source read")
				return false
			}); got != (id == networkId) {
				t.Error("completed cache decision changed")
			}
		}
	})
}

// A canceled follower releases only its own wait; the first caller still owns
// its source read and can publish a decision for a later admission.
func TestPeersEnabledCacheCanceledFollowerLeavesOwnerRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &peersEnabledCache{entries: map[server.Id]networkPeersEnabledEntry{}}
		networkId := server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		result := make(chan bool, 1)
		go func() {
			result <- cache.GetOrLoad(t.Context(), networkId, func() bool {
				close(entered)
				<-release
				return true
			})
		}()
		<-entered
		waitCtx, cancelWait := context.WithCancel(t.Context())
		defer cancelWait()
		waitResult := make(chan any, 1)
		go func() {
			waitResult <- server.HandleError(func() {
				cache.GetOrLoad(waitCtx, networkId, func() bool {
					t.Error("follower started a second source read")
					return false
				})
			})
		}()
		synctest.Wait()
		cancelWait()
		synctest.Wait()
		if len(waitResult) != 1 {
			t.Error("canceled follower waited for the source owner")
		}
		close(release)
		if cause, ok := (<-waitResult).(error); !ok || !errors.Is(cause, context.Canceled) {
			t.Errorf("follower cancellation returned %v", cause)
		}
		if !<-result {
			t.Error("follower cancellation changed the owner's decision")
		}
		if enabled, cached := cache.Get(networkId); !cached || !enabled {
			t.Error("follower cancellation removed the completed decision")
		}
	})
}

// Source panics must complete the load marker. Waiting callers retry under
// their own contexts instead of inheriting a failure or waiting forever.
func TestPeersEnabledCachePanickingOwnerWakesFollowers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &peersEnabledCache{entries: map[server.Id]networkPeersEnabledEntry{}}
		networkId := server.NewId()
		entered, release := make(chan struct{}), make(chan struct{})
		sourceErr := errors.New("synthetic peer count failure")
		failedResult := make(chan any, 1)
		go func() {
			failedResult <- server.HandleError(func() {
				cache.GetOrLoad(t.Context(), networkId, func() bool {
					close(entered)
					<-release
					panic(sourceErr)
				})
			})
		}()
		<-entered
		const followerCount = 16
		results := make(chan bool, followerCount)
		var retriedReads atomic.Int32
		for range followerCount {
			go func() {
				results <- cache.GetOrLoad(t.Context(), networkId, func() bool {
					retriedReads.Add(1)
					return true
				})
			}()
		}
		synctest.Wait()
		if len(results) != 0 || retriedReads.Load() != 0 {
			t.Error("followers bypassed the in-progress owner")
		}
		close(release)
		if cause, ok := (<-failedResult).(error); !ok || !errors.Is(cause, sourceErr) {
			t.Errorf("source failure changed: %v", cause)
		}
		for range followerCount {
			if !<-results {
				t.Error("source failure became a cached denial")
			}
		}
		if got := retriedReads.Load(); got != 1 {
			t.Errorf("woken followers performed %d replacement reads, want 1", got)
		}
	})
}

// Canceling the original source request also releases the generation. A live
// follower can re-count without using the canceled source context.
func TestPeersEnabledCacheCanceledOwnerLetsFollowerRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &peersEnabledCache{entries: map[server.Id]networkPeersEnabledEntry{}}
		networkId := server.NewId()
		ownerCtx, cancelOwner := context.WithCancel(t.Context())
		defer cancelOwner()
		entered := make(chan struct{})
		ownerResult := make(chan any, 1)
		go func() {
			ownerResult <- server.HandleError(func() {
				cache.GetOrLoad(ownerCtx, networkId, func() bool {
					close(entered)
					<-ownerCtx.Done()
					server.Raise(ownerCtx.Err())
					return false
				})
			})
		}()
		<-entered
		followerResult := make(chan bool, 1)
		go func() {
			followerResult <- cache.GetOrLoad(t.Context(), networkId, func() bool { return true })
		}()
		synctest.Wait()
		if len(followerResult) != 0 {
			t.Error("follower bypassed the original source owner")
		}
		cancelOwner()
		if cause, ok := (<-ownerResult).(error); !ok || !errors.Is(cause, context.Canceled) {
			t.Errorf("source cancellation returned %v", cause)
		}
		if !<-followerResult {
			t.Error("live follower inherited the original source cancellation")
		}
	})
}

// A committed ACL change invalidates the old source snapshot. Its completion
// cannot overwrite a newer decision, and its followers may re-count promptly.
func TestPeersEnabledCacheInvalidationFencesActiveLoads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, clearAll := range []bool{false, true} {
			cache := &peersEnabledCache{entries: map[server.Id]networkPeersEnabledEntry{}}
			networkId := server.NewId()
			entered, release := make(chan struct{}), make(chan struct{})
			oldResult := make(chan bool, 1)
			go func() {
				oldResult <- cache.GetOrLoad(t.Context(), networkId, func() bool {
					close(entered)
					<-release
					return true
				})
			}()
			<-entered
			newResult := make(chan bool, 1)
			go func() {
				newResult <- cache.GetOrLoad(t.Context(), networkId, func() bool { return false })
			}()
			synctest.Wait()
			if len(newResult) != 0 {
				t.Error("follower re-counted before invalidation")
			}
			if clearAll {
				cache.Clear()
			} else {
				cache.Remove(networkId)
			}
			synctest.Wait()
			if len(newResult) != 1 {
				t.Error("invalidated follower still waited for the stale source owner")
			}
			close(release)
			if !<-oldResult || <-newResult {
				t.Error("source generation returned another generation's result")
			}
			if enabled, cached := cache.Get(networkId); !cached || enabled {
				t.Errorf("stale owner overwrote the new decision after clearAll=%t", clearAll)
			}
		}
	})
}

// Successful completion starts the five-minute ttl; an exact expiry starts
// one new read, and negative decisions retain the same caching behavior.
func TestPeersEnabledCacheExpiresFromSuccessfulCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := &peersEnabledCache{entries: map[server.Id]networkPeersEnabledEntry{}}
		networkId := server.NewId()
		if !cache.GetOrLoad(t.Context(), networkId, func() bool {
			time.Sleep(time.Minute)
			return true
		}) {
			t.Fatal("successful first read returned a denial")
		}
		time.Sleep(networkPeersEnabledTtl - time.Nanosecond)
		if enabled, cached := cache.Get(networkId); !cached || !enabled {
			t.Fatal("decision expired before its completion-relative deadline")
		}
		time.Sleep(time.Nanosecond)
		reads := 0
		for range 2 {
			if cache.GetOrLoad(t.Context(), networkId, func() bool { reads++; return false }) {
				t.Error("expired positive decision survived the new denial")
			}
		}
		if reads != 1 {
			t.Errorf("negative decision performed %d source reads, want 1", reads)
		}
	})
}

// The production model entry uses the cache and releases its source owner
// when the real PostgreSQL acquisition guard raises. This needs no database.
func TestNetworkPeersEnabledSourceFailureDoesNotRetainLoad(t *testing.T) {
	networkId := server.NewId()
	defer networkPeersEnabledCache.Remove(networkId)
	ctx := server.WithoutPostgres(t.Context())
	networkPeersEnabledCache.Put(networkId, true)
	if !NetworkPeersEnabled(ctx, networkId) || server.PacketPostgresAttempts(ctx) != 0 {
		t.Fatal("warm peer admission reached the source")
	}
	networkPeersEnabledCache.Remove(networkId)
	for range 2 {
		result := server.HandleError(func() { NetworkPeersEnabled(ctx, networkId) })
		if cause, ok := result.(error); !ok || !errors.Is(cause, server.ErrPacketPostgres) {
			t.Fatalf("real source guard failure changed: %v", result)
		}
	}
	if got := server.PacketPostgresAttempts(ctx); got != 2 {
		t.Errorf("source failure retained a cached decision: attempts=%d", got)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	result := server.HandleError(func() { NetworkPeersEnabled(canceledCtx, networkId) })
	if cause, ok := result.(error); !ok || !errors.Is(cause, context.Canceled) || server.PacketPostgresAttempts(ctx) != 2 {
		t.Fatalf("canceled admission started another source read: %v", result)
	}
}
