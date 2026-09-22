// Deterministic retry-policy controls pin hints independently from random jitter.
package task

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Explicit bounds prevent hints from becoming a hot loop or unbounded delay.
func TestTaskRetryDelayRejectsInvalidBounds(t *testing.T) {
	oldBase, oldCap := RescheduleTimeout, RescheduleBackoffMaxTimeout
	RescheduleTimeout, RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
	t.Cleanup(func() { RescheduleTimeout, RescheduleBackoffMaxTimeout = oldBase, oldCap })
	cause := errors.New("synthetic target failure")
	for _, delay := range []time.Duration{-time.Second, 0, time.Nanosecond, time.Second, time.Hour + time.Nanosecond} {
		if WithRetryDelay(cause, delay) != cause {
			t.Errorf("invalid delay %s changed original error authority", delay)
		}
	}
	if WithRetryDelay(nil, 3*time.Second) != nil {
		t.Fatal("retry hint manufactured a failure")
	}
	for _, delay := range []time.Duration{2 * time.Second, 3 * time.Second, time.Minute, time.Hour} {
		err := WithRetryDelay(cause, delay)
		got, delta := taskErrorRetryDelay(err, 16, 0.5)
		if got != delay || delta != 1 || !errors.Is(err, cause) || err.Error() != cause.Error() {
			t.Errorf("bounded delay %s lost its cause, count, or requested cadence", delay)
		}
	}
}

// A root hint cannot erase cancellation, drain or version skew; a hint nested
// in any mixed join cannot shorten the other failure's ordinary backoff.
func TestTaskRetryDelayPreservesOrdinaryErrorPrecedence(t *testing.T) {
	oldBase, oldCap := RescheduleTimeout, RescheduleBackoffMaxTimeout
	RescheduleTimeout, RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
	t.Cleanup(func() { RescheduleTimeout, RescheduleBackoffMaxTimeout = oldBase, oldCap })
	cause := errors.New("synthetic accounting rejection")
	other := errors.New("synthetic unrelated failure")
	hint := WithRetryDelay(cause, 4*time.Second)
	ordinary := errorRescheduleDelay(RescheduleTimeout, RescheduleBackoffMaxTimeout, 16, rescheduleBackoffMaxExponent, 0.5)
	cases := []struct {
		name  string
		err   error
		delay time.Duration
		delta int
	}{
		{name: "ordinary", err: cause, delay: ordinary, delta: 1},
		{name: "joined-other", err: errors.Join(hint, other), delay: ordinary, delta: 1},
		{name: "joined-cancel", err: errors.Join(hint, context.Canceled), delay: ordinary, delta: 1},
		{name: "joined-timeout", err: errors.Join(hint, errors.New("Timeout")), delay: ordinary, delta: 1},
		{name: "wrapped-hint", err: fmt.Errorf("synthetic outer failure: %w", hint), delay: ordinary, delta: 1},
		{name: "hint-containing-cancel", err: WithRetryDelay(errors.Join(cause, context.Canceled), 3*time.Second), delay: ordinary, delta: 1},
		{name: "hint-containing-deadline", err: WithRetryDelay(context.DeadlineExceeded, 3*time.Second), delay: ordinary, delta: 1},
		{name: "hint-containing-drain", err: WithRetryDelay(errors.Join(cause, ErrDrained), time.Minute), delay: 3 * time.Second, delta: 0},
		{name: "hint-containing-target-not-found", err: WithRetryDelay(errors.Join(cause, ErrTargetNotFound), time.Minute), delay: 17 * time.Second, delta: 1},
		{name: "drain-before-target", err: WithRetryDelay(errors.Join(ErrDrained, ErrTargetNotFound), time.Minute), delay: 3 * time.Second, delta: 0},
	}
	for _, c := range cases {
		if delay, delta := taskErrorRetryDelay(c.err, 16, 0.5); delay != c.delay || delta != c.delta {
			t.Errorf("%s: got retry %s/count delta %d, want %s/%d", c.name, delay, delta, c.delay, c.delta)
		}
	}
	RescheduleBackoffMaxTimeout = 2 * time.Second
	if delay, _ := taskErrorRetryDelay(hint, 16, 0.5); delay != errorRescheduleDelay(2*time.Second, 2*time.Second, 16, rescheduleBackoffMaxExponent, 0.5) {
		t.Fatal("old hint bypassed the current duration cap")
	}
}
