package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Cancellation is an error identity, not a substring of a diagnostic report.
func TestIsDoneErrorRequiresExplicitShutdownSignal(t *testing.T) {
	cases := []struct {
		name  string
		value any
		done  bool
	}{
		{name: "canceled", value: context.Canceled, done: true},
		{name: "wrapped cancellation", value: fmt.Errorf("synthetic database operation: %w", context.Canceled), done: true},
		{name: "database done", value: DbContextDoneError, done: true},
		{name: "wrapped database done", value: fmt.Errorf("synthetic database operation: %w", DbContextDoneError), done: true},
		{name: "legacy string", value: "Done", done: true},
		{name: "deadline", value: context.DeadlineExceeded},
		{name: "wrapped deadline", value: fmt.Errorf("synthetic database operation: %w", context.DeadlineExceeded)},
		{name: "done prefix", value: errors.New("DoneCounter invariant violated")},
		{name: "legacy error", value: errors.New("Done"), done: true},
		{name: "wrapped legacy error", value: fmt.Errorf("synthetic shutdown: %w", errors.New("Done")), done: true},
		{name: "joined shutdown", value: errors.Join(context.Canceled, DbContextDoneError), done: true},
		{name: "joined unexpected failure", value: errors.Join(context.Canceled, errors.New("synthetic invariant failure"))},
		{name: "wrapped mixed failure", value: fmt.Errorf("synthetic operation: %w", errors.Join(context.Canceled, context.DeadlineExceeded))},
		{name: "cancellation diagnostic", value: errors.New("application invariant failed; prior diagnostic: context canceled")},
		{name: "deallocation failure", value: errors.New("failed to deallocate cached statement(s): synthetic transport failed")},
		{name: "string prefix", value: "DoneCounter invariant violated"},
		{name: "string cancellation", value: "context canceled"},
		{name: "nil", value: nil},
	}
	for _, c := range cases {
		if got := IsDoneError(c.value); got != c.done {
			t.Errorf("%s: IsDoneError(%v) = %t, want %t", c.name, c.value, got, c.done)
		}
	}
}
