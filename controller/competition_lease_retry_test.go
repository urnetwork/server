// Synthetic clock/wait transitions prove retry admission without wall-clock
// sleeps, PostgreSQL, external executors or changes to a submission lifetime.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// A malformed cause graph must fail closed before any unbounded Is traversal.
type competitionLeaseRetryCycleError struct{}

// The diagnostic value contains no private metadata.
func (self *competitionLeaseRetryCycleError) Error() string { return "synthetic cyclic cause" }

// Its deliberate cycle is bounded by the production cause inspector.
func (self *competitionLeaseRetryCycleError) Unwrap() error { return self }

// A custom As hook must not impersonate a genuine database rollback leaf.
type competitionLeaseRetryForgingLeaf struct{ asCalls int }

// Fixed diagnostic content keeps the control independent of backend messages.
func (self *competitionLeaseRetryForgingLeaf) Error() string { return "synthetic forged lock leaf" }

// Counting proves operational classification never invokes arbitrary leaf hooks.
func (self *competitionLeaseRetryForgingLeaf) As(target any) bool {
	self.asCalls++
	if value, ok := target.(**pgconn.PgError); ok {
		*value = &pgconn.PgError{Code: "55P03"}
		return true
	}
	return false
}

// An owned clock is safe across the test's explicit worker/supervisor barriers.
type competitionLeaseRetryClock struct {
	stateLock sync.Mutex
	now       time.Time
}

// Read and advance share one lock; no scheduler timing determines authority.
func (self *competitionLeaseRetryClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// Only the test's acknowledged wait or fence transition advances its clock.
func (self *competitionLeaseRetryClock) Advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(duration)
}

// A later successful attempt, not the first refused one, anchors the next ack.
func TestCompetitionLeaseRetryReturnsSuccessfulAttemptStart(t *testing.T) {
	clock := &competitionLeaseRetryClock{now: time.Now()}
	settings := defaultCompetitionLeaseRetrySettings()
	settings.Now = clock.Now
	waits, calls := 0, 0
	settings.Wait = func(_ context.Context, delay time.Duration) error {
		waits++
		clock.Advance(delay)
		return nil
	}
	initial := clock.Now()
	attemptStartedAt, err := retryCompetitionLeaseOperation(t.Context(), initial.Add(time.Minute), settings, func(context.Context) error {
		calls++
		if calls == 1 {
			clock.Advance(time.Second)
			return fmt.Errorf("synthetic statement: %w", &pgconn.PgError{Code: "55P03"})
		}
		return nil
	})
	if err != nil || calls != 2 || waits != 1 || !attemptStartedAt.Equal(initial.Add(time.Second+settings.Interval)) {
		t.Fatalf("retry lost its successful attempt start: calls=%d waits=%d start=%s, %v", calls, waits, attemptStartedAt, err)
	}
}

// Retry classification never treats another failure or a mixed lost fence as
// permission to keep evaluation alive, even if a lock error is also wrapped.
func TestCompetitionLeaseRetryRejectsNonLockAndLostAuthorityErrors(t *testing.T) {
	for _, check := range []struct {
		name  string
		cause error
	}{
		{name: "lost fence", cause: ErrLeaseLost},
		{name: "mixed lost fence", cause: errors.Join(ErrLeaseLost, &pgconn.PgError{Code: "55P03"})},
		{name: "canceled", cause: context.Canceled},
		{name: "deadline", cause: context.DeadlineExceeded},
		{name: "serialization", cause: &pgconn.PgError{Code: "40001"}},
		{name: "network", cause: &pgconn.PgError{Code: "08006"}},
		{name: "mixed network", cause: errors.Join(&pgconn.PgError{Code: "55P03"}, &pgconn.PgError{Code: "08006"})},
		{name: "mixed unrelated", cause: errors.Join(&pgconn.PgError{Code: "55P03"}, errors.New("synthetic uncertain completion"))},
		{name: "mixed database shutdown", cause: errors.Join(&pgconn.PgError{Code: "55P03"}, server.DbContextDoneError)},
		{name: "mixed done alias", cause: errors.Join(&pgconn.PgError{Code: "55P03"}, errors.New("Done"))},
		{name: "mixed punctuated done alias", cause: errors.Join(&pgconn.PgError{Code: "55P03"}, errors.New("Done."))},
		{name: "nil branch", cause: server.NewErrorCauseBatch([]error{&pgconn.PgError{Code: "55P03"}, nil})},
		{name: "typed nil branch", cause: errors.Join(&pgconn.PgError{Code: "55P03"}, (*pgconn.PgError)(nil))},
		{name: "cyclic branch", cause: errors.Join(&pgconn.PgError{Code: "55P03"}, &competitionLeaseRetryCycleError{})},
		{name: "unrelated", cause: errors.New("synthetic non-postgresql failure")},
	} {
		settings := defaultCompetitionLeaseRetrySettings()
		settings.Wait = func(context.Context, time.Duration) error {
			t.Fatalf("%s was retried", check.name)
			return nil
		}
		calls := 0
		_, err := retryCompetitionLeaseOperation(t.Context(), settings.Now().Add(time.Minute), settings, func(context.Context) error {
			calls++
			return check.cause
		})
		if calls != 1 || !errors.Is(err, check.cause) {
			t.Fatalf("%s changed failure identity or attempts: calls=%d, %v", check.name, calls, err)
		}
	}
}

