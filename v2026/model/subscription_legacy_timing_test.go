package model

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Synthetic monotonic advances prove accumulation and maxima without depending
// on machine speed. Nested families remain independent of the joined wall time.
func TestLegacySettlementTimingAccumulatesFinitePhases(t *testing.T) {
	var nanos atomic.Int64
	observer := &legacySettlementTimingObserver{now: func() time.Time { return time.Unix(0, nanos.Load()) }}
	ctx := context.WithValue(t.Context(), legacySettlementTimingKey{}, observer)
	for _, elapsed := range []time.Duration{1500 * time.Microsecond, 500 * time.Microsecond} {
		leave := enterLegacySettlementTiming(ctx, legacySettlementSelection)
		nanos.Add(int64(elapsed))
		leave()
	}
	joined := enterLegacySettlementTiming(ctx, legacySettlementJoinedPosts)
	mirror := enterLegacySettlementTiming(ctx, legacySettlementMirror)
	census := enterLegacySettlementTiming(ctx, legacySettlementColdCensus)
	nanos.Add(int64(3 * time.Millisecond))
	census()
	nanos.Add(int64(2 * time.Millisecond))
	mirror()
	joined()
	got := observer.snapshot()
	if got.Selection != (LegacySettlementPhaseDuration{Count: 2, ElapsedMs: 2, MaxMs: 1}) ||
		got.ColdCensus != (LegacySettlementPhaseDuration{Count: 1, ElapsedMs: 3, MaxMs: 3}) ||
		got.Mirror != (LegacySettlementPhaseDuration{Count: 1, ElapsedMs: 5, MaxMs: 5}) || got.JoinedPosts != got.Mirror {
		t.Fatal("phase aggregation lost short stages or confused nested work with page latency")
	}
	encoded, err := json.Marshal(LegacySettlementFlushResult{Timings: got})
	var stored LegacySettlementFlushResult
	if err != nil || json.Unmarshal(encoded, &stored) != nil || !reflect.DeepEqual(stored.Timings, got) {
		t.Fatal("finite phase timings did not survive a task result JSON round trip")
	}
	if len(encoded) > 2048 {
		t.Fatal("bounded phase result unexpectedly exceeded its fixed payload budget")
	}
}

// A wrapper forwards later post generations and preserves the original panic;
// canceled parent state does not change a callback's existing execution policy.
func TestLegacySettlementTimingPreservesCallbackOutcome(t *testing.T) {
	var nanos atomic.Int64
	observer := &legacySettlementTimingObserver{now: func() time.Time { return time.Unix(0, nanos.Load()) }}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), legacySettlementTimingKey{}, observer))
	cancel()
	called := 0
	child := server.PostFunction(func() any { called++; return nil })
	post := observeLegacySettlementPost(ctx, legacySettlementClock, func() any {
		nanos.Add(int64(7 * time.Millisecond))
		return child
	})
	forwarded, ok := post().(server.PostFunction)
	if !ok || called != 0 {
		t.Fatal("timing wrapper ran or discarded a later callback generation")
	}
	forwarded()
	want := "synthetic stream callback failure"
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		observeLegacySettlementPost(ctx, legacySettlementStream, func() any {
			nanos.Add(int64(4 * time.Millisecond))
			panic(want)
		})()
	}()
	got := observer.snapshot()
	if recovered != want || called != 1 || got.Clock != (LegacySettlementPhaseDuration{Count: 1, ElapsedMs: 7, MaxMs: 7}) ||
		got.Stream != (LegacySettlementPhaseDuration{Count: 1, ElapsedMs: 4, MaxMs: 4}) {
		t.Fatal("phase observation changed callback execution, panic identity, or failed-attempt timing")
	}
}

func TestLegacySettlementTimingJoinsConcurrentFamilies(t *testing.T) {
	observer := &legacySettlementTimingObserver{now: func() time.Time { return time.Unix(0, 0) }}
	ctx := context.WithValue(t.Context(), legacySettlementTimingKey{}, observer)
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			observeLegacySettlementPost(ctx, legacySettlementMirror, func() any { return nil })()
			_ = observer.snapshot()
		}()
	}
	wg.Wait()
	if got := observer.snapshot().Mirror; got != (LegacySettlementPhaseDuration{Count: 64}) {
		t.Fatal("concurrent callback timing lost an observation")
	}
}
