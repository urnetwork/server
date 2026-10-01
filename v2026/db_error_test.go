package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Produces the same unexported pgproto3.writeError returned when a PostgreSQL
// frontend cannot flush a query. Callers choose the writer's exact boundary.
func testPgprotoWriteError(t testing.TB, writer io.Writer) error {
	t.Helper()
	frontend := pgproto3.NewFrontend(bytes.NewReader(nil), writer)
	frontend.SendQuery(&pgproto3.Query{String: "SELECT 1"})
	err := frontend.Flush()
	if err == nil {
		t.Fatal("pgproto3 frontend unexpectedly flushed the synthetic query")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("pgproto3 write error = %T %v; want wrapped network timeout", err, err)
	}
	return err
}

// Mirrors pgx's deadline context watcher with an explicit cancellation
// barrier, then returns the protocol-layer write error caused by that deadline.
func testCanceledPgprotoWriteError(t testing.TB) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	localConn, peerConn := net.Pipe()
	defer localConn.Close()
	defer peerConn.Close()

	deadlineResult := make(chan error, 1)
	go func() {
		<-ctx.Done()
		deadlineResult <- localConn.SetWriteDeadline(time.Unix(1, 0))
	}()
	cancel()
	if err := <-deadlineResult; err != nil {
		t.Fatalf("set canceled PostgreSQL write deadline: %v", err)
	}
	return testPgprotoWriteError(t, localConn)
}

// Forces a protocol write to report that one byte may have reached PostgreSQL.
// pgconn.SafeToRetry must reject this uncertain execution boundary.
type partialPgWriteTimeout struct{}

func (self *partialPgWriteTimeout) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 1, &net.OpError{
		Op:  "write",
		Net: "tcp",
		Err: os.ErrDeadlineExceeded,
	}
}

// Captures the database boundary's panic without invoking the production
// logger, so negative controls do not manufacture scorer stability findings.
func captureDbErrorPanic(callback func()) (recovered any) {
	defer func() {
		recovered = recover()
	}()
	callback()
	return
}

// Creates a private one-connection pool so a retry can prove whether the
// connection that raised a protocol failure was discarded before reacquire.
func newSingleConnectionDbTestPool(t testing.TB) *safePgPool {
	t.Helper()
	config := safePool.open().Config().Copy()
	config.MinConns = 0
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	return &safePgPool{pool: pool}
}

// Wrapped PostgreSQL transaction failures retain their SQLSTATE classification.
// Database adapters commonly add context before errors reach the retry loop.
func TestDbClassifiesWrappedTransientError(t *testing.T) {
	err := fmt.Errorf("transaction callback: %w", &pgconn.PgError{Code: "40001"})
	if !isTransientError(err) {
		t.Fatal("wrapped serialization failure was not classified as transient")
	}
}

// A real pgproto3 write failure unwraps to net.OpError. Recognizing the wrapped
// socket error keeps expected cancellation on the database done path.
func TestDbClassifiesWrappedConnectionWriteTimeout(t *testing.T) {
	err := testCanceledPgprotoWriteError(t)
	if !isConnectionError(err) {
		t.Fatalf("pgproto3 TCP write timeout was not classified as a connection error: %T %v", err, err)
	}
	if !pgconn.SafeToRetry(err) {
		t.Fatal("zero-byte pgproto3 write timeout was not safe to retry")
	}
	if !canRetryConnectionError(err) {
		t.Fatal("safe pgproto3 write timeout was not eligible for connection retry")
	}
}

// Existing connection retries that carry no explicit unsafe-write evidence
// remain compatible. This includes SQLSTATE failures and legacy socket errors.
func TestDbPreservesLegacyConnectionRetryClasses(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
	}{
		{
			name: "SQLSTATE connection exception",
			err:  &pgconn.PgError{Code: "08006"},
		},
		{
			name: "legacy network timeout",
			err: &net.OpError{
				Op:  "read",
				Net: "tcp",
				Err: os.ErrDeadlineExceeded,
			},
		},
		{
			name: "closed connection sentinel",
			err:  pgconn.ErrConnClosed,
		},
	} {
		if !isConnectionError(testCase.err) {
			t.Errorf("%s was not classified as a connection error", testCase.name)
		}
		if !canRetryConnectionError(testCase.err) {
			t.Errorf("%s lost its established retry behavior", testCase.name)
		}
	}
}

// An explicit partial-write marker is a connection-disposal signal, not
// permission to replay the callback whose server outcome is uncertain.
func TestDbRejectsUnsafePgprotoWriteRetry(t *testing.T) {
	writeErr := testPgprotoWriteError(t, &partialPgWriteTimeout{})
	if pgconn.SafeToRetry(writeErr) {
		t.Fatal("partial pgproto3 write was marked safe to retry")
	}
	for _, testCase := range []struct {
		name string
		err  error
	}{
		{name: "direct", err: writeErr},
		{name: "wrapped", err: fmt.Errorf("query callback: %w", writeErr)},
	} {
		if canRetryConnectionError(testCase.err) {
			t.Errorf("%s partial pgproto3 write was eligible for callback replay", testCase.name)
		}
	}
}

// The complete database loop must convert a canceled pgproto3 write timeout to
// Done without replaying the callback under either retry policy.
func TestDbCanceledWriteTimeoutUsesDonePathWithoutReplay(t *testing.T) {
	writeErr := testCanceledPgprotoWriteError(t)
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		for _, testCase := range []struct {
			name    string
			options []any
		}{
			{name: "default retry"},
			{name: "no retry", options: []any{OptNoRetry()}},
		} {
			ctx, cancel := context.WithCancel(context.Background())
			callbackCount := 0
			recovered := captureDbErrorPanic(func() {
				Db(ctx, func(conn PgConn) {
					callbackCount += 1
					cancel()
					WithPgResult(nil, writeErr, func() {})
				}, testCase.options...)
			})
			if recovered != DbContextDoneError {
				t.Errorf("%s: canceled write recovered %T %v; want DbContextDoneError", testCase.name, recovered, recovered)
			}
			if callbackCount != 1 {
				t.Errorf("%s: canceled database callback count = %d; want 1", testCase.name, callbackCount)
			}
		}
	})
}

