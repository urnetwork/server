package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The barrier orders publication; the timeout only bounds a broken fixture.
func startReconcileReceiptPublisher(t *testing.T, barrier <-chan struct{}, publish func()) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-barrier:
			publish()
		case <-ctx.Done():
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("receipt publisher did not retire")
		}
	})
	return done
}

func awaitReconcileReceiptPublisher(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("receipt publication barrier did not complete")
	}
}

// The real check path must not judge a later copied completion by its early clock.
// This file also compiles against the unchanged baseline for the causal RED run.
func TestReconcileReceiptObservationAfterConcurrentCompletion(t *testing.T) {
	checkTime := time.Date(2026, 10, 18, 9, 19, 55, 268096964, time.UTC)
	completed := checkTime.Add(1643374 * time.Nanosecond)
	observed := completed.Add(time.Nanosecond)
	tailer := syntheticScopePair(checkTime, "api")
	tailer.clock = func() time.Time { return completed }
	barrier := make(chan struct{})
	published := startReconcileReceiptPublisher(t, barrier, func() {
		tailer.recordReconcile(completed.Add(-logReconcileLookback), nil)
		tailer.stateLock.Lock()
		tailer.lastLineTime = completed
		tailer.stateLock.Unlock()
	})
	clockCalls := 0
	env := &probeEnv{now: func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			// Capture the old check time, then publish before any collector copy.
			close(barrier)
			awaitReconcileReceiptPublisher(t, published)
			return checkTime
		}
		return observed
	}}
	var diagnostics strings.Builder
	probe := &logTailProbe{tailers: []*logTailer{tailer}, reconcileDiagnostics: &diagnostics}
	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	receipt := parseSyntheticReconcileReceipt(t, diagnostics.String())
	scope := requireCollectorScope(t, receipt, "api")
	if scope.LatestCompletedAt == nil || !scope.LatestCompletedAt.Equal(completed) ||
		receipt.ObservedAt.Before(completed) || scope.CoverageState != "fresh-two-windows" ||
		scope.Fresh == nil || !*scope.Fresh || scope.ConsecutiveTwo == nil || !*scope.ConsecutiveTwo ||
		receipt.Fresh != 1 || receipt.ConsecutiveTwo != 1 {
		t.Fatal("causal assertion: concurrently published completion was judged against the earlier check clock")
	}
	if clockCalls != 2 || !receipt.ObservedAt.Equal(observed) || receipt.Collectors != 1 || receipt.Enabled != 1 ||
		scope.PreviousCompletedAt == nil || !scope.PreviousCompletedAt.Equal(checkTime) ||
		scope.LastSuccessAt == nil || !scope.LastSuccessAt.Equal(completed) ||
		scope.LastFailurePresent == nil || *scope.LastFailurePresent {
		t.Fatal("receipt lost its observation clock, collector fields or success counters")
	}
	for _, class := range []string{"tailer-reconcile", "tailer-silent"} {
		if !findingByClass(t, findings, class).healthy {
			t.Fatal("receipt correction changed the adjacent health result for new successful activity")
		}
	}
}

// Both early and late collectors remain immutable when publication follows all
// copies, even if the publication precedes the receipt's observation timestamp.
func TestReconcileReceiptSnapshotExcludesLaterPublication(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "completion"
		if failure {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			initial := time.Date(2026, 10, 18, 9, 20, 0, 0, time.UTC)
			completed := initial.Add(time.Millisecond)
			observed := completed.Add(time.Millisecond)
			tailers := []*logTailer{syntheticScopePair(initial, "api"), syntheticScopePair(initial, "proxy")}
			for _, tailer := range tailers {
				tailer.clock = func() time.Time { return completed }
			}
			barrier := make(chan struct{})
			published := startReconcileReceiptPublisher(t, barrier, func() {
				var result error
				if failure {
					result = errors.New("private-error private-token")
				}
				for _, tailer := range tailers {
					tailer.recordReconcile(completed.Add(-logReconcileLookback), result)
				}
			})
			clockCalls := 0
			env := &probeEnv{now: func() time.Time {
				clockCalls++
				if clockCalls == 1 {
					return initial
				}
				if clockCalls == 2 {
					// Production calls this only after every receipt copy is complete.
					close(barrier)
					awaitReconcileReceiptPublisher(t, published)
				}
				return observed
			}}
			var diagnostics strings.Builder
			probe := &logTailProbe{tailers: tailers, reconcileDiagnostics: &diagnostics}
			if _, err := probe.check(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			if clockCalls != 2 {
				t.Fatal("receipt did not sample its clock after the collector copies")
			}
			receipt := parseSyntheticReconcileReceipt(t, diagnostics.String())
			if !receipt.ObservedAt.Equal(observed) || receipt.Collectors != 2 || receipt.Enabled != 2 ||
				receipt.Fresh != 2 || receipt.ConsecutiveTwo != 2 || !receipt.ScopeProjectionComplete {
				t.Fatal("post-copy publication changed the current aggregate")
			}
			for _, service := range []string{"api", "proxy"} {
				scope := requireCollectorScope(t, receipt, service)
				if scope.CoverageState != "fresh-two-windows" || scope.LatestCompletedAt == nil ||
					!scope.LatestCompletedAt.Equal(initial) || scope.PreviousCompletedAt == nil ||
					!scope.PreviousCompletedAt.Equal(initial.Add(-logReconcileInterval)) ||
					scope.LastSuccessAt == nil || !scope.LastSuccessAt.Equal(initial) ||
					scope.LastFailurePresent == nil || *scope.LastFailurePresent {
					t.Fatal("post-copy publication mutated a copied collector")
				}
			}
			diagnostics.Reset()
			if _, err := probe.check(context.Background(), &probeEnv{now: func() time.Time { return observed }}); err != nil {
				t.Fatal(err)
			}
			next := parseSyntheticReconcileReceipt(t, diagnostics.String())
			for _, service := range []string{"api", "proxy"} {
				scope := requireCollectorScope(t, next, service)
				if failure {
					if scope.CoverageState != "no-valid-window" || scope.LatestCompletedAt != nil ||
						scope.LastFailurePresent == nil || !*scope.LastFailurePresent ||
						scope.LastSuccessAt == nil || !scope.LastSuccessAt.Equal(initial) ||
						next.Fresh != 0 || next.ConsecutiveTwo != 0 {
						t.Fatal("later receipt lost the published failure or historical last success")
					}
				} else if scope.CoverageState != "fresh-two-windows" || scope.LatestCompletedAt == nil ||
					!scope.LatestCompletedAt.Equal(completed) || scope.PreviousCompletedAt == nil ||
					!scope.PreviousCompletedAt.Equal(initial) || scope.LastSuccessAt == nil ||
					!scope.LastSuccessAt.Equal(completed) || next.Fresh != 2 || next.ConsecutiveTwo != 2 {
					t.Fatal("later receipt lost the published advancing completion")
				}
			}
		})
	}
}

