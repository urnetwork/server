// Finite task results separate database work from joined settlement callbacks.
package model

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// Counts include failed attempts. Durations use the process monotonic clock;
// each total is rounded once, so short repeated stages are not discarded.
type LegacySettlementPhaseDuration struct {
	Count     int   `json:"count"`
	ElapsedMs int64 `json:"elapsed_ms"`
	MaxMs     int64 `json:"max_ms"`
}

// These fields are one page's observations, not global rates or financial
// outcomes. The callback families can overlap; their sum is not page latency.
// ColdCensus is a subset of Mirror; callback families are within JoinedPosts.
// Selection and Financial include pool acquisition and transaction completion.
// An interrupted final attempt can be timed without entering the Visited count;
// JoinedPosts also records calls with no callbacks after a busy ownership gate.
type LegacySettlementTimings struct {
	Selection   LegacySettlementPhaseDuration `json:"selection"`
	Financial   LegacySettlementPhaseDuration `json:"financial"`
	JoinedPosts LegacySettlementPhaseDuration `json:"joined_posts"`
	Mirror      LegacySettlementPhaseDuration `json:"mirror"`
	ColdCensus  LegacySettlementPhaseDuration `json:"cold_census"`
	Clock       LegacySettlementPhaseDuration `json:"clock"`
	Stream      LegacySettlementPhaseDuration `json:"stream"`
}

type legacySettlementTimingPhase uint8

const (
	legacySettlementSelection legacySettlementTimingPhase = iota
	legacySettlementFinancial
	legacySettlementJoinedPosts
	legacySettlementMirror
	legacySettlementColdCensus
	legacySettlementClock
	legacySettlementStream
	legacySettlementPhaseCount
)

type legacySettlementTimingKey struct{}

type legacySettlementTimingSample struct {
	count   int
	elapsed time.Duration
	maximum time.Duration
}

// One page owns the observer. RunPosts may call independent families in
// parallel, so only the small accumulated state shares a mutex. No I/O runs
// under it. Other settlement callers have no observer and retain their posts.
type legacySettlementTimingObserver struct {
	mu      sync.Mutex
	now     func() time.Time
	samples [legacySettlementPhaseCount]legacySettlementTimingSample
}

func enterLegacySettlementTiming(ctx context.Context, phase legacySettlementTimingPhase) func() {
	observer, _ := ctx.Value(legacySettlementTimingKey{}).(*legacySettlementTimingObserver)
	if observer == nil || phase >= legacySettlementPhaseCount {
		return func() {}
	}
	started := observer.now()
	return func() {
		elapsed := max(time.Duration(0), observer.now().Sub(started))
		observer.mu.Lock()
		sample := &observer.samples[phase]
		sample.count++
		sample.elapsed += elapsed
		sample.maximum = max(sample.maximum, elapsed)
		observer.mu.Unlock()
	}
}

func observeLegacySettlementPost(ctx context.Context, phase legacySettlementTimingPhase, post server.PostFunction) server.PostFunction {
	if observer, _ := ctx.Value(legacySettlementTimingKey{}).(*legacySettlementTimingObserver); observer == nil && legacyTargetTraceOf(ctx) == nil {
		return post
	}
	return func() any {
		if name := legacyTargetTracePostStage(phase); name != "" {
			defer enterLegacyTargetTrace(ctx, name)()
		}
		defer enterLegacySettlementTiming(ctx, phase)()
		return post()
	}
}

func (self *legacySettlementTimingObserver) snapshot() *LegacySettlementTimings {
	self.mu.Lock()
	defer self.mu.Unlock()
	result := &LegacySettlementTimings{}
	for phase, target := range []*LegacySettlementPhaseDuration{
		&result.Selection, &result.Financial, &result.JoinedPosts, &result.Mirror,
		&result.ColdCensus, &result.Clock, &result.Stream,
	} {
		sample := self.samples[phase]
		*target = LegacySettlementPhaseDuration{Count: sample.count, ElapsedMs: sample.elapsed.Milliseconds(), MaxMs: sample.maximum.Milliseconds()}
	}
	return result
}