// Classification is bounded graph inspection plus concrete leaf type checks,
// never an errors.As dispatch controlled by an arbitrary returned error.
func TestCompetitionLeaseRetryDoesNotInvokeLeafAsHooks(t *testing.T) {
	leaf := &competitionLeaseRetryForgingLeaf{}
	cause := errors.Join(&pgconn.PgError{Code: "55P03"}, leaf)
	settings := defaultCompetitionLeaseRetrySettings()
	calls := 0
	_, err := retryCompetitionLeaseOperation(t.Context(), settings.Now().Add(time.Minute), settings, func(context.Context) error { calls++; return cause })
	if calls != 1 || err != cause || leaf.asCalls != 0 {
		t.Fatal("operational retry invoked a custom As hook or accepted its forged database identity")
	}
}

// Multiple complete lock-only leaves may retry, unlike mixed shutdown causes.
func TestCompetitionLeaseRetryAcceptsOnlyCompleteLockGraphs(t *testing.T) {
	clock := &competitionLeaseRetryClock{now: time.Now()}
	settings := defaultCompetitionLeaseRetrySettings()
	settings.Now = clock.Now
	settings.Wait = func(_ context.Context, delay time.Duration) error { clock.Advance(delay); return nil }
	calls := 0
	_, err := retryCompetitionLeaseOperation(t.Context(), clock.Now().Add(time.Minute), settings, func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.Join(&pgconn.PgError{Code: "55P03"}, fmt.Errorf("synthetic rollback: %w", &pgconn.PgError{Code: "55P03"}))
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("complete all-lock graph did not recover: calls=%d, %v", calls, err)
	}
}

// Expiry and caller cancellation are admission gates, including the first call.
func TestCompetitionLeaseRetryRejectsExpiredOrCanceledBeforeFirstCall(t *testing.T) {
	settings := defaultCompetitionLeaseRetrySettings()
	now := settings.Now()
	settings.Now = func() time.Time { return now }
	called := false
	operation := func(context.Context) error { called = true; return nil }
	if _, err := retryCompetitionLeaseOperation(t.Context(), now, settings, operation); !errors.Is(err, ErrLeaseLost) || called {
		t.Fatalf("expired authority reached an operation: called=%t, %v", called, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := retryCompetitionLeaseOperation(ctx, now.Add(time.Minute), settings, operation); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled owner reached an operation: called=%t, %v", called, err)
	}
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Minute))
	defer cancel()
	if _, err := retryCompetitionLeaseOperation(ctx, now.Add(time.Minute), settings, operation); !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("expired execution reached an operation: called=%t, %v", called, err)
	}
}

// Legacy stores without a known initial expiry get one actual call, not an
// invented authority window under which an unsuccessful call could be replayed.
func TestCompetitionLeaseRetryUnknownExpiryDoesNotGrantRetries(t *testing.T) {
	settings := defaultCompetitionLeaseRetrySettings()
	cause := &pgconn.PgError{Code: "55P03"}
	calls := 0
	settings.Wait = func(context.Context, time.Duration) error { t.Fatal("unknown authority was retried"); return nil }
	_, err := retryCompetitionLeaseOperation(t.Context(), time.Time{}, settings, func(context.Context) error { calls++; return cause })
	if calls != 1 || !errors.Is(err, cause) {
		t.Fatalf("unknown authority changed the historical first-call failure: calls=%d, %v", calls, err)
	}
}

