// Result cleanup must expose late errors without replacing an earlier outcome.
package server

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Unused methods come from the embedded interface; these controls exercise only
// the generic result helper's error and cleanup contract, not a fake database.
type resultCloseTestRows struct {
	PgResult
	err        error
	closeErr   error
	closePanic any
	closeCalls int
}

func (self *resultCloseTestRows) Err() error { return self.err }

func (self *resultCloseTestRows) Close() {
	self.closeCalls++
	self.err = self.closeErr
	if self.closePanic != nil {
		panic(self.closePanic)
	}
}

// A final transport error can first appear while Close drains the last reply.
// The caller must see that typed cause before it can issue another query.
func TestWithPgResultRaisesLateCloseErrorBeforeContinuation(t *testing.T) {
	late := fmt.Errorf("synthetic final reply: %w", context.DeadlineExceeded)
	rows := &resultCloseTestRows{closeErr: late}
	callbacks, continued := 0, false
	recovered := captureDbErrorPanic(func() {
		WithPgResult(rows, nil, func() { callbacks++ })
		continued = true
	})
	err, _ := recovered.(error)
	if recovered != late || !errors.Is(err, context.DeadlineExceeded) || callbacks != 1 || continued || rows.closeCalls != 1 {
		t.Fatalf("late close cause escaped or changed: same=%t deadline=%t callbacks=%d continued=%t closes=%d",
			recovered == late, errors.Is(err, context.DeadlineExceeded), callbacks, continued, rows.closeCalls)
	}
}

// Both supported callback forms close once and preserve successful completion.
func TestWithPgResultSuccessfulCallbacksCloseOnce(t *testing.T) {
	for _, withRows := range []bool{false, true} {
		rows := &resultCloseTestRows{}
		callbacks := 0
		var callback any = func() { callbacks++ }
		if withRows {
			callback = func(got PgResult) {
				if got != rows {
					panic(errors.New("result callback received another owner"))
				}
				callbacks++
			}
		}
		recovered := captureDbErrorPanic(func() { WithPgResult(rows, nil, callback) })
		if recovered != nil || callbacks != 1 || rows.closeCalls != 1 {
			t.Fatalf("healthy callback changed: result_callback=%t callbacks=%d closes=%d panic=%v", withRows, callbacks, rows.closeCalls, recovered)
		}
	}
}

// An arbitrary callback panic owns the outcome even when cleanup itself panics.
func TestWithPgResultPreservesCallbackPanicDuringCleanup(t *testing.T) {
	original := &struct{ marker int }{marker: 17}
	rows := &resultCloseTestRows{closeErr: context.DeadlineExceeded, closePanic: errors.New("secondary close panic")}
	recovered := captureDbErrorPanic(func() { WithPgResult(rows, nil, func() { panic(original) }) })
	if recovered != original || rows.closeCalls != 1 {
		t.Fatalf("cleanup replaced callback panic: same=%t closes=%d", recovered == original, rows.closeCalls)
	}
}

// Goexit must continue terminating its goroutine; cleanup cannot turn it into
// a panic or permit the code after the helper to execute.
func TestWithPgResultPreservesGoexitDuringCleanup(t *testing.T) {
	rows := &resultCloseTestRows{closeErr: context.DeadlineExceeded, closePanic: errors.New("secondary close panic")}
	done := make(chan struct{})
	var recovered any
	continued := false
	go func() {
		defer close(done)
		defer func() { recovered = recover() }()
		WithPgResult(rows, nil, func() { runtime.Goexit() })
		continued = true
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Goexit result cleanup did not join")
	}
	if recovered != nil || continued || rows.closeCalls != 1 {
		t.Fatalf("cleanup changed Goexit: panic=%v continued=%t closes=%d", recovered, continued, rows.closeCalls)
	}
}

// Capture the pre-existing row error before cleanup can replace its stored
// value or panic. Its typed SQLSTATE remains the caller's original cause.
func TestWithPgResultPreservesEarlierRowError(t *testing.T) {
	original := &pgconn.PgError{Code: "40001", Message: "synthetic earlier row error"}
	rows := &resultCloseTestRows{err: original, closeErr: context.DeadlineExceeded, closePanic: errors.New("secondary close panic")}
	callbacks := 0
	recovered := captureDbErrorPanic(func() { WithPgResult(rows, nil, func() { callbacks++ }) })
	err, _ := recovered.(error)
	var got *pgconn.PgError
	if recovered != original || !errors.As(err, &got) || got.Code != original.Code || callbacks != 1 || rows.closeCalls != 1 {
		t.Fatalf("cleanup replaced earlier row error: same=%t callbacks=%d closes=%d", recovered == original, callbacks, rows.closeCalls)
	}
}

// Query failures retain precedence whether there are rows to clean or not.
func TestWithPgResultPreservesQueryErrorAndClosesReturnedRows(t *testing.T) {
	original := errors.New("synthetic initial query failure")
	rows := &resultCloseTestRows{closeErr: context.DeadlineExceeded, closePanic: errors.New("secondary close panic")}
	callbacks := 0
	recovered := captureDbErrorPanic(func() { WithPgResult(rows, original, func() { callbacks++ }) })
	if recovered != original || callbacks != 0 || rows.closeCalls != 1 {
		t.Fatalf("query failure lost precedence or cleanup: same=%t callbacks=%d closes=%d", recovered == original, callbacks, rows.closeCalls)
	}
	recovered = captureDbErrorPanic(func() { WithPgResult(nil, original, func() { callbacks++ }) })
	if recovered != original || callbacks != 0 {
		t.Fatal("nil failed result changed query failure")
	}
}

// With no earlier failure, a Close panic is itself the outcome, never success.
func TestWithPgResultSurfacesClosePanicAfterSuccessfulCallback(t *testing.T) {
	original := errors.New("synthetic primary close panic")
	rows := &resultCloseTestRows{closePanic: original}
	callbacks, continued := 0, false
	recovered := captureDbErrorPanic(func() {
		WithPgResult(rows, nil, func() { callbacks++ })
		continued = true
	})
	if recovered != original || callbacks != 1 || continued || rows.closeCalls != 1 {
		t.Fatalf("primary close panic changed: same=%t callbacks=%d continued=%t closes=%d", recovered == original, callbacks, continued, rows.closeCalls)
	}
}
