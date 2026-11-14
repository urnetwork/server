// Pure control-budget regressions never load settings, tokens or a database.
package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The real constructed deadline is tested against a deterministic sequential
// work plan. Changing only the old30s default makes the fourth stage fail.
func TestRecoveryControlContextAdmitsHealthySequentialPreflight(t *testing.T) {
	startedAt := time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
	limits := defaultRecoverySettings()
	ctx, cancel, err := newRecoveryControlContext(context.Background(), limits, startedAt)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("actual control context has no deadline")
	}
	var elapsed time.Duration
	var proofStartedAt, proofCompletedAt time.Time
	for _, phase := range []struct {
		Name     string
		Duration time.Duration
	}{
		{Name: "frozen-source-and-read-preparation", Duration: 12 * time.Second},
		{Name: "first-locked-gate", Duration: 4 * time.Second},
		{Name: "exact-original-status", Duration: 9 * time.Second},
		{Name: "local-availability", Duration: 9 * time.Second},
		{Name: "remote-availability", Duration: 9 * time.Second},
		{Name: "final-locked-gate", Duration: 4 * time.Second},
	} {
		elapsed += phase.Duration
		completedAt := startedAt.Add(elapsed)
		if !completedAt.Before(deadline) {
			t.Fatalf("healthy sequential phase %q exceeds actual constructed control deadline", phase.Name)
		}
		if phase.Name == "first-locked-gate" {
			proofStartedAt = completedAt
		}
		if phase.Name == "final-locked-gate" {
			proofCompletedAt = completedAt
		}
	}
	if proofStartedAt.IsZero() || proofCompletedAt.Sub(proofStartedAt) >= time.Minute {
		t.Fatal("fixture incorrectly requires relaxing the separate native-proof window")
	}
	if deadline != startedAt.Add(2*time.Minute) {
		t.Fatal("actual control context does not own the approved two-minute budget")
	}
	if limits.DocumentBytes != 1024*1024 || limits.TokenBytes != 1025 {
		t.Fatal("unrelated control document/token bounds changed")
	}
}

// A longer CLI allowance cannot extend a caller's lifetime or swallow its stop.
func TestRecoveryControlContextPreservesParentLifetime(t *testing.T) {
	startedAt := time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
	parentDeadline := startedAt.Add(15 * time.Second)
	parent, parentCancel := context.WithDeadline(context.Background(), parentDeadline)
	defer parentCancel()
	ctx, cancel, err := newRecoveryControlContext(parent, defaultRecoverySettings(), startedAt)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if deadline, ok := ctx.Deadline(); !ok || deadline != parentDeadline {
		t.Fatal("parent deadline was extended")
	}
	if !startedAt.Add(14*time.Second).Before(parentDeadline) || startedAt.Add(16*time.Second).Before(parentDeadline) {
		t.Fatal("parent-bound sequential fixture is malformed")
	}
	parentCancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("parent cancellation did not propagate")
	}
	refused, refusedCancel, err := newRecoveryControlContext(parent, defaultRecoverySettings(), startedAt)
	if !errors.Is(err, context.Canceled) || refused != nil || refusedCancel != nil {
		t.Fatal("already canceled parent admitted control work")
	}
	expired, expiredCancel := context.WithDeadline(context.Background(), time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC))
	defer expiredCancel()
	refused, refusedCancel, err = newRecoveryControlContext(expired, defaultRecoverySettings(), startedAt)
	if !errors.Is(err, context.DeadlineExceeded) || refused != nil || refusedCancel != nil {
		t.Fatal("expired parent admitted control work")
	}
}

// No fallback/default can turn a missing or nonpositive owner bound into work.
func TestRecoveryControlContextRejectsInvalidBounds(t *testing.T) {
	startedAt := time.Date(2100, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, timeout := range []time.Duration{0, -time.Nanosecond, -time.Minute} {
		limits := defaultRecoverySettings()
		limits.Timeout = timeout
		ctx, cancel, err := newRecoveryControlContext(context.Background(), limits, startedAt)
		if err == nil || ctx != nil || cancel != nil {
			t.Errorf("nonpositive bound %s admitted", timeout)
		}
	}
	ctx, cancel, err := newRecoveryControlContext(context.Background(), defaultRecoverySettings(), time.Time{})
	if err == nil || ctx != nil || cancel != nil {
		t.Fatal("missing owner clock admitted")
	}
	ctx, cancel, err = newRecoveryControlContext(nil, defaultRecoverySettings(), startedAt)
	if err == nil || ctx != nil || cancel != nil {
		t.Fatal("missing parent admitted")
	}
}
