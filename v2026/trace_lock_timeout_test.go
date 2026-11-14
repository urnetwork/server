// HandleError counts bounded lock waits instead of reporting them.
package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A lock wait its bound ended (55P03) is a busy resource whose caller retries:
// counted, without an unexpected-error report. On 2026-10-25 connect reported
// each one with a stack. The statement stop that follows joins context
// cancellations to it, and that join still classifies. A lock timeout joined
// with any other failure, another PostgreSQL error, or text is still
// reported, and every recovery still reaches its handlers.
func TestHandleErrorCountsLockTimeoutsWithoutAReport(t *testing.T) {
	reports := 0
	logf := unexpectedErrorLogf
	unexpectedErrorLogf = func(string, ...any) { reports++ }
	defer func() { unexpectedErrorLogf = logf }()
	lockTimeout := &pgconn.PgError{Code: pgerrcode.LockNotAvailable, Message: "canceling statement due to lock timeout"}
	serialization := &pgconn.PgError{Code: pgerrcode.SerializationFailure, Message: "could not serialize access"}
	other := errors.New("synthetic invariant failure")
	cases := []struct {
		name    string
		value   any
		counted bool
		report  bool
	}{
		{name: "lock timeout", value: lockTimeout, counted: true},
		{name: "wrapped lock timeout", value: fmt.Errorf("synthetic transaction: %w", lockTimeout), counted: true},
		{name: "lock timeout joined with its statement stop", value: errors.Join(lockTimeout, context.Canceled, context.Canceled), counted: true},
		{name: "lock timeout joined with a closed database context", value: errors.Join(lockTimeout, DbContextDoneError), counted: true},
		{name: "batch of lock timeouts", value: fmt.Errorf("synthetic visit: %w", NewErrorCauseBatch([]error{lockTimeout, lockTimeout})), counted: true},
		{name: "shutdown alone", value: errors.Join(context.Canceled, context.Canceled)},
		{name: "lock timeout joined with a failure", value: errors.Join(lockTimeout, other), report: true},
		{name: "wrapped mixed join", value: fmt.Errorf("synthetic visit: %w", errors.Join(lockTimeout, context.Canceled, other)), report: true},
		{name: "another database error", value: serialization, report: true},
		{name: "text", value: "canceling statement due to lock timeout", report: true},
	}
	for _, c := range cases {
		reports = 0
		counted := testutil.ToFloat64(lockTimeoutRecoveredCounter)
		var handled error
		recovered := HandleError(func() { panic(c.value) }, func(err error) { handled = err })
		if recovered != c.value || handled == nil {
			t.Errorf("%s: recovery or handler lost", c.name)
		}
		delta := testutil.ToFloat64(lockTimeoutRecoveredCounter) - counted
		if c.counted != (delta == 1) || (!c.counted && delta != 0) {
			t.Errorf("%s: counted %v lock timeouts", c.name, delta)
		}
		if c.report != (reports == 1) || (!c.report && reports != 0) {
			t.Errorf("%s: reported %d times", c.name, reports)
		}
	}
}
