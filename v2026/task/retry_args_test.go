// Tests immutable, explicit retry checkpoints independently from task execution.
package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Serialized arguments are frozen before their caller can mutate a cursor.
func TestTaskRetryArgsSnapshotKeepsFailureAndDelay(t *testing.T) {
	cause := errors.New("synthetic accounting rejection")
	args := map[string]any{"cursor": "first"}
	err := WithRetryDelayAndArgs(cause, time.Minute, args)
	args["cursor"] = "later"
	got := taskRetryArgsJson(err)
	if got == nil || *got != `{"cursor":"first"}` || err.Error() != cause.Error() || !errors.Is(err, cause) {
		t.Fatal("retry checkpoint changed its arguments or owning error")
	}
	if delay, delta := taskErrorRetryDelay(err, 16, 0.5); delay != time.Minute || delta != 1 {
		t.Fatal("checkpoint changed existing failure cadence or error count")
	}
	without := withoutTaskRetryArgs(err)
	if taskRetryArgsJson(without) != nil || without.Error() != cause.Error() {
		t.Fatal("cancellation cleanup did not withdraw only the checkpoint")
	}
	if delay, delta := taskErrorRetryDelay(without, 16, 0.5); delay != time.Minute || delta != 1 {
		t.Fatal("withdrawing progress changed failure cadence")
	}
}

// Joins, cancellation, ownership loss and wrapped hints cannot skip work.
func TestTaskRetryArgsRefusesAmbiguousOrCanceledProgress(t *testing.T) {
	cause := errors.New("synthetic accounting rejection")
	args := map[string]any{"cursor": "next"}
	hint := WithRetryDelayAndArgs(cause, time.Minute, args)
	for _, err := range []error{
		cause,
		WithRetryDelay(cause, time.Minute),
		errors.Join(hint, errors.New("synthetic operational failure")),
		fmt.Errorf("synthetic outer failure: %w", hint),
		WithRetryDelayAndArgs(errors.Join(cause, errors.New("synthetic other failure")), time.Minute, args),
		WithRetryDelayAndArgs(context.Canceled, time.Minute, args),
		WithRetryDelayAndArgs(context.DeadlineExceeded, time.Minute, args),
		WithRetryDelayAndArgs(ErrDrained, time.Minute, args),
		WithRetryDelayAndArgs(ErrTargetNotFound, time.Minute, args),
		WithRetryDelayAndArgs(cause, 0, args),
	} {
		if taskRetryArgsJson(err) != nil {
			t.Fatal("ambiguous or canceled failure acquired cursor authority")
		}
	}
}

// Invalid or oversized arguments retain the failure and its existing delay.
func TestTaskRetryArgsRejectsInvalidOrUnboundedArguments(t *testing.T) {
	cause := errors.New("synthetic accounting rejection")
	for _, args := range []any{nil, "scalar", []string{"array"}, map[string]any{"invalid": func() {}}, map[string]string{"large": strings.Repeat("x", 4*1024)}} {
		err := WithRetryDelayAndArgs(cause, time.Minute, args)
		if taskRetryArgsJson(err) != nil || !errors.Is(err, cause) {
			t.Fatal("invalid serialization changed retry authority")
		}
		if delay, delta := taskErrorRetryDelay(err, 16, 0.5); delay != time.Minute || delta != 1 {
			t.Fatal("invalid arguments changed failure cadence")
		}
	}
	if WithRetryDelayAndArgs(nil, time.Minute, map[string]int{}) != nil {
		t.Fatal("checkpoint manufactured a task failure")
	}
}
