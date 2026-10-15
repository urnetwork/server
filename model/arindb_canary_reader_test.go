// The request-time reader of the published canary decision, with an injected
// clock, store and barriers (arindbctl/ARINDB.md, "Publication and
// request-time consistency").
package model

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A controllable clock for the cache.
type testArindbClock struct {
	now time.Time
}

func (self *testArindbClock) read() time.Time {
	return self.now
}

// One process reads the decision at most once per ttl and serves the read
// value in between; a stale cache reads again.
func TestArindbServingEpochCacheReadsOncePerTtl(t *testing.T) {
	clock := &testArindbClock{now: testCanaryClock}
	cache := &arindbServingEpochCache{}
	var reads atomic.Int32
	epoch := testCanaryOldEpoch
	read := func(ctx context.Context) (int64, string) {
		reads.Add(1)
		return epoch, ArindbDecisionReadOk
	}
	if got := cache.get(t.Context(), clock.read, read); got != testCanaryOldEpoch || reads.Load() != 1 {
		t.Fatalf("first read %d after %d reads", got, reads.Load())
	}
	epoch = testCanaryLatestEpoch
	clock.now = clock.now.Add(arindbServingEpochCacheTtl - time.Millisecond)
	if got := cache.get(t.Context(), clock.read, read); got != testCanaryOldEpoch || reads.Load() != 1 {
		t.Fatalf("within the ttl: %d after %d reads", got, reads.Load())
	}
	clock.now = clock.now.Add(time.Millisecond)
	if got := cache.get(t.Context(), clock.read, read); got != testCanaryLatestEpoch || reads.Load() != 2 {
		t.Fatalf("after the ttl: %d after %d reads", got, reads.Load())
	}
}

// While one reader refreshes a stale value, the others serve the previous
// value instead of reading again or waiting.
func TestArindbServingEpochRefreshUnderWayServesThePreviousValue(t *testing.T) {
	clock := &testArindbClock{now: testCanaryClock}
	cache := &arindbServingEpochCache{}
	cache.get(t.Context(), clock.read, func(context.Context) (int64, string) {
		return testCanaryOldEpoch, ArindbDecisionReadOk
	})
	clock.now = clock.now.Add(arindbServingEpochCacheTtl)
	started := make(chan struct{})
	release := make(chan struct{})
	refreshed := make(chan int64)
	var reads atomic.Int32
	go func() {
		refreshed <- cache.get(context.Background(), clock.read, func(context.Context) (int64, string) {
			reads.Add(1)
			close(started)
			<-release
			return testCanaryLatestEpoch, ArindbDecisionReadOk
		})
	}()
	<-started
	if got := cache.get(t.Context(), clock.read, func(context.Context) (int64, string) {
		reads.Add(1)
		return 0, ArindbDecisionReadError
	}); got != testCanaryOldEpoch || reads.Load() != 1 {
		t.Fatalf("during a refresh: %d after %d reads, want the previous value without a read", got, reads.Load())
	}
	close(release)
	if got := <-refreshed; got != testCanaryLatestEpoch {
		t.Fatalf("refresh returned %d", got)
	}
	if got := cache.get(t.Context(), clock.read, nil); got != testCanaryLatestEpoch {
		t.Fatalf("after the refresh: %d", got)
	}
}

// A read that panics is recorded as an error, which keeps the last decision
// read, and releases the refresh, so the cache reads again after the ttl
// instead of serving its previous value forever.
func TestArindbServingEpochAPanickingReadNeverStrandsTheCache(t *testing.T) {
	clock := &testArindbClock{now: testCanaryClock}
	cache := &arindbServingEpochCache{}
	cache.get(t.Context(), clock.read, func(context.Context) (int64, string) {
		return testCanaryOldEpoch, ArindbDecisionReadOk
	})
	clock.now = clock.now.Add(arindbServingEpochCacheTtl)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the read's panic was swallowed")
			}
		}()
		cache.get(t.Context(), clock.read, func(context.Context) (int64, string) {
			panic("synthetic read failure")
		})
	}()
	if got := cache.get(t.Context(), clock.read, nil); got != testCanaryOldEpoch {
		t.Fatalf("after a panicking read: %d, want the last decision read", got)
	}
	clock.now = clock.now.Add(arindbServingEpochCacheTtl)
	if got := cache.get(t.Context(), clock.read, func(context.Context) (int64, string) {
		return testCanaryLatestEpoch, ArindbDecisionReadOk
	}); got != testCanaryLatestEpoch {
		t.Fatalf("the cache did not read again after the ttl: %d", got)
	}
}

