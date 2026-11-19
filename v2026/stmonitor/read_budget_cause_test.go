// Closed classification controls prevent mixed or recursive causes from
// inheriting permission to repeat a database snapshot.
package stmonitor

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The custom classifier lies about matching a soft sentinel.
type readBudgetCustomCause struct{}

// Fixed text keeps the test free of source paths.
func (self *readBudgetCustomCause) Error() string { return "synthetic custom cause" }

// This deliberately false matching claim must never be called by admission.
func (self *readBudgetCustomCause) Is(error) bool { return true }

// A soft nested cause cannot authorize the custom outer classifier.
func (self *readBudgetCustomCause) Unwrap() error { return syscall.EIO }

// Recursive causes must reach a finite refusal without invoking errors.Is.
type readBudgetRecursiveCause struct{}

// Formatting does not recurse through the deliberately cyclic wrapper.
func (self *readBudgetRecursiveCause) Error() string { return "synthetic recursive cause" }

// Recursion is bounded by the reader's explicit cause census.
func (self *readBudgetRecursiveCause) Unwrap() error { return self }

// Explicit known transport/resource errors remain retryable through ordinary
// contextual wrappers, including errno's standard Is implementation.
func TestOperatorReadBudgetKnownCausesRemainRetryable(t *testing.T) {
	for _, cause := range []error{syscall.EIO, syscall.EAGAIN, syscall.ECONNREFUSED, syscall.ECONNRESET, context.DeadlineExceeded, io.EOF, &pgconn.PgError{Code: "57P01"}, &pgconn.PgError{Code: "57014"}, &pgconn.PgError{Code: "55P03"}} {
		if !retryableRead(refuse("unavailable", errors.Join(&os.PathError{Op: "read", Path: "synthetic-source", Err: cause}, context.DeadlineExceeded))) {
			t.Fatalf("known read cause became terminal: %v", cause)
		}
	}
}

// Hard joined causes, user-defined classification and typed-nil/cyclic trees
// cannot acquire a retry from their adjacent transient leaf.
func TestOperatorReadBudgetMixedAndRecursiveCausesRemainTerminal(t *testing.T) {
	var nilRead *ReadError
	for _, cause := range []error{nilRead, &readBudgetCustomCause{}, &readBudgetRecursiveCause{}, &pgconn.PgError{Code: "42501"}, &pgconn.PgError{Code: "42601"}, syscall.EBADF, os.ErrClosed, refuse("identity", nil), refuse("invalid", syscall.EIO), refuse("capacity", nil), errors.New("synthetic unknown cause")} {
		if retryableRead(errors.Join(syscall.EIO, cause)) {
			t.Fatalf("hard cause borrowed transient retry: %v", cause)
		}
		_ = readRefusalCode(cause)
	}
	if code := readRefusalCode(errors.Join(refuse("unavailable", syscall.EIO), refuse("identity", nil))); code != "identity" {
		t.Fatal("earlier unavailable cause masked original identity refusal", code)
	}
}
