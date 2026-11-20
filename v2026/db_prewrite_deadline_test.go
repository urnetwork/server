// A stopped caller and a failed transport have different disposal authority:
// pgx's own connection state decides disposal, not the error that stopped the
// callback. Drive real pgx errors on the existing owned in-memory wire fixture.
package server

import (
	"context"
	"errors"
	"net"
	"sync"
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

// A connection pgx still reports idle and in sync stays pooled whatever error
// stopped its callback: a refusal after earlier statements were written, a
// transport error raised from another socket, or a bare deadline. pgx closes
// a connection whenever it interrupts an exchange, so none of these is
// evidence against this one. The exact cause still surfaces without a replay.
func TestDbStopAfterHealthyExchangeReusesConnection(t *testing.T) {
	for _, boundary := range []string{"earlier_write", "socket_timeout", "bare_deadline"} {
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
		if !errors.Is(err, original) || !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
			t.Fatal("stopped callback lost its cause or replayed", boundary, err, calls)
		}
		if first.IsClosed() {
			t.Fatal("a stop with no evidence against the session destroyed it", boundary)
		}
		recovery, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		dbWithPool(recovery, pool, func(conn PgConn) {
			if conn.Conn().PgConn() != first {
				t.Fatal("healthy connection was not reused", boundary)
			}
			RaisePgResult(conn.Exec(recovery, "SELECT 1"))
		}, OptNoRetry())
		cancel()
		fixture.stateLock.Lock()
		dials := fixture.dialCount
		fixture.stateLock.Unlock()
		if dials != 1 {
			t.Fatal("a stopped callback forced another connection startup", boundary, dials)
		}
	}
}

// pgx's own interruption of a statement in flight remains the evidence that
// disposes of a session, and its replacement still waits for that cleanup.
func TestDbInterruptedExchangeDiscardsConnection(t *testing.T) {
	cancelDialed := make(chan struct{})
	var cancelOnce sync.Once
	held := make(chan struct{})
	var heldOnce sync.Once
	drop := make(chan struct{})
	_, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if query == "SELECT 991" {
			heldOnce.Do(func() { close(held) })
			<-drop
			return false
		}
		return true
	}, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network string, address string) (net.Conn, error) {
			// pgx dials its cancel request over the fixture's pipe network
			// only after it has already interrupted the exchange.
			if network == "pipe" {
				cancelOnce.Do(func() { close(cancelDialed) })
			}
			return dial(ctx, network, address)
		}
	})
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var first *pgconn.PgConn
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			dbWithPool(ctx, pool, func(conn PgConn) {
				first = conn.Conn().PgConn()
				RaisePgResult(conn.Exec(ctx, "SELECT 991"))
			}, OptNoRetry())
		})
	}()
	<-held
	stop()
	<-cancelDialed
	close(drop)
	recovered := <-done
	if err, _ := recovered.(error); !errors.Is(err, context.Canceled) || !first.IsClosed() {
		t.Fatal("interrupted exchange lost its cause or kept its socket", recovered)
	}
	recovery, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	dbWithPool(recovery, pool, func(conn PgConn) {
		if conn.Conn().PgConn() == first {
			t.Fatal("interrupted connection was reused")
		}
		select {
		case <-first.CleanupDone():
		default:
			t.Fatal("replacement preceded the interrupted connection's cleanup")
		}
	}, OptNoRetry())
}