// Attempts cannot obtain a new total budget after consuming the first one.
func TestCompetitionLeaseRetryHasOneEightSecondBudget(t *testing.T) {
	clock := &competitionLeaseRetryClock{now: time.Now()}
	settings := defaultCompetitionLeaseRetrySettings()
	if settings.Timeout != 8*time.Second {
		t.Fatal("default total retry budget changed")
	}
	settings.MaximumAttempts = 20
	settings.Now = clock.Now
	settings.Wait = func(_ context.Context, delay time.Duration) error { clock.Advance(delay); return nil }
	cause := &pgconn.PgError{Code: "55P03"}
	calls := 0
	var operationDeadline time.Time
	_, err := retryCompetitionLeaseOperation(t.Context(), clock.Now().Add(time.Minute), settings, func(ctx context.Context) error {
		calls++
		deadline, ok := ctx.Deadline()
		if calls == 1 {
			operationDeadline = deadline
		}
		if !ok || !deadline.Equal(operationDeadline) {
			t.Fatal("operation did not inherit the bounded retry context")
		}
		clock.Advance(4 * time.Second)
		return cause
	})
	if calls != 2 || !errors.Is(err, cause) {
		t.Fatalf("attempt restarted its elapsed budget: calls=%d, %v", calls, err)
	}
}

// A known lease that expires during backoff is terminal before the next call.
func TestCompetitionLeaseRetryStopsAtConfirmedExpiry(t *testing.T) {
	clock := &competitionLeaseRetryClock{now: time.Now()}
	settings := defaultCompetitionLeaseRetrySettings()
	settings.Timeout, settings.Interval = time.Hour, 30*time.Minute
	settings.Now = clock.Now
	leaseExpiresAt := clock.Now().Add(settings.Interval)
	settings.Wait = func(_ context.Context, delay time.Duration) error { clock.Advance(delay); return nil }
	calls := 0
	_, err := retryCompetitionLeaseOperation(t.Context(), leaseExpiresAt, settings, func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "55P03"}
	})
	if calls != 1 || !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired lease was retried: calls=%d, %v", calls, err)
	}
}

// A successful response arriving after the confirmed window cannot revive
// authority, even when the injected clock advances without a wall timer firing.
func TestCompetitionLeaseRetryLateSuccessDoesNotReviveAuthority(t *testing.T) {
	clock := &competitionLeaseRetryClock{now: time.Now()}
	settings := defaultCompetitionLeaseRetrySettings()
	settings.Timeout = time.Hour
	settings.Now = clock.Now
	leaseExpiresAt := clock.Now().Add(30 * time.Minute)
	calls := 0
	_, err := retryCompetitionLeaseOperation(t.Context(), leaseExpiresAt, settings, func(context.Context) error {
		calls++
		clock.Advance(30 * time.Minute)
		return nil
	})
	if calls != 1 || !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late success revived expired authority: calls=%d, %v", calls, err)
	}
}

// The attempt cap is independent of elapsed time and never resets on failure.
func TestCompetitionLeaseRetryStopsAfterThreeAttempts(t *testing.T) {
	clock := &competitionLeaseRetryClock{now: time.Now()}
	settings := defaultCompetitionLeaseRetrySettings()
	settings.Timeout = time.Hour
	settings.Now = clock.Now
	cause := &pgconn.PgError{Code: "55P03"}
	calls, waits := 0, 0
	settings.Wait = func(_ context.Context, delay time.Duration) error { waits++; clock.Advance(delay); return nil }
	_, err := retryCompetitionLeaseOperation(t.Context(), clock.Now().Add(time.Hour), settings, func(context.Context) error { calls++; return cause })
	if calls != 3 || waits != 2 || !errors.Is(err, cause) {
		t.Fatalf("attempt cap changed: calls=%d waits=%d, %v", calls, waits, err)
	}
}

// A shorter parent deadline is passed to every attempted store operation.
func TestCompetitionLeaseRetryPreservesExactParentDeadline(t *testing.T) {
	clock := &competitionLeaseRetryClock{now: time.Now()}
	settings := defaultCompetitionLeaseRetrySettings()
	settings.Timeout = time.Hour
	settings.Now = clock.Now
	settings.Wait = func(_ context.Context, delay time.Duration) error { clock.Advance(delay); return nil }
	deadline := time.Now().Add(10 * time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	calls := 0
	_, err := retryCompetitionLeaseOperation(ctx, clock.Now().Add(time.Hour), settings, func(ctx context.Context) error {
		calls++
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(deadline) {
			t.Fatal("retry changed its parent's existing deadline")
		}
		if calls == 1 {
			return &pgconn.PgError{Code: "55P03"}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("bounded parent operation did not recover: calls=%d, %v", calls, err)
	}
}

// A parent stop wakes the injected wait and cannot dispatch another operation.
func TestCompetitionLeaseRetryCancellationStopsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	settings := defaultCompetitionLeaseRetrySettings()
	settings.Wait = func(ctx context.Context, _ time.Duration) error { cancel(); <-ctx.Done(); return ctx.Err() }
	calls := 0
	_, err := retryCompetitionLeaseOperation(ctx, settings.Now().Add(time.Minute), settings, func(context.Context) error {
		calls++
		return &pgconn.PgError{Code: "55P03"}
	})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled owner retried its lock failure: calls=%d, %v", calls, err)
	}
}
