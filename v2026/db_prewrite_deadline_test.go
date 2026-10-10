// A stopped caller and a failed transport have different disposal authority.
// Drive real pgx pre-write errors on the existing owned in-memory wire fixture.
package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A real cancellation channel supplies an explicit expiration barrier; only
// its error is deadline-class. No timer or scheduler race owns the test proof.
type prewriteDeadlineContext struct {
	context.Context
}

func (self prewriteDeadlineContext) Err() error {
	if self.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

// Expiration after acquisition but before the first query must preserve the
// exact error and never replay, while retaining the untouched healthy socket.
func TestDbPrewriteDeadlineReusesHealthyConnection(t *testing.T) {
	for _, options := range [][]any{nil, {OptNoRetry()}} {
		fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
		base, expire := context.WithCancel(t.Context())
		defer expire()
		ctx := prewriteDeadlineContext{Context: base}
		var first *pgconn.PgConn
		var original error
		calls := 0
		failure := captureDbErrorPanic(func() {
			dbWithPool(ctx, pool, func(conn PgConn) {
				calls++
				first = conn.Conn().PgConn()
				before := snapshotPgWrites(first.Conn())
				expire()
				_, original = conn.Exec(ctx, "SELECT 991")
				if original == nil || !pgconn.SafeToRetry(original) || !before.unchanged() || first.IsClosed() || first.IsBusy() || first.TxStatus() != 'I' {
					t.Fatal("fixture did not reach pgx's healthy pre-write deadline refusal")
				}
				panic(original)
			}, options...)
		})
		err, _ := failure.(error)
		if !errors.Is(err, original) || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, DbContextDoneError) || calls != 1 {
			t.Fatal("pre-write deadline lost its exact outcome or replayed the callback", err, calls)
		}
		stat := pool.open().Stat()
		if first.IsClosed() || stat.IdleConns() != 1 || stat.AcquiredConns() != 0 || stat.ConstructingConns() != 0 || stat.NewConnsCount() != 1 {
			t.Fatal("pre-write caller deadline destroyed a healthy idle connection")
		}
		select {
		case <-first.CleanupDone():
			t.Fatal("pre-write caller deadline began physical disposal")
		default:
		}
		recovery, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		dbWithPool(recovery, pool, func(conn PgConn) {
			if conn.Conn().PgConn() != first {
				t.Fatal("later healthy caller did not reuse the original connection")
			}
			RaisePgResult(conn.Exec(recovery, "SELECT 1"))
		}, OptNoRetry())
		cancel()
		fixture.stateLock.Lock()
		pings, dials := fixture.pingCount, fixture.dialCount
		queries := append([]string(nil), fixture.queries...)
		fixture.stateLock.Unlock()
		if pings != 1 || dials != 1 || len(queries) != 2 || queries[0] != "-- ping" || queries[1] != "SELECT 1" {
			t.Fatal("deadline wrote SQL or manufactured another startup validation", pings, dials, queries)
		}
		assertPgPoolWrapperLifecycleMetrics(t, pool, 0, 0, 0)
	}
}

// No-data on the final statement cannot erase earlier writes in this checkout;
// neither a transport write error nor a bare application deadline proves reuse.
func TestDbPrewriteDeadlineKeepsUncertainConnectionsDiscarded(t *testing.T) {
	for _, boundary := range []string{"earlier_write", "socket_timeout", "bare_deadline"} {
		_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
		base, expire := context.WithCancel(t.Context())
		defer expire()
		ctx := prewriteDeadlineContext{Context: base}
		var first *pgconn.PgConn
		var original error
		calls := 0
		failure := captureDbErrorPanic(func() {
			dbWithPool(ctx, pool, func(conn PgConn) {
				calls++
				first = conn.Conn().PgConn()
				if boundary == "earlier_write" {
					RaisePgResult(conn.Exec(ctx, "SELECT 1"))
				}
				expire()
				switch boundary {
				case "earlier_write":
					_, original = conn.Exec(ctx, "SELECT 991")
				case "socket_timeout":
					original = testCanceledPgprotoWriteError(t)
				case "bare_deadline":
					original = context.DeadlineExceeded
				}
				panic(original)
			}, OptNoRetry())
		})
		err, _ := failure.(error)
		if !errors.Is(err, original) || !errors.Is(err, context.DeadlineExceeded) || calls != 1 || !first.IsClosed() {
			t.Fatal("uncertain checkout lost cause, replayed or escaped disposal", boundary, err, calls)
		}
		recovery, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		dbWithPool(recovery, pool, func(conn PgConn) {
			if conn.Conn().PgConn() == first {
				t.Fatal("uncertain connection was reused", boundary)
			}
			RaisePgResult(conn.Exec(recovery, "SELECT 1"))
		}, OptNoRetry())
		cancel()
		select {
		case <-first.CleanupDone():
		default:
			t.Fatal("replacement preceded original physical cleanup", boundary)
		}
	}
}
