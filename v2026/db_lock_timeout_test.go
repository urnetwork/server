package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The lock bound rides the BEGIN round trip: pgx's own BEGIN of the options,
// or the caller's BeginQuery, then a transaction-scoped lock_timeout of at
// least one millisecond.
func TestBeginSqlWithLockTimeout(t *testing.T) {
	for _, c := range []struct {
		txOptions   pgx.TxOptions
		lockTimeout time.Duration
		want        string
	}{
		{
			txOptions:   pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadWrite, DeferrableMode: pgx.NotDeferrable},
			lockTimeout: 2 * time.Second,
			want:        "begin isolation level repeatable read read write not deferrable; set local lock_timeout = 2000",
		},
		{
			txOptions:   pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly},
			lockTimeout: 250 * time.Millisecond,
			want:        "begin isolation level read committed read only; set local lock_timeout = 250",
		},
		{
			txOptions:   pgx.TxOptions{},
			lockTimeout: time.Microsecond,
			want:        "begin; set local lock_timeout = 1",
		},
		{
			txOptions:   pgx.TxOptions{BeginQuery: "begin isolation level serializable"},
			lockTimeout: time.Second,
			want:        "begin isolation level serializable; set local lock_timeout = 1000",
		},
	} {
		if got := beginSqlWithLockTimeout(c.txOptions, c.lockTimeout); got != c.want {
			t.Errorf("%+v %s: begin = %q, want %q", c.txOptions, c.lockTimeout, got, c.want)
		}
	}
}

// A lock timeout is never rerun inside Tx: a rerun would queue the attempt
// again while holding a pooler server. The caller's own retry decides.
func TestLockTimeoutIsNotRerun(t *testing.T) {
	if class := pgRetryClassOf(&pgconn.PgError{Code: pgerrcode.LockNotAvailable}); class != pgRetryNever {
		t.Fatalf("lock timeout retry class = %d, want never", class)
	}
}

// Holds a transaction-scoped advisory lock on its own maintenance session for
// `hold`, then releases it, so a waiter without a bound succeeds late instead
// of waiting forever. Returns once the lock is held; done closes on release.
func holdTestAdvisoryLock(t testing.TB, ctx context.Context, key int64, hold time.Duration) (done chan struct{}) {
	t.Helper()
	done = make(chan struct{})
	held := make(chan struct{})
	go func() {
		defer close(done)
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key))
			close(held)
			time.Sleep(hold)
		}, OptReadWrite())
	}()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the test lock was not taken")
	}
	return done
}

// A main-pool transaction waiting on a lock another session holds fails with
// 55P03 at PgLockTimeout and returns its server, instead of waiting for the
// holder: the failure that let one hot lock take the whole pool.
func TestTxBoundsLockWaits(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := int64(NewId()[0])<<32 | int64(time.Now().UnixNano()&0xffffffff)
		done := holdTestAdvisoryLock(t, ctx, key, PgLockTimeout+4*time.Second)
		defer func() {
			<-done
		}()

		start := time.Now()
		var err error
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					err, _ = recovered.(error)
				}
			}()
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key))
			})
		}()
		elapsed := time.Since(start)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable {
			t.Fatalf("a lock wait past the bound ended with %v, want 55P03", err)
		}
		if elapsed < PgLockTimeout || PgLockTimeout+3*time.Second < elapsed {
			t.Fatalf("the lock wait ended after %s, want about %s and no rerun", elapsed, PgLockTimeout)
		}
	})
}

// A caller's TxLockTimeout replaces the bound, and a non-positive one leaves
// the wait to the server's default.
func TestTxLockTimeoutOption(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		showLockTimeout := func(options ...any) (setting string) {
			Tx(ctx, func(tx PgTx) {
				Raise(tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&setting))
			}, options...)
			return
		}
		if setting := showLockTimeout(); setting != "2s" {
			t.Fatalf("default transaction lock_timeout = %q, want 2s", setting)
		}
		if setting := showLockTimeout(TxLockTimeout(300 * time.Millisecond)); setting != "300ms" {
			t.Fatalf("lock_timeout with TxLockTimeout(300ms) = %q", setting)
		}
		if setting := showLockTimeout(TxLockTimeout(0)); setting != "0" {
			t.Fatalf("lock_timeout with TxLockTimeout(0) = %q, want the server default 0", setting)
		}
		if setting := showLockTimeout(TxReadCommitted, OptReadWrite()); setting != "2s" {
			t.Fatalf("read committed read-write lock_timeout = %q, want 2s", setting)
		}

		key := int64(NewId()[0])<<32 | int64(time.Now().UnixNano()&0xffffffff)
		done := holdTestAdvisoryLock(t, ctx, key, 4*time.Second)
		defer func() {
			<-done
		}()
		start := time.Now()
		var err error
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					err, _ = recovered.(error)
				}
			}()
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key))
			}, TxLockTimeout(300*time.Millisecond))
		}()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable || 2*time.Second < time.Since(start) {
			t.Fatalf("a 300ms bound ended after %s with %v, want 55P03 promptly", time.Since(start), err)
		}
	})
}

// The bound is the main pool's: a maintenance transaction keeps the server's
// unbounded default, and a bounded transaction's SET LOCAL never reaches the
// next use of its pooled connection.
func TestLockTimeoutStaysInItsTransaction(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var maintenance string
		MaintenanceTx(ctx, func(tx PgTx) {
			Raise(tx.QueryRow(ctx, `SHOW lock_timeout`).Scan(&maintenance))
		})
		if maintenance != "0" {
			t.Fatalf("maintenance transaction lock_timeout = %q, want 0", maintenance)
		}
		for range 4 {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `SELECT 1`))
			})
			var session string
			Db(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SHOW lock_timeout`).Scan(&session))
			})
			if session != "0" {
				t.Fatalf("a pooled session after a bounded transaction has lock_timeout %q, want 0", session)
			}
		}
	})
}
