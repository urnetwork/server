// A deadline or transport error from other work inside a transaction, such as
// a deadline-bound Redis admission or HTTP call, ends the callback after the
// detached rollback has already returned its session to idle. These tests
// drive real pgx pool connections on the owned in-memory wire fixture and
// require that such a session stays pooled instead of forcing a reconnect.
package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Errors a transaction callback raises from work other than its own
// PostgreSQL session: the bounded deadline of a Redis admission, and the
// socket read timeout go-redis and HTTP clients report.
func otherWorkStopErrors() []error {
	return []error{
		fmt.Errorf("synthetic deadline-bound admission: %w", context.DeadlineExceeded),
		&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded},
	}
}

// A live caller's transaction fails on other work after a written statement.
// Its exact error surfaces without a replay, and the rolled-back session is
// reused rather than destroyed.
func TestTxDeadlineFromOtherWorkKeepsPooledConnection(t *testing.T) {
	for _, otherErr := range otherWorkStopErrors() {
		fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		var first *pgconn.PgConn
		callbacks := 0
		recovered := captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(tx PgTx) {
				callbacks++
				first = tx.Conn().PgConn()
				RaisePgResult(tx.Exec(ctx, "SELECT 1"))
				panic(otherErr)
			}, TxReadCommitted)
		})
		if recovered != otherErr || callbacks != 1 {
			t.Fatal("transaction lost its exact cause or replayed written work", otherErr, recovered, callbacks)
		}
		if first.IsClosed() {
			t.Fatal("other work's deadline destroyed a rolled-back session", otherErr)
		}
		dbWithPool(ctx, pool, func(conn PgConn) {
			if conn.Conn().PgConn() != first {
				t.Fatal("rolled-back session was not reused", otherErr)
			}
			RaisePgResult(conn.Exec(ctx, "SELECT 1"))
		}, OptNoRetry())
		cancel()
		fixture.stateLock.Lock()
		dials := fixture.dialCount
		queries := append([]string(nil), fixture.queries...)
		fixture.stateLock.Unlock()
		if dials != 1 || len(queries) != 5 || !strings.HasPrefix(queries[1], "begin") || queries[3] != "rollback" {
			t.Fatal("transaction did not roll back and reuse its only session", otherErr, dials, queries)
		}
	}
}

// A server-reported connection exception still disposes of a transaction's
// session, even though pgx keeps the connection open after a non-fatal error.
func TestTxServerConnectionExceptionDiscardsSession(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	exception := &pgconn.PgError{Code: "08006", Message: "synthetic connection failure"}
	var first *pgconn.PgConn
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(tx PgTx) {
			first = tx.Conn().PgConn()
			RaisePgResult(tx.Exec(ctx, "SELECT 1"))
			panic(exception)
		}, TxReadCommitted, OptNoRetry())
	})
	if recovered != exception || !first.IsClosed() {
		t.Fatal("server-reported connection failure kept its session", recovered)
	}
	dbWithPool(ctx, pool, func(conn PgConn) {
		if conn.Conn().PgConn() == first {
			t.Fatal("session with a connection exception was reused")
		}
	}, OptNoRetry())
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if fixture.dialCount != 2 {
		t.Fatal("replacement session was not constructed", fixture.dialCount)
	}
}
