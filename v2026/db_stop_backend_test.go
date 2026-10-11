// Against the local test PostgreSQL, a caller's stop or a deadline from other
// work must leave the pooled session's backend in place. Each test owns a
// one-slot pool, so a destroyed pooled connection shows up as a new backend
// pid on the next checkout.
package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Returns the backend pid behind the pool's only connection.
func singleSlotBackendPid(t testing.TB, pool *safePgPool) (pid uint32) {
	t.Helper()
	dbWithPool(context.Background(), pool, func(conn PgConn) {
		Raise(conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid))
	}, OptNoRetry())
	return
}

// A deadline-bound call to other work fails inside a transaction after a
// written statement. The rolled-back session keeps its backend.
func TestTxDeadlineFromOtherWorkKeepsBackend(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		pool := newSingleConnectionDbTestPool(t)
		defer pool.close()
		before := singleSlotBackendPid(t, pool)
		otherErr := fmt.Errorf("synthetic deadline-bound admission: %w", context.DeadlineExceeded)
		recovered := captureDbErrorPanic(func() {
			txWithPool(context.Background(), pool, func(tx PgTx) {
				RaisePgResult(tx.Exec(context.Background(), `SELECT 1`))
				panic(otherErr)
			}, TxReadCommitted)
		})
		if recovered != otherErr {
			t.Fatalf("transaction lost its exact cause: %v", recovered)
		}
		if after := singleSlotBackendPid(t, pool); after != before {
			t.Fatalf("other work's deadline replaced backend %d with %d", before, after)
		}
	})
}

// A caller that stopped between checkout and BEGIN keeps the session's
// backend; pgx would otherwise destroy the connection whose BEGIN it refused.
func TestTxStopBeforeBeginKeepsBackend(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		pool := newSingleConnectionDbTestPool(t)
		defer pool.close()
		before := singleSlotBackendPid(t, pool)
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		recovered := captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(PgTx) {
				t.Error("stopped caller ran callback work")
			}, TxReadBeforeBegin(func(PgCanQuery) { stop() }), TxReadCommitted, OptNoRetry())
		})
		if err, _ := recovered.(error); !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped caller lost its cause: %v", recovered)
		}
		if after := singleSlotBackendPid(t, pool); after != before {
			t.Fatalf("a stopped caller replaced backend %d with %d", before, after)
		}
	})
}