// The shared read is bounded and outlives the request that triggers it: a
// canceled request cannot make every reader serve the latest slot.
func TestArindbServingEpochReadIgnoresTheTriggeringRequestsCancellation(t *testing.T) {
	cache := &arindbServingEpochCache{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	got := cache.get(canceled, func() time.Time { return testCanaryClock }, func(ctx context.Context) (int64, string) {
		deadline, bounded := ctx.Deadline()
		if ctx.Err() != nil || !bounded || arindbDecisionReadTimeout < time.Until(deadline) {
			return 0, ArindbDecisionReadError
		}
		return testCanaryOldEpoch, ArindbDecisionReadOk
	})
	if got != testCanaryOldEpoch {
		t.Fatalf("a canceled request's read served %d", got)
	}
}

// Without a readable decision the reader serves each row's latest slot (0)
// and counts why: absent, invalid or a store error. A readable decision
// serves its epoch.
func TestArindbServingEpochUnreadableDecisionServesTheLatestSlot(t *testing.T) {
	store := useTestArindbCanaryDecisionStore(t)
	fallback := decideTestCanary(testCanaryPair(), testCanaryProviders(10, "aa", true, testCanaryNonQuality, testCanaryClean), defaultArindbCanarySettings())
	valid, err := json.Marshal(fallback)
	if err != nil {
		t.Fatal(err)
	}
	var future map[string]any
	if err := json.Unmarshal(valid, &future); err != nil {
		t.Fatal(err)
	}
	future["version"] = ArindbCanaryDecisionVersion + 1
	futureData, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		value   []byte
		err     error
		epoch   int64
		outcome string
	}{
		{name: "absent or expired", outcome: ArindbDecisionReadAbsent},
		{name: "malformed", value: []byte("{not json"), outcome: ArindbDecisionReadInvalid},
		{name: "another version", value: futureData, outcome: ArindbDecisionReadInvalid},
		{name: "a store error", err: errors.New("synthetic store failure"), outcome: ArindbDecisionReadError},
		{name: "a readable fallback", value: valid, epoch: testCanaryOldEpoch, outcome: ArindbDecisionReadOk},
	} {
		store.value, store.err = c.value, c.err
		before := testutil.ToFloat64(arindbDecisionReads.WithLabelValues(c.outcome))
		cache := &arindbServingEpochCache{}
		got := cache.get(t.Context(), func() time.Time { return testCanaryClock }, readArindbServingEpochFromStore)
		if got != c.epoch {
			t.Errorf("%s: served %d, want %d", c.name, got, c.epoch)
		}
		if after := testutil.ToFloat64(arindbDecisionReads.WithLabelValues(c.outcome)); after != before+1 {
			t.Errorf("%s: %s reads went %v to %v", c.name, c.outcome, before, after)
		}
	}
}

// A Redis error or timeout keeps serving the last decision the process read,
// so a blip cannot drop an active fallback, but only for a bounded time; a
// decision that is absent, expired or invalid serves each row's latest slot at
// once, and a later error cannot bring it back.
func TestArindbServingEpochKeepsTheLastDecisionThroughReadErrors(t *testing.T) {
	clock := &testArindbClock{now: testCanaryClock}
	cache := &arindbServingEpochCache{}
	read := func(epoch int64, outcome string) func(context.Context) (int64, string) {
		return func(context.Context) (int64, string) { return epoch, outcome }
	}
	step := func(name string, outcome string, epoch int64, want int64) {
		t.Helper()
		clock.now = clock.now.Add(arindbServingEpochCacheTtl)
		if got := cache.get(t.Context(), clock.read, read(epoch, outcome)); got != want {
			t.Fatalf("%s: served %d, want %d", name, got, want)
		}
	}
	step("a fallback", ArindbDecisionReadOk, testCanaryOldEpoch, testCanaryOldEpoch)
	lastOk := clock.now
	step("a Redis error", ArindbDecisionReadError, 0, testCanaryOldEpoch)
	step("a timeout", ArindbDecisionReadError, 0, testCanaryOldEpoch)
	// past the hold, errors serve each row's latest slot
	clock.now = lastOk.Add(arindbServingEpochErrorHold - arindbServingEpochCacheTtl)
	step("an error at the hold", ArindbDecisionReadError, 0, 0)
	step("a fallback again", ArindbDecisionReadOk, testCanaryOldEpoch, testCanaryOldEpoch)
	step("an expired decision", ArindbDecisionReadAbsent, 0, 0)
	step("an error after expiry", ArindbDecisionReadError, 0, 0)
	step("a fallback once more", ArindbDecisionReadOk, testCanaryOldEpoch, testCanaryOldEpoch)
	step("an invalid decision", ArindbDecisionReadInvalid, 0, 0)
	step("an error after an invalid one", ArindbDecisionReadError, 0, 0)
}

// Every outcome is exported from the first scrape, zero included.
func TestArindbDecisionReadOutcomesAreAllExported(t *testing.T) {
	if got := testutil.CollectAndCount(arindbDecisionReads); got != len(arindbDecisionReadOutcomes) {
		t.Fatalf("%d outcome series, want %d", got, len(arindbDecisionReadOutcomes))
	}
}