// No tolerance or clamping: genuine future clocks and the exact stale boundary
// retain their existing meaning after the observation clock moves after copying.
func TestReconcileReceiptObservationClockBoundaries(t *testing.T) {
	initial := time.Date(2026, 10, 18, 9, 20, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name              string
		checkOffset       time.Duration
		observationOffset time.Duration
		completionOffset  time.Duration
		state             string
		fresh, two        bool
	}{
		{name: "equal", state: "fresh-two-windows", fresh: true, two: true},
		{name: "future-one-nanosecond", completionOffset: time.Nanosecond, state: "invalid-time"},
		{name: "clock-step-back", observationOffset: -time.Nanosecond, state: "invalid-time"},
		{name: "last-fresh-nanosecond", checkOffset: 2*logReconcileInterval - time.Nanosecond,
			observationOffset: 2*logReconcileInterval - time.Nanosecond, state: "fresh-two-windows", fresh: true, two: true},
		{name: "crosses-stale-boundary", checkOffset: 2*logReconcileInterval - time.Nanosecond,
			observationOffset: 2 * logReconcileInterval, state: "stale-window"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tailer := syntheticScopePair(initial, "api")
			completed := initial.Add(testCase.completionOffset)
			tailer.reconcileSuccesses[1].completed = completed
			tailer.lastReconcileTime = completed
			clockCalls := 0
			env := &probeEnv{now: func() time.Time {
				clockCalls++
				if clockCalls == 1 {
					return initial.Add(testCase.checkOffset)
				}
				return initial.Add(testCase.observationOffset)
			}}
			var diagnostics strings.Builder
			probe := &logTailProbe{tailers: []*logTailer{tailer}, reconcileDiagnostics: &diagnostics}
			if _, err := probe.check(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			receipt := parseSyntheticReconcileReceipt(t, diagnostics.String())
			scope := requireCollectorScope(t, receipt, "api")
			fresh, two := 0, 0
			if testCase.fresh {
				fresh = 1
			}
			if testCase.two {
				two = 1
			}
			if clockCalls != 2 || !receipt.ObservedAt.Equal(initial.Add(testCase.observationOffset)) ||
				scope.CoverageState != testCase.state || scope.Fresh == nil || *scope.Fresh != testCase.fresh ||
				scope.ConsecutiveTwo == nil || *scope.ConsecutiveTwo != testCase.two ||
				scope.LatestCompletedAt == nil || !scope.LatestCompletedAt.Equal(completed) ||
				receipt.Fresh != fresh || receipt.ConsecutiveTwo != two {
				t.Fatal("receipt relaxed a clock boundary or rewrote a source timestamp")
			}
		})
	}
}

// The extra receipt clock is only sampled when a diagnostic can be emitted.
func TestReconcileReceiptObservationSkippedWithoutSinkOrAfterCancel(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "no-sink"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 18, 9, 20, 0, 0, time.UTC)
			clockCalls := 0
			env := &probeEnv{now: func() time.Time { clockCalls++; return now }}
			var diagnostics strings.Builder
			probe := &logTailProbe{tailers: []*logTailer{syntheticScopePair(now, "api")}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				probe.reconcileDiagnostics = &diagnostics
				cancel()
			}
			if _, err := probe.check(ctx, env); err != nil {
				t.Fatal(err)
			}
			if clockCalls != 1 || diagnostics.Len() != 0 {
				t.Fatal("unemitted receipt sampled another clock or wrote diagnostics")
			}
		})
	}
}
