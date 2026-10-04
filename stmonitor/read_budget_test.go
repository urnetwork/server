// Public read controls use actual isolated PostgreSQL sessions, real blocked
// queries and joined cancellation. No successful snapshot is supplied by hooks.
package stmonitor_test

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/stmonitor"
	"github.com/urnetwork/server/stmonitor/testfixture"
)

// Actual session settings and the actual child context share the supported
// one-minute attempt instead of truncating reads at ten or fifteen seconds.
func TestOperatorReadBudgetPublicSessionHasSixtySecondBounds(t *testing.T) {
	f := testfixture.New(t)
	observed := 0
	var startedAt time.Time
	ctx := stmonitor.ObserveReadForTest(t.Context(), nil, nil, func(ctx context.Context, conn *pgx.Conn, tx pgx.Tx) error {
		observed++
		var statement, lock, idle bool
		var readOnly, isolation string
		if err := tx.QueryRow(ctx, `SELECT current_setting('statement_timeout')::interval=interval '60 seconds',current_setting('lock_timeout')::interval=interval '60 seconds',current_setting('idle_in_transaction_session_timeout')::interval=interval '60 seconds',current_setting('transaction_read_only'),current_setting('transaction_isolation')`).Scan(&statement, &lock, &idle, &readOnly, &isolation); err != nil {
			return err
		}
		deadline, ok := ctx.Deadline()
		if !statement || !lock || !idle || readOnly != "on" || isolation != "repeatable read" || conn.Config().ConnectTimeout != 60*time.Second || !ok || deadline.Before(startedAt.Add(60*time.Second)) || time.Until(deadline) > 60*time.Second || time.Until(deadline) <= 0 {
			return fmt.Errorf("actual owned session budget differs: %t %t %t %s %s %s %t", statement, lock, idle, readOnly, isolation, conn.Config().ConnectTimeout, ok)
		}
		return nil
	})
	startedAt = time.Now()
	value, err := stmonitor.Read(ctx, f.Dsn, f.Source)
	if err != nil || value == nil || observed != 1 || stmonitor.ReadTimeout != 300*time.Second || !value.Source.Equal(f.Source) {
		t.Fatal("public read did not use the original bounded session", value, err, observed)
	}
	waitReader(t, f, false)
}

// Killing the actual blocked backend reproduces a transport failure without a
// synthetic read result. A retry cannot begin before the old backend is gone.
func TestOperatorReadBudgetPublicBackendLossRecoversAfterJoin(t *testing.T) {
	f := testfixture.New(t)
	lock, err := f.Database.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err := lock.Exec(t.Context(), `LOCK TABLE st_epoch IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	retrying, resume := make(chan struct{}), make(chan struct{})
	waits := 0
	ctx := stmonitor.ObserveReadForTest(parent, nil, func(ctx context.Context, delay time.Duration) error {
		waits++
		if waits != 1 || delay != time.Second {
			return errors.New("backend loss escaped its first retry boundary")
		}
		close(retrying)
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)
	type result struct {
		value *stmonitor.Snapshot
		err   error
	}
	done := make(chan result, 1)
	go func() { value, err := stmonitor.Read(ctx, f.Dsn, f.Source); done <- result{value: value, err: err} }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			<-done
		}
	}()
	waitReader(t, f, true)
	var firstPid uint32
	if err := f.Admin.QueryRow(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=$1 AND application_name='sn-operator-monitor' AND wait_event_type='Lock'`, f.Source.Database).Scan(&firstPid); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := f.Admin.QueryRow(t.Context(), `SELECT pg_terminate_backend($1)`, firstPid).Scan(&terminated); err != nil || !terminated {
		t.Fatal("actual blocked backend did not terminate", err, terminated)
	}
	select {
	case <-retrying:
	case result := <-done:
		joined = true
		t.Fatal("transient backend loss escaped without its retry", result.value, result.err)
	case <-t.Context().Done():
		t.Fatal("canceled before actual retry boundary")
	}
	var remaining int
	if err := f.Admin.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE pid=$1`, firstPid).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("retry retained the failed database owner", remaining, err)
	}
	close(resume)
	waitReader(t, f, true)
	var nextPid uint32
	if err := f.Admin.QueryRow(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=$1 AND application_name='sn-operator-monitor' AND wait_event_type='Lock'`, f.Source.Database).Scan(&nextPid); err != nil || nextPid == firstPid {
		t.Fatal("retry failed to own a new independent snapshot", nextPid, err)
	}
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	read := <-done
	joined = true
	if read.err != nil || read.value == nil || waits != 1 || !read.value.Source.Equal(f.Source) || read.value.Mirror == nil {
		t.Fatal("actual retry failed to recover the original source", read.value, read.err, waits)
	}
	waitReader(t, f, false)
}

