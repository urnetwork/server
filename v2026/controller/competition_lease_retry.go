// Competition liveness retries only completed lock-timeout failures. Every
// attempt retains its owner's cancellation and last confirmed authority window.
package controller

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Each worker or supervisor owns its policy and deterministic clock/wait seams.
// No connection, transaction or mutable budget is retained between attempts.
type competitionLeaseRetrySettings struct {
	// Positive total operation budget; non-positive disables additional attempts.
	Timeout time.Duration
	// Positive delay after a rolled-back lock timeout; non-positive disables retries.
	Interval time.Duration
	// Includes the first operation; values below two disable additional attempts.
	MaximumAttempts int
	// Nil uses the server clock. Successful attempt starts bound worker lease acks.
	Now func() time.Time
	// Nil uses a cancellable timer. Tests advance an owned clock or explicit barrier.
	Wait func(context.Context, time.Duration) error
}

// Three two-second lock waits fit within the shared eight-second operation
// budget; a live lease or parent deadline can shorten it, never extend it.
func defaultCompetitionLeaseRetrySettings() competitionLeaseRetrySettings {
	return competitionLeaseRetrySettings{Timeout: 8 * time.Second, Interval: 250 * time.Millisecond, MaximumAttempts: 3,
		Now: server.NowUtc, Wait: func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		}}
}

// The callback must finish its entire transaction before returning an error.
// Unknown lease expiry permits the historical first call, but no retry; only a
// successful real heartbeat may establish a confirmed window for its owner.
func retryCompetitionLeaseOperation(ctx context.Context, leaseExpiresAt time.Time, settings competitionLeaseRetrySettings, operation func(context.Context) error) (time.Time, error) {
	defaults := defaultCompetitionLeaseRetrySettings()
	if settings.Now == nil {
		settings.Now = defaults.Now
	}
	if settings.Wait == nil {
		settings.Wait = defaults.Wait
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	now := settings.Now()
	if !leaseExpiresAt.IsZero() && !now.Before(leaseExpiresAt) {
		return time.Time{}, ErrLeaseLost
	}
	deadline := leaseExpiresAt
	if settings.Timeout > 0 && (deadline.IsZero() || now.Add(settings.Timeout).Before(deadline)) {
		deadline = now.Add(settings.Timeout)
	}
	operationCtx, cancel := context.WithCancel(ctx)
	if !deadline.IsZero() {
		cancel()
		operationCtx, cancel = context.WithTimeout(ctx, deadline.Sub(now))
	}
	defer cancel()
	var lastError error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		now = settings.Now()
		if !leaseExpiresAt.IsZero() && !now.Before(leaseExpiresAt) {
			return time.Time{}, ErrLeaseLost
		}
		if !deadline.IsZero() && !now.Before(deadline) && lastError != nil {
			return time.Time{}, lastError
		}
		if err := operationCtx.Err(); err != nil {
			return time.Time{}, err
		}
		attemptStartedAt := now
		err := operation(operationCtx)
		if parentError := ctx.Err(); parentError != nil {
			return time.Time{}, parentError
		}
		if err == nil {
			now = settings.Now()
			if !leaseExpiresAt.IsZero() && !now.Before(leaseExpiresAt) {
				return time.Time{}, ErrLeaseLost
			}
			if !deadline.IsZero() && !now.Before(deadline) {
				return time.Time{}, context.DeadlineExceeded
			}
			if err := operationCtx.Err(); err != nil {
				return time.Time{}, err
			}
			return attemptStartedAt, nil
		}
		// Logging refusal taxonomy also permits shutdown leaves; operational
		// retries require a complete graph containing only actual lock timeouts.
		causes := server.InspectErrorCauseBatch(err)
		if !causes.Complete || causes.NilBranches != 0 {
			return time.Time{}, err
		}
		foundLockTimeout := false
		for _, cause := range causes.Nodes {
			if cause.Leaf {
				lockError, ok := cause.Err.(*pgconn.PgError)
				if !ok || lockError == nil || lockError.Code != "55P03" {
					return time.Time{}, err
				}
				foundLockTimeout = true
			}
		}
		if !foundLockTimeout {
			return time.Time{}, err
		}
		if leaseExpiresAt.IsZero() || settings.Timeout <= 0 || settings.Interval <= 0 || attempt+1 >= settings.MaximumAttempts {
			return time.Time{}, err
		}
		lastError = err
		now = settings.Now()
		if !now.Before(leaseExpiresAt) {
			return time.Time{}, ErrLeaseLost
		}
		if !now.Before(deadline) {
			return time.Time{}, err
		}
		delay := min(settings.Interval, deadline.Sub(now))
		if err := settings.Wait(operationCtx, delay); err != nil {
			if parentError := ctx.Err(); parentError != nil {
				return time.Time{}, parentError
			}
			return time.Time{}, err
		}
	}
}
