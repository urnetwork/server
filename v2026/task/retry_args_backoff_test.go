// A completed-page checkpoint preserves ordinary failure backoff and accounting.
package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Persisting completed scan work must not turn an operational failure into a
// short-retry hint, successful task, new identity or cleared failure count.
func TestTaskRetryArgsPreserveOrdinaryBackoffAndFailure(t *testing.T) {
	cause := errors.New("synthetic completed visit failure")
	args := map[string]string{"cursor": "synthetic-next"}
	hint := WithRetryArgs(cause, args)
	args["cursor"] = "mutated"
	if hint == nil || !errors.Is(hint, cause) || hint.Error() != cause.Error() ||
		taskRetryArgsJson(hint) == nil || !strings.Contains(*taskRetryArgsJson(hint), "synthetic-next") {
		t.Fatal("checkpoint lost the failure or immutable arguments")
	}
	for _, count := range []int{0, 1, 4, 16} {
		for _, jitter := range []float64{0, 0.5, 0.99} {
			wantDelay, wantDelta := taskErrorRetryDelay(cause, count, jitter)
			if delay, delta := taskErrorRetryDelay(hint, count, jitter); delay != wantDelay || delta != wantDelta || delta != 1 {
				t.Fatal("checkpoint changed ordinary backoff or failure accounting", count, jitter, delay, delta)
			}
		}
	}
	stripped := withoutTaskRetryArgs(hint)
	if taskRetryArgsJson(stripped) != nil || !errors.Is(stripped, cause) || stripped.Error() != cause.Error() {
		t.Fatal("late cancellation did not withdraw only the checkpoint")
	}
}

// Independent failures and incomplete or canceled work retain their old args.
func TestTaskRetryArgsRefuseIncompleteOrUnownedProgress(t *testing.T) {
	cause := errors.New("synthetic completed visit failure")
	cycle := &taskCauseTestOne{}
	cycle.cause = cycle
	args := map[string]string{"cursor": "synthetic-next"}
	hint := WithRetryArgs(cause, args)
	for _, err := range []error{
		WithRetryArgs(context.Canceled, args), WithRetryArgs(context.DeadlineExceeded, args),
		WithRetryArgs(ErrDrained, args), WithRetryArgs(ErrTargetNotFound, args),
		WithRetryArgs(cycle, args), WithRetryArgs(&taskCauseTestOne{}, args),
		WithRetryArgs(errors.Join(cause, errors.New("synthetic independent failure")), args),
		errors.Join(hint, errors.New("synthetic later failure")), fmt.Errorf("synthetic outer failure: %w", hint),
	} {
		if taskRetryArgsJson(err) != nil {
			t.Fatal("incomplete or unrelated failure inherited cursor authority")
		}
	}
	for _, args := range []any{nil, []int{1}, "synthetic", map[string]string{"large": strings.Repeat("x", 4096)}, make(chan int)} {
		if taskRetryArgsJson(WithRetryArgs(cause, args)) != nil {
			t.Fatal("invalid checkpoint arguments were persisted")
		}
	}
	if WithRetryArgs(nil, args) != nil {
		t.Fatal("checkpoint manufactured an error")
	}
}

// The same recorder used by EvalTasks counts the checkpointed error as a
// failed execution with its typed database cause, never as a successful scan.
func TestTaskRetryArgsPreserveFailureMetrics(t *testing.T) {
	const metricName = "synthetic.CompletedExpiryVisit"
	failed := taskExecutionsTotal.WithLabelValues(metricName, "system", "failed")
	succeeded := taskExecutionsTotal.WithLabelValues(metricName, "system", "succeeded")
	causes := taskExecutionErrorsTotal.WithLabelValues(metricName, "system", "postgres_other")
	failedBefore, succeededBefore, causeBefore := testutil.ToFloat64(failed), testutil.ToFloat64(succeeded), testutil.ToFloat64(causes)
	err := WithRetryArgs(&pgconn.PgError{Code: "53200", Message: "synthetic proof refusal"}, map[string]int{"cursor": 1})
	recordTaskExecution(metricName, "system", 12, 0, time.Millisecond, err)
	if testutil.ToFloat64(failed) != failedBefore+1 || testutil.ToFloat64(succeeded) != succeededBefore ||
		testutil.ToFloat64(causes) != causeBefore+1 {
		t.Fatal("completed raw checkpoint hid its failed execution or typed cause")
	}
}