// Advancing only the retained wait clock exhausts exactly one operation budget
// while each attempt still owns a real configured read-only transaction.
func TestOperatorReadBudgetPublicExhaustionPreservesOriginalCause(t *testing.T) {
	f := testfixture.New(t)
	stamp, waited, attempts, waits := time.Now(), time.Duration(0), 0, 0
	ctx := stmonitor.ObserveReadForTest(t.Context(), func() time.Time { return stamp }, func(ctx context.Context, delay time.Duration) error {
		waits++
		if delay <= 0 || delay > 10*time.Second || ctx.Err() != nil {
			return errors.New("owned read wait escaped its bounded window")
		}
		waited += delay
		stamp = stamp.Add(delay)
		return nil
	}, func(ctx context.Context, _ *pgx.Conn, tx pgx.Tx) error {
		attempts++
		var observed int
		if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&observed); err != nil || observed != 1 {
			return errors.Join(errors.New("actual read transaction unavailable"), err)
		}
		return syscall.EIO
	})
	value, err := stmonitor.Read(ctx, f.Dsn, f.Source)
	if value != nil || !errors.Is(err, syscall.EIO) || !errors.Is(err, context.DeadlineExceeded) || attempts < 2 || waits != attempts || waited != 300*time.Second {
		t.Fatal("read exhaustion reset or lost its original budget", value, err, attempts, waits, waited)
	}
	waitReader(t, f, false)
}

// Cancellation between joined attempts preserves the first actual unavailable
// observation and cannot leave a connection alive behind the returned error.
func TestOperatorReadBudgetPublicCancellationPreservesOriginalCause(t *testing.T) {
	f := testfixture.New(t)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts, waits := 0, 0
	ctx := stmonitor.ObserveReadForTest(parent, nil, func(ctx context.Context, _ time.Duration) error { waits++; cancel(); return ctx.Err() }, func(context.Context, *pgx.Conn, pgx.Tx) error { attempts++; return syscall.EIO })
	value, err := stmonitor.Read(ctx, f.Dsn, f.Source)
	if value != nil || !errors.Is(err, syscall.EIO) || !errors.Is(err, context.Canceled) || attempts != 1 || waits != 1 {
		t.Fatal("canceled retry lost original evidence or admission", value, err, attempts, waits)
	}
	waitReader(t, f, false)
}

// An explicit caller minute is the same immutable deadline for every attempt;
// the reader's normal five-minute default never extends that owner boundary.
func TestOperatorReadBudgetPublicCallerMinuteIsPreserved(t *testing.T) {
	f := testfixture.New(t)
	stamp, waited, attempts := time.Now(), time.Duration(0), 0
	deadline := stamp.Add(60 * time.Second)
	parent, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	ctx := stmonitor.ObserveReadForTest(parent, func() time.Time { return stamp }, func(_ context.Context, delay time.Duration) error {
		waited += delay
		stamp = stamp.Add(delay)
		return nil
	}, func(ctx context.Context, _ *pgx.Conn, _ pgx.Tx) error {
		attempts++
		observed, ok := ctx.Deadline()
		if !ok || !observed.Equal(deadline) {
			return errors.New("attempt extended the original caller deadline")
		}
		return syscall.EIO
	})
	value, err := stmonitor.Read(ctx, f.Dsn, f.Source)
	if value != nil || !errors.Is(err, syscall.EIO) || !errors.Is(err, context.DeadlineExceeded) || attempts < 2 || waited != 60*time.Second {
		t.Fatal("reader reset or extended the caller's original minute", value, err, attempts, waited)
	}
	waitReader(t, f, false)
}

// A changed actual database privilege is a completed identity refusal even
// when the immediately preceding connection failed with a retryable cause.
func TestOperatorReadBudgetPublicIdentityDominatesEarlierUnavailable(t *testing.T) {
	f := testfixture.New(t)
	attempts, waits := 0, 0
	ctx := stmonitor.ObserveReadForTest(t.Context(), nil, func(ctx context.Context, _ time.Duration) error {
		waits++
		_, err := f.Database.Exec(ctx, "GRANT UPDATE ON st_publish TO "+pgx.Identifier{f.Source.User}.Sanitize())
		return err
	}, func(context.Context, *pgx.Conn, pgx.Tx) error {
		attempts++
		if attempts == 1 {
			return syscall.EIO
		}
		return nil
	})
	value, err := stmonitor.Read(ctx, f.Dsn, f.Source)
	var classified *stmonitor.ReadError
	if value != nil || !errors.As(err, &classified) || classified.Code != "identity" || !errors.Is(err, syscall.EIO) || attempts != 2 || waits != 1 {
		t.Fatal("actual identity refusal inherited a previous read retry", value, err, attempts, waits)
	}
	waitReader(t, f, false)
}

// An unknown joined cause never inherits a known errno's permission to retry.
func TestOperatorReadBudgetPublicMixedUnknownCauseRefusesRetry(t *testing.T) {
	f := testfixture.New(t)
	unknown := errors.New("synthetic completed refusal")
	attempts, waits := 0, 0
	ctx := stmonitor.ObserveReadForTest(t.Context(), nil, func(context.Context, time.Duration) error { waits++; return nil }, func(context.Context, *pgx.Conn, pgx.Tx) error { attempts++; return errors.Join(syscall.EIO, unknown) })
	value, err := stmonitor.Read(ctx, f.Dsn, f.Source)
	if value != nil || !errors.Is(err, unknown) || !errors.Is(err, syscall.EIO) || attempts != 1 || waits != 0 {
		t.Fatal("mixed hard cause borrowed a transport retry", value, err, attempts, waits)
	}
	waitReader(t, f, false)
}
