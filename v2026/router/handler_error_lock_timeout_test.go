package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// A lock wait the transaction's bound ended (server.PgLockTimeout) answers a
// retryable 503 with a retry hint and a fixed body, however it was wrapped,
// so a client backs off and retries instead of failing on a generic 500. Any
// other database error stays an internal failure with its cause in the log.
func TestRaiseHttpErrorAnswersALockTimeoutAsRetryable(t *testing.T) {
	lockTimeout := &pgconn.PgError{Code: pgerrcode.LockNotAvailable, Message: "canceling statement due to lock timeout"}
	for _, err := range []error{
		lockTimeout,
		fmt.Errorf("[impl]registration: %w", lockTimeout),
		fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", lockTimeout)),
	} {
		w := httptest.NewRecorder()
		if statusError := RaiseHttpError(err, w); !statusError {
			t.Errorf("%v: not answered as a declared status", err)
		}
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
			t.Errorf("%v: answered %d with Retry-After %q, want 503 and 1", err, w.Code, w.Header().Get("Retry-After"))
		}
		if body := strings.TrimSpace(w.Body.String()); body != `{"error":"Temporarily busy. Retry shortly."}` || strings.Contains(body, "lock timeout") {
			t.Errorf("%v: body %q", err, body)
		}
	}

	w := httptest.NewRecorder()
	RaiseHttpError(fmt.Errorf("query: %w", &pgconn.PgError{Code: pgerrcode.UniqueViolation}), w)
	if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" {
		t.Fatalf("another database error answered %d with Retry-After %q, want a plain 500", w.Code, w.Header().Get("Retry-After"))
	}
}
