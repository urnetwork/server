// Against the local test PostgreSQL, a caller's stop or a deadline from other
// work must leave the pooled session's backend in place. Each test owns a
// one-slot pool, so a destroyed pooled connection shows up as a new backend
// pid on the next checkout.
package server

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
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

// Waits until the backend's advisory lock request is queued behind a holder.
// This only synchronizes the test with the server; the proof is the outcome.
func waitForAdvisoryLockWait(t testing.TB, pid uint32) {
	t.Helper()
	for range 1000 {
		var waiting bool
		Db(context.Background(), func(conn PgConn) {
			Raise(conn.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)`, pid).Scan(&waiting))
		}, OptNoRetry())
		if waiting {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatal("test stopped before the statement reached its lock wait")
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("the statement never waited on the held lock")
}

// A caller that stops while its statement waits on a lock keeps the session's
// backend. The statement completes once the lock is granted, reports the stop,
// and the transaction rolls back on the same backend; pgx would otherwise
// cancel the wait and destroy the session, which behind the transaction pooler
// leaves its server idle in transaction until the pooler destroys it.
func TestTxStopDuringLockWaitKeepsBackend(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		pool := newSingleConnectionDbTestPool(t)
		defer pool.close()
		before := singleSlotBackendPid(t, pool)
		id := NewId()
		lockKey := int64(binary.BigEndian.Uint64(id[:8]))

		holding := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		holderDone := make(chan any, 1)
		go func() {
			holderDone <- captureDbErrorPanic(func() {
				Tx(context.Background(), func(tx PgTx) {
					RaisePgResult(tx.Exec(context.Background(), `SELECT pg_advisory_xact_lock($1)`, lockKey))
					close(holding)
					<-release
				}, TxReadCommitted, OptNoRetry())
			})
		}()
		<-holding

		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		afterStatement := 0
		done := make(chan any, 1)
		go func() {
			done <- captureDbErrorPanic(func() {
				txWithPool(ctx, pool, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1)`, lockKey))
					afterStatement++
				}, TxReadCommitted, OptNoRetry(), txStatementStopTimeout(time.Hour))
			})
		}()
		waitForAdvisoryLockWait(t, before)
		stop()
		releaseOnce.Do(func() { close(release) })
		recovered := <-done
		if holder := <-holderDone; holder != nil {
			t.Fatalf("lock holder failed: %v", holder)
		}
		if err, _ := recovered.(error); !errors.Is(err, context.Canceled) || afterStatement != 0 {
			t.Fatalf("the stopped statement lost its caller's stop or the callback went on: %v %d", recovered, afterStatement)
		}
		if after := singleSlotBackendPid(t, pool); after != before {
			t.Fatalf("a caller stopped during a lock wait replaced backend %d with %d", before, after)
		}
	})
}
