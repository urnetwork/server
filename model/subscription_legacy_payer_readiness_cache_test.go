package model

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"
)

func TestLegacyPayerDueIndexUsesOnlyLiveMatchingPositiveObservation(t *testing.T) {
	identity := sha256.Sum256([]byte("synthetic database one"))
	other := sha256.Sum256([]byte("synthetic database two"))
	now := time.Unix(100, 0)
	cache := &legacySettlementPayerIndexCache{}
	if cache.hasReadyObservation(now) {
		t.Fatal("empty cache would add a resource read")
	}
	checks := 0
	if !cache.load(t.Context(), identity, func() time.Time { return now }, func(context.Context) (bool, error) {
		checks++
		return true, nil
	}) || checks != 1 {
		t.Fatal("initial full-index validation failed")
	}
	for _, test := range []struct {
		name     string
		identity [sha256.Size]byte
		at       time.Time
		ready    bool
	}{
		{name: "same valid resource", identity: identity, at: now, ready: true},
		{name: "last valid instant", identity: identity, at: now.Add(legacySettlementPayerIndexCacheLifetime - time.Nanosecond), ready: true},
		{name: "exact expiry", identity: identity, at: now.Add(legacySettlementPayerIndexCacheLifetime)},
		{name: "different resource", identity: other, at: now},
	} {
		beforeExpiry := cache.expires
		got := cache.readyObservation(t.Context(), test.identity, test.at)
		if got != test.ready || cache.expires != beforeExpiry || checks != 1 {
			t.Fatalf("%s: cache read changed readiness, expiry or probe count: got=%t want=%t checks=%d", test.name, got, test.ready, checks)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if cache.readyObservation(canceled, identity, now) {
		t.Fatal("canceled caller used schema readiness")
	}
	cache.ready = false
	if cache.hasReadyObservation(now) || cache.readyObservation(t.Context(), identity, now) {
		t.Fatal("negative observation became ready")
	}
}

// RED is the same refused fresh probe without a known positive observation.
// GREEN reuses the exact still-live observation and performs no second probe.
func TestLegacyPayerDueIndexPositiveObservationAvoidsTransientRefusal(t *testing.T) {
	identity := sha256.Sum256([]byte("synthetic database"))
	now := time.Unix(100, 0)
	cache := &legacySettlementPayerIndexCache{identity: identity, ready: true,
		expires: now.Add(legacySettlementPayerIndexCacheLifetime)}
	readCalls := 0
	read := func(context.Context) (bool, error) {
		readCalls++
		return false, context.DeadlineExceeded
	}
	uncached := observeLegacySettlementPayerDueIndex(t.Context(), read, time.Now)
	if uncached.Outcome != "deadline" || readCalls != 1 {
		t.Fatal("transient-refusal control did not refuse", uncached, readCalls)
	}
	observed := observeLegacySettlementPayerIndexWithCache(t.Context(), func(ctx context.Context) bool {
		return cache.readyObservation(ctx, identity, now)
	}, read, time.Now)
	if observed.Outcome != "ready" || !observed.Cached || readCalls != 1 {
		t.Fatal("validated cache still entered the refused catalog probe", observed, readCalls)
	}
	cache.expires = now
	observed = observeLegacySettlementPayerIndexWithCache(t.Context(), func(ctx context.Context) bool {
		return cache.readyObservation(ctx, identity, now)
	}, read, time.Now)
	if observed.Outcome != "deadline" || observed.Cached || readCalls != 2 {
		t.Fatal("expired observation suppressed the ordinary bounded probe", observed, readCalls)
	}
}

func TestLegacyPayerDueIndexConcurrentRefreshPreservesExactLiveObservation(t *testing.T) {
	identity := sha256.Sum256([]byte("synthetic database one"))
	other := sha256.Sum256([]byte("synthetic database two"))
	now := time.Unix(100, 0)
	cache := &legacySettlementPayerIndexCache{identity: identity, ready: true,
		expires: now.Add(legacySettlementPayerIndexCacheLifetime)}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		done <- cache.load(t.Context(), other, func() time.Time { return now }, func(context.Context) (bool, error) {
			close(entered)
			<-release
			return true, nil
		})
	}()
	<-entered
	live := cache.readyObservation(t.Context(), identity, now)
	unproved := cache.readyObservation(t.Context(), other, now)
	expired := cache.readyObservation(t.Context(), identity, now.Add(legacySettlementPayerIndexCacheLifetime))
	close(release)
	refreshed := <-done
	if !live || unproved || expired || !refreshed || cache.readyObservation(t.Context(), identity, now) || !cache.readyObservation(t.Context(), other, now) {
		t.Fatal("concurrent refresh widened, suppressed or extended readiness", live, unproved, expired, refreshed)
	}
}
