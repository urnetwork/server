package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func captureDbRollbackPanic(callback func()) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	callback()
	return nil
}

func createDbRollbackEffectTable(t testing.TB) {
	t.Helper()
	Db(context.Background(), func(conn PgConn) {
		RaisePgResult(conn.Exec(
			context.Background(),
			`CREATE TABLE tx_rollback_effect (value integer NOT NULL)`,
		))
	}, OptReadWrite())
}

func requireDbRollbackEffectCount(t testing.TB, want int) {
	t.Helper()
	var got int
	Db(context.Background(), func(conn PgConn) {
		Raise(conn.QueryRow(
			context.Background(),
			`SELECT count(*) FROM tx_rollback_effect`,
		).Scan(&got))
	})
	if got != want {
		t.Fatalf("rollback effect count = %d; want %d", got, want)
	}
}

// A transient callback failure is retained while txWithPool rolls it back and
// reaches its existing caller-cancellation decision. Before rollback cleanup
// became best-effort, the canceled rollback panicked, the outer cleanup called
// Rollback again, and pgx.ErrTxClosed replaced both earlier failures.
func TestTxCanceledTransientRollbackDoesNotBecomeTxClosed(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		createDbRollbackEffectTable(t)
		ctx, cancel := context.WithCancel(context.Background())
		original := &pgconn.PgError{
			Code:    "40001",
			Message: "synthetic serialization failure",
		}

		recovered := captureDbRollbackPanic(func() {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO tx_rollback_effect (value) VALUES (1)`))
				cancel()
				panic(original)
			})
		})

		recoveredErr, ok := recovered.(error)
		if !ok {
			t.Fatalf("recovered value = %#v; want error", recovered)
		}
		if errors.Is(recoveredErr, pgx.ErrTxClosed) {
			t.Fatalf("rollback cleanup replaced the transaction outcome with pgx.ErrTxClosed: %v", recoveredErr)
		}
		if recoveredErr != DbContextDoneError {
			t.Fatalf("recovered error = %v; want DbContextDoneError", recoveredErr)
		}
		requireDbRollbackEffectCount(t, 0)
	})
}

// A rollback error is cleanup evidence, not a replacement for an application
// panic. The transaction write must still be absent after the detached cleanup.
func TestTxCanceledNonTransientRollbackPreservesPanic(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		createDbRollbackEffectTable(t)
		ctx, cancel := context.WithCancel(context.Background())
		original := errors.New("synthetic transaction callback failure")

		recovered := captureDbRollbackPanic(func() {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO tx_rollback_effect (value) VALUES (1)`))
				cancel()
				panic(original)
			})
		})

		if recovered != original {
			t.Fatalf("recovered value = %#v; want original sentinel %#v", recovered, original)
		}
		requireDbRollbackEffectCount(t, 0)
	})
}

type recordingRollbackTx struct {
	PgTx
	calls       int
	contextErr  error
	hasDeadline bool
	remaining   time.Duration
}

func (r *recordingRollbackTx) Rollback(ctx context.Context) error {
	r.calls++
	r.contextErr = ctx.Err()
	deadline, ok := ctx.Deadline()
	r.hasDeadline = ok
	if ok {
		r.remaining = time.Until(deadline)
	}
	return pgx.ErrTxClosed
}

func TestRollbackTxUsesLiveBoundedContext(t *testing.T) {
	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	tx := &recordingRollbackTx{}

	rollbackTx(callerCtx, tx)

	if tx.calls != 1 {
		t.Fatalf("Rollback calls = %d; want 1", tx.calls)
	}
	if tx.contextErr != nil {
		t.Fatalf("Rollback received canceled context: %v", tx.contextErr)
	}
	if !tx.hasDeadline {
		t.Fatal("Rollback context has no deadline")
	}
	if tx.remaining <= 0 || PgRollbackTimeout < tx.remaining {
		t.Fatalf("Rollback context remaining = %v; want within (0, %v]", tx.remaining, PgRollbackTimeout)
	}
}
