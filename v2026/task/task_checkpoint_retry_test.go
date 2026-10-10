package task

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Only the exact target's acknowledged checkpoint can shorten a budget retry.
// It retains the owning error/count; drain and collector policy still win.
func TestTaskCommittedProgressRetryKeepsCauseAndFailureCount(t *testing.T) {
	timeout := errors.Join(errors.New("Timeout"), context.Canceled)
	for _, cause := range []error{timeout, context.Canceled, context.DeadlineExceeded, errors.New("synthetic SQL failure after progress")} {
		ordinary, ordinaryDelta := taskErrorRetryDelay(cause, 19, 0.5)
		hinted := WithCommittedProgressRetry(cause)
		delay, delta := taskErrorRetryDelay(hinted, 19, 0.5)
		if ordinary <= time.Minute || ordinaryDelta != 1 || delay != RescheduleTimeout || delta != 1 ||
			hinted.Error() != cause.Error() || !errors.Is(hinted, cause) || taskRetryArgsJson(hinted) != nil {
			t.Fatal("acknowledged progress did not retain cause/count and its bounded continuation")
		}
		ordinaryHint, ordinaryDelta := taskErrorRetryDelay(WithRetryDelay(cause, RescheduleTimeout), 19, 0.5)
		if (errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)) &&
			(ordinaryHint <= time.Minute || ordinaryDelta != 1) {
			t.Fatal("ordinary canceled retry acquired checkpoint authority")
		}
	}
	drained := WithCommittedProgressRetry(ErrDrained)
	_, delta := taskErrorRetryDelay(drained, 19, 0.5)
	if delta != 0 {
		t.Fatal("checkpoint retry replaced drain policy")
	}
	interrupted := &taskCollectorInterruption{cause: context.Canceled}
	if WithCommittedProgressRetry(interrupted) != interrupted {
		t.Fatal("checkpoint wrapper hid collector cancellation provenance")
	}
	if WithCommittedProgressRetry(nil) != nil {
		t.Fatal("checkpoint invented a failure")
	}
}