// A live zero-byte protocol failure is safe to replay on a fresh connection.
// This retains the established bounded connection retry contract.
func TestDbRetriesSafePgprotoWriteFailure(t *testing.T) {
	writeErr := testCanceledPgprotoWriteError(t)
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		callbackCount := 0
		Db(context.Background(), func(conn PgConn) {
			callbackCount += 1
			if callbackCount == 1 {
				WithPgResult(nil, writeErr, func() {})
			}
		})
		if callbackCount != 2 {
			t.Fatalf("safe database callback count = %d; want 2", callbackCount)
		}
	})
}

// Disabling retries must keep a live protocol timeout visible to its caller;
// only a timeout paired with a done context becomes expected shutdown.
func TestDbLiveWriteTimeoutWithNoRetryRemainsVisible(t *testing.T) {
	writeErr := testCanceledPgprotoWriteError(t)
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		callbackCount := 0
		recovered := captureDbErrorPanic(func() {
			Db(context.Background(), func(conn PgConn) {
				callbackCount += 1
				WithPgResult(nil, writeErr, func() {})
			}, OptNoRetry())
		})
		if recovered != writeErr {
			t.Fatalf("live write timeout recovered %T %v; want original %T %v", recovered, recovered, writeErr, writeErr)
		}
		if callbackCount != 1 {
			t.Fatalf("live no-retry database callback count = %d; want 1", callbackCount)
		}
	})
}

// Classification must happen before cleanup. Otherwise the panic defer
// releases the failed connection while connErr is still nil and the retry
// reacquires the same PostgreSQL session.
func TestDbDiscardsClassifiedConnectionBeforeRetry(t *testing.T) {
	writeErr := testCanceledPgprotoWriteError(t)
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		pool := newSingleConnectionDbTestPool(t)
		defer pool.close()
		connectionPids := []uint32{}
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			connectionPids = append(connectionPids, conn.Conn().PgConn().PID())
			if len(connectionPids) == 1 {
				WithPgResult(nil, writeErr, func() {})
			}
		})
		if len(connectionPids) != 2 {
			t.Fatalf("database callback count = %d; want 2", len(connectionPids))
		}
		if connectionPids[0] == connectionPids[1] {
			t.Fatalf("classified connection pid %d was released and reacquired instead of discarded", connectionPids[0])
		}
	})
}

// A partial protocol write has an uncertain server outcome. A live caller must
// receive that error without replaying a potentially non-idempotent callback.
func TestDbDoesNotRetryUnsafePgprotoWriteFailure(t *testing.T) {
	writeErr := fmt.Errorf(
		"query callback: %w",
		testPgprotoWriteError(t, &partialPgWriteTimeout{}),
	)
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		callbackCount := 0
		recovered := captureDbErrorPanic(func() {
			Db(context.Background(), func(conn PgConn) {
				callbackCount += 1
				if callbackCount == 1 {
					WithPgResult(nil, writeErr, func() {})
				}
			})
		})
		if recovered != writeErr {
			t.Fatalf("partial write recovered %T %v; want original %T %v", recovered, recovered, writeErr, writeErr)
		}
		if callbackCount != 1 {
			t.Fatalf("unsafe database callback count = %d; want 1", callbackCount)
		}
	})
}

// Cancellation cannot turn an unrelated application failure into expected
// shutdown. It must remain available to HandleError's unexpected path.
func TestDbCanceledContextPreservesOrdinaryFailure(t *testing.T) {
	ordinaryErr := errors.New("synthetic callback invariant failure")
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		recovered := captureDbErrorPanic(func() {
			Db(ctx, func(conn PgConn) {
				cancel()
				panic(ordinaryErr)
			}, OptNoRetry())
		})
		if recovered != ordinaryErr {
			t.Fatalf("ordinary failure recovered %T %v; want original error", recovered, recovered)
		}
		if IsDoneError(recovered) {
			t.Fatal("ordinary failure was classified as expected shutdown")
		}
	})
}

// Pgx wraps a closed-connection sentinel when statement-cache cleanup fails.
// The wrapper must still take dbWithPool's bounded fresh-connection retry.
func TestDbClassifiesWrappedClosedConnection(t *testing.T) {
	err := fmt.Errorf("failed to deallocate cached statement(s): %w", pgconn.ErrConnClosed)
	if !isConnectionError(err) {
		t.Fatal("wrapped closed connection was not classified as a connection error")
	}
}

// SQL errors outside the connection-exception class must stay on the normal
// failure path even when an adapter wraps them with additional context.
func TestDbRejectsWrappedNonConnectionError(t *testing.T) {
	err := fmt.Errorf("query callback: %w", &pgconn.PgError{Code: "42P01"})
	if isConnectionError(err) {
		t.Fatal("undefined-table error was classified as a connection failure")
	}
}

// Unrelated wrappers must not become retryable merely because they implement
// standard error unwrapping.
func TestDbRejectsWrappedOrdinaryError(t *testing.T) {
	err := fmt.Errorf("query callback: %w", errors.New("invalid query result"))
	if isConnectionError(err) || isTransientError(err) {
		t.Fatal("ordinary wrapped error was classified as retryable")
	}
}
