// A target's retry floor delays its failures, a max-time timeout included, but
// never the drain or version-skew retries, and never shortens the backoff.
package task

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTargetRetryFloorDelaysFailuresButKeepsDrainSkewAndBackoff(t *testing.T) {
	oldBase, oldCap := RescheduleTimeout, RescheduleBackoffMaxTimeout
	RescheduleTimeout, RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
	t.Cleanup(func() { RescheduleTimeout, RescheduleBackoffMaxTimeout = oldBase, oldCap })
	const floor = 30 * time.Minute
	target := &errorRetryFlooredTarget{minDelay: floor}
	failures := []error{
		errors.New("accounting rejection"),
		context.Canceled,
		context.DeadlineExceeded,
		// the shape a run canceled at its MaxTime returns
		errors.Join(errors.New("Timeout"), context.Canceled),
	}
	for _, err := range failures {
		// the first failures, which ordinary backoff retries within seconds
		for _, errorCount := range []int{0, 1, 4} {
			if delay, delta := taskTargetErrorRetryDelay(target, err, errorCount, 0.5); delay != floor || delta != 1 {
				t.Fatalf("%v at count %d: retry %s/count delta %d, want %s/1", err, errorCount, delay, delta, floor)
			}
			if delay, _ := taskTargetErrorRetryDelay(nil, err, errorCount, 0.5); floor <= delay {
				t.Fatal("an unconfigured target took the floor")
			}
		}
		// past the floor the ordinary backoff stands
		ordinary, _ := taskErrorRetryDelay(err, 16, 0.5)
		if delay, delta := taskTargetErrorRetryDelay(target, err, 16, 0.5); delay != ordinary || delta != 1 {
			t.Fatalf("%v: the floor replaced a longer backoff: %s, want %s", err, delay, ordinary)
		}
	}
	for _, err := range []error{ErrDrained, ErrTargetNotFound, errors.Join(ErrDrained, context.Canceled)} {
		wantDelay, wantDelta := taskErrorRetryDelay(err, 0, 0.5)
		if delay, delta := taskTargetErrorRetryDelay(target, err, 0, 0.5); delay != wantDelay || delta != wantDelta {
			t.Fatalf("the floor changed a drain or version-skew retry: %s, want %s", delay, wantDelay)
		}
	}
	if got := Testing_TargetErrorRetryDelay(target, context.Canceled, 0); got != floor {
		t.Fatalf("testing delay = %s, want %s", got, floor)
	}
	for _, invalid := range []time.Duration{0, time.Second, time.Hour + time.Second} {
		if got := WithErrorRetryFloor(target, invalid); got != target {
			t.Fatalf("invalid floor %s changed the target", invalid)
		}
	}
	if WithErrorRetryFloor(nil, floor) != nil {
		t.Fatal("a floor made a target of nil")
	}
}
