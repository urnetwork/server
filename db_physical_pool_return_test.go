// Physical release receipts follow the actual pgx/puddle resource path. A
// protocol-idle entry is insufficient after reset, expiry, closing, or hooks.
package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A real rolled-back transaction returns the resource before publishing proof.
func TestDbPhysicalPoolReturnAfterAcknowledgedRollback(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	observation := NewDbTxObservation()
	cause := errors.New("synthetic rollback")
	recovered := captureDbErrorPanic(func() {
		txWithPool(t.Context(), pool, func(PgTx) { panic(cause) }, OptNoRetry(), observation)
	})
	snapshot := observation.Snapshot()
	if recovered != cause || snapshot.RollbackAcknowledged != 1 || snapshot.CommitAttempts != 0 ||
		!snapshot.LastRelease.PoolReturned || snapshot.LastRelease.Disposition != DbTxReleaseIdle || snapshot.LastRelease.CleanupComplete ||
		pool.open().Stat().IdleConns() != 1 || pool.open().Stat().AcquiredConns() != 0 {
		t.Fatal("actual pool return was not authenticated after rollback", snapshot)
	}
}

// The public legacy API stays idempotent; a second release cannot mint proof.
func TestDbPhysicalPoolReturnRejectsRepeatedRelease(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	conn, err := pool.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !conn.ReleaseWithIdleReturn() || conn.ReleaseWithIdleReturn() {
		t.Fatal("double release fabricated an actual physical return")
	}
	conn.Release()
}

// Puddle resets destroy old resources even while pgx's pool remains open.
// A reset generation remains conservatively unprovable, including new borrows.
func TestDbPhysicalPoolReturnRejectsResetGeneration(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	conn, err := pool.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pool.open().Reset()
	if conn.ReleaseWithIdleReturn() {
		t.Fatal("reset resource was falsely returned idle")
	}
	next, err := pool.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if next.ReleaseWithIdleReturn() {
		t.Fatal("reset generation regained unsupported physical proof")
	}
}

// The real Close parks waiting for this borrowed resource. Virtual quiescence
// orders its closed-channel publication before the actual release observation.
func TestDbPhysicalPoolReturnRejectsClosingPool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { pool.open().Close(); close(done) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("pool Close did not retain borrowed capacity")
		default:
		}
		if conn.ReleaseWithIdleReturn() {
			t.Fatal("closed pool fabricated idle reinsertion")
		}
		<-done
	})
}

// Async release hooks remain unknown until their independent owner returns;
// the synchronous caller cannot turn that pending callback into a receipt.
func TestDbPhysicalPoolReturnRejectsAsynchronousHook(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(_ *pgPoolWireFixture, config *pgxpool.Config) {
			config.AfterRelease = func(*pgx.Conn) bool { close(entered); <-release; return true }
		})
		t.Cleanup(unblock)
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if conn.ReleaseWithIdleReturn() {
			t.Fatal("pending hook fabricated physical return")
		}
		<-entered
		if pool.open().Stat().AcquiredConns() != 1 {
			t.Fatal("pending hook lost its actual capacity owner")
		}
		unblock()
		synctest.Wait()
		if pool.open().Stat().IdleConns() != 1 {
			t.Fatal("hook did not retain its existing release behavior")
		}
	})
}

// Eight seconds of owned deadline and four of the unchanged five-second
// statement grace still allow physical completion/rollback without cancellation.
func TestDbPhysicalPoolReturnPreservesHealthyStatementStopGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newStatementStopFixture(t, "UPDATE synthetic_row")
		defer func() {
			select {
			case fixture.reply <- false:
			default:
			}
		}()
		observation := NewDbTxObservation()
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		done := make(chan any, 1)
		go func() {
			done <- captureDbErrorPanic(func() {
				txWithPool(ctx, fixture.pool, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, "UPDATE synthetic_row SET value=1"))
				}, OptNoRetry(), observation)
			})
		}()
		statementCtx := <-fixture.tracer.contexts
		<-fixture.arrived
		time.Sleep(12 * time.Second)
		synctest.Wait()
		if ctx.Err() != context.DeadlineExceeded || statementCtx.Err() != nil {
			t.Fatal("healthy statement lost its full unchanged stop grace")
		}
		fixture.reply <- true
		cause, _ := (<-done).(error)
		snapshot := observation.Snapshot()
		if !errors.Is(cause, context.DeadlineExceeded) || snapshot.RollbackAcknowledged != 1 || snapshot.CommitAttempts != 0 ||
			!snapshot.LastRelease.PoolReturned || fixture.first.IsClosed() {
			t.Fatal("late healthy statement did not roll back and return the same pool resource", cause, snapshot)
		}
		fixture.requireRolledBackAndReused(t, "UPDATE synthetic_row")
	})
}

// Exactly the old stop bound still interrupts a stuck statement. No rollback
// acknowledgement or physical return can be guessed from its release entry.
func TestDbPhysicalPoolReturnRefusesInterruptedStatement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newStatementStopFixture(t, "UPDATE synthetic_row")
		defer func() {
			select {
			case fixture.reply <- false:
			default:
			}
		}()
		observation := NewDbTxObservation()
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		done := make(chan any, 1)
		go func() {
			done <- captureDbErrorPanic(func() {
				txWithPool(ctx, fixture.pool, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, "UPDATE synthetic_row SET value=1"))
				}, OptNoRetry(), observation)
			})
		}()
		statementCtx := <-fixture.tracer.contexts
		<-fixture.arrived
		time.Sleep(13 * time.Second)
		synctest.Wait()
		if statementCtx.Err() == nil {
			t.Fatal("stalled statement exceeded its unchanged stop bound")
		}
		fixture.reply <- false
		cause, _ := (<-done).(error)
		snapshot := observation.Snapshot()
		if !errors.Is(cause, context.DeadlineExceeded) || snapshot.RollbackAcknowledged != 0 || snapshot.LastRelease.PoolReturned ||
			snapshot.LastRelease.Disposition != DbTxReleaseDestroyTransferred {
			t.Fatal("interrupted statement gained rollback/physical return proof", cause, snapshot)
		}
	})
}

// Optional older observers and contradictory dispositions never mint proof.
func TestDbPhysicalPoolReturnRequiresExactReturnedReceipt(t *testing.T) {
	observation := NewDbTxObservation()
	observation.releaseEntered()
	observation.releaseReturned(DbTxReleaseIdle, nil)
	if observation.Snapshot().LastRelease.PoolReturned {
		t.Fatal("idle entry alone fabricated return")
	}
	for _, disposition := range []DbTxReleaseDisposition{DbTxReleaseUnknown, DbTxReleaseDestroyTransferred} {
		observation.releaseEntered()
		observation.releaseReturned(disposition, nil, true)
		if observation.Snapshot().LastRelease.PoolReturned {
			t.Fatal("contradictory transfer fabricated physical return")
		}
	}
}
