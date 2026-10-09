// A completed page preserves each row's diagnostic and independent error budget.
package task

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// The task's exact root hint still owns the immutable argument snapshot, same
// failure and ordinary backoff when a completed page has many failed members.
func TestTaskRetryManyCompletedRowsPreserveCheckpointAndFailure(t *testing.T) {
	causes := make([]error, 512)
	for index := range causes {
		causes[index] = &pgconn.PgError{Code: "53200", Message: fmt.Sprintf("synthetic proof refusal %d", index)}
	}
	batch := server.NewErrorCauseBatch(causes)
	cause := fmt.Errorf("synthetic completed page: %w", batch)
	args := map[string]string{"cursor": "synthetic-next"}
	hint := WithRetryArgs(cause, args)
	args["cursor"] = "mutated"
	if taskRetryArgsJson(hint) == nil || !strings.Contains(*taskRetryArgsJson(hint), "synthetic-next") ||
		hint.Error() != cause.Error() || !errors.Is(hint, causes[len(causes)-1]) ||
		taskExecutionErrorCause(hint) != "postgres_other" {
		t.Fatal("many-row progress lost its snapshot, original error or typed failure metric")
	}
	for _, count := range []int{0, 1, 4, 16} {
		for _, jitter := range []float64{0, 0.5, 0.99} {
			wantDelay, wantDelta := taskErrorRetryDelay(causes[0], count, jitter)
			if delay, delta := taskErrorRetryDelay(hint, count, jitter); delay != wantDelay || delta != wantDelta || delta != 1 {
				t.Fatal("many-row checkpoint changed ordinary failure backoff", count, jitter)
			}
		}
	}
	for _, invalid := range []error{batch, errors.Join(hint, errors.New("synthetic later failure")), fmt.Errorf("synthetic outer: %w", hint)} {
		var observed error = invalid
		if invalid == batch {
			observed = WithRetryArgs(invalid, args)
		}
		if taskRetryArgsJson(observed) != nil {
			t.Fatal("a joined or unowned failure inherited the completed-page checkpoint")
		}
	}
	stripped := withoutTaskRetryArgs(hint)
	if taskRetryArgsJson(stripped) != nil || stripped.Error() != cause.Error() || !errors.Is(stripped, causes[len(causes)-1]) {
		t.Fatal("late cancellation did not withdraw only the many-row checkpoint")
	}
}

// Bounded per-row traversal still observes a hard or malformed final member,
// and mutable causes must be revalidated again when completion stores arguments.
func TestTaskRetryManyCompletedRowsRejectLateHardOrMalformedMember(t *testing.T) {
	leaf := errors.New("synthetic proof failure")
	mutable := &taskCauseTestOne{cause: leaf}
	causes := make([]error, 512)
	for index := range causes {
		causes[index] = leaf
	}
	causes[len(causes)-1] = mutable
	cause := fmt.Errorf("synthetic completed page: %w", server.NewErrorCauseBatch(causes))
	hint := WithRetryDelayAndArgs(cause, time.Minute, map[string]int{"cursor": 1})
	if taskRetryArgsJson(hint) == nil {
		t.Fatal("complete original row receipt was refused")
	}
	wide := make([]error, 128)
	for index := range wide {
		wide[index] = leaf
	}
	var typedNil *taskCauseTestOne
	for _, invalid := range []error{nil, typedNil, context.Canceled, context.DeadlineExceeded,
		ErrDrained, ErrTargetNotFound, mutable, errors.Join(wide...), server.NewErrorCauseBatch([]error{leaf})} {
		mutable.cause = invalid
		if taskRetryArgsJson(hint) != nil || taskRetryArgsJson(WithRetryArgs(cause, map[string]int{"cursor": 2})) != nil {
			t.Fatal("late malformed or hard row retained cursor publication authority")
		}
	}
}
