// Pins callback replay separately from connection health-check admission.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A socket can still fail after a successful health check. Once the peer has
// received an application statement, losing its reply must not replay it.
func TestDbHotAcquireDoesNotReplayLostQueryReply(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(connectionIndex int, query string) bool {
		return query == "-- ping"
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	callbacks := 0
	var firstErr error
	recovered := captureDbErrorPanic(func() {
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			callbacks += 1
			if 1 < callbacks {
				panic(errors.New("synthetic application callback was replayed"))
			}
			_, firstErr = conn.Exec(context.Background(), "INSERT INTO synthetic_effect VALUES (1)")
			Raise(firstErr)
		})
	})
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if recovered == nil || callbacks != 1 || len(fixture.queries) != 2 {
		t.Fatalf("lost reply error=%T %v safe_to_retry=%t callbacks=%d queries=%d, want error,1,2", firstErr, firstErr, pgconn.SafeToRetry(firstErr), callbacks, len(fixture.queries))
	}
}

// A safe failure of the final operation does not prove an entire callback is
// safe: a previous statement may already have committed an application effect.
func TestDbDoesNotReplayEarlierStatementAfterSafeLastFailure(t *testing.T) {
	writeErr := testCanceledPgprotoWriteError(t)
	fixture, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			callbacks += 1
			if callbacks != 1 {
				panic(errors.New("synthetic earlier statement was replayed"))
			}
			RaisePgResult(conn.Exec(context.Background(), "INSERT INTO synthetic_effect VALUES (1)"))
			panic(writeErr)
		})
	})
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if recovered != writeErr || callbacks != 1 || len(fixture.queries) != 2 {
		t.Fatalf("earlier statement failure=%v callbacks=%d queries=%d, want original,1,2", recovered, callbacks, len(fixture.queries))
	}
}

// Injects a zero-byte socket failure at an explicit callback-owned boundary.
// Closing the peer also prevents cleanup from sending unrelated protocol bytes.
type pgPreSendFailureConn struct {
	net.Conn
	failNext *atomic.Bool
}

// Delegates all writes except the one explicitly armed by the test callback.
func (self *pgPreSendFailureConn) Write(p []byte) (int, error) {
	if self.failNext.Swap(false) {
		_ = self.Conn.Close()
		return 0, &net.OpError{Op: "write", Net: "pipe", Err: os.ErrDeadlineExceeded}
	}
	return self.Conn.Write(p)
}

// A genuinely unsent first statement remains eligible for one fresh-socket
// attempt; the application statement reaches PostgreSQL exactly once.
func TestDbRetriesProvenPreSendFailure(t *testing.T) {
	var failNext atomic.Bool
	fixture, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network string, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &pgPreSendFailureConn{Conn: conn, failNext: &failNext}, nil
		}
	})
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			callbacks += 1
			if callbacks == 1 {
				failNext.Store(true)
			}
			RaisePgResult(conn.Exec(context.Background(), "INSERT INTO synthetic_effect VALUES (1)"))
		})
	})
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if recovered != nil || callbacks != 2 || fixture.dialCount != 2 || len(fixture.queries) != 3 {
		t.Fatalf("pre-send failure=%v callbacks=%d dials=%d queries=%d, want nil,2,2,3", recovered, callbacks, fixture.dialCount, len(fixture.queries))
	}
}

// A transaction body may have application effects outside its SQL transaction.
// An uncertain body reply must not silently replay the application callback.
func TestTxDoesNotReplayLostBodyReply(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(connectionIndex int, query string) bool {
		return query != "INSERT INTO synthetic_effect VALUES (1)"
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(context.Background(), pool, func(tx PgTx) {
			callbacks += 1
			if callbacks != 1 {
				panic(errors.New("synthetic transaction body was replayed"))
			}
			RaisePgResult(tx.Exec(context.Background(), "INSERT INTO synthetic_effect VALUES (1)"))
		})
	})
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if recovered == nil || callbacks != 1 || fixture.dialCount != 1 {
		t.Fatalf("lost body failure=%v callbacks=%d dials=%d, want error,1,1", recovered, callbacks, fixture.dialCount)
	}
}

// PostgreSQL can commit and lose only its response. The detached commit budget
// limits waiting but cannot turn an unknown result into permission to replay.
func TestTxDoesNotReplayLostCommitReply(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(connectionIndex int, query string) bool {
		return query != "commit"
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(context.Background(), pool, func(tx PgTx) {
			callbacks += 1
			if callbacks != 1 {
				panic(errors.New("synthetic committed callback was replayed"))
			}
			RaisePgResult(tx.Exec(context.Background(), "INSERT INTO synthetic_effect VALUES (1)"))
		})
	})
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if recovered == nil || callbacks != 1 || fixture.dialCount != 1 {
		t.Fatalf("lost commit failure=%v callbacks=%d dials=%d, want error,1,1", recovered, callbacks, fixture.dialCount)
	}
}

// A server-confirmed serialization rollback is not ambiguous and retains the
// established bounded whole-transaction retry.
func TestTxRetriesServerConfirmedCommitRollback(t *testing.T) {
	commits := 0
	fixture, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
			if query == "commit" {
				commits += 1
				if commits == 1 {
					return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40001", Message: "synthetic serialization rollback"}
				}
			}
			return nil
		}
	})
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(context.Background(), pool, func(tx PgTx) {
			callbacks += 1
			RaisePgResult(tx.Exec(context.Background(), "INSERT INTO synthetic_effect VALUES (1)"))
		})
	})
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if recovered != nil || callbacks != 2 || commits != 2 || fixture.dialCount != 1 {
		t.Fatalf("confirmed rollback=%v callbacks=%d commits=%d dials=%d, want nil,2,2,1", recovered, callbacks, commits, fixture.dialCount)
	}
}

// Class 40 also includes statement_completion_unknown; the class name alone
// is not evidence that a statement or commit was rolled back.
func TestDbDoesNotRetryStatementCompletionUnknown(t *testing.T) {
	if isTransientError(&pgconn.PgError{Code: "40003"}) {
		t.Fatal("unknown statement completion was classified as a safe rollback retry")
	}
}

// Even an explicit PostgreSQL response cannot authorize replay when that
// response says the statement's completion itself is unknown.
func TestTxDoesNotReplayCompletionUnknownCommit(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		fixture.queryError = func(connectionIndex int, query string) *pgproto3.ErrorResponse {
			if query == "commit" {
				return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "40003", Message: "synthetic completion unknown"}
			}
			return nil
		}
	})
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(context.Background(), pool, func(tx PgTx) {
			callbacks += 1
			if callbacks != 1 {
				panic(errors.New("synthetic unknown commit was replayed"))
			}
		})
	})
	var pgErr *pgconn.PgError
	err, ok := recovered.(error)
	if !ok || !errors.As(err, &pgErr) || pgErr.Code != "40003" || callbacks != 1 {
		t.Fatalf("unknown commit=%v callbacks=%d, want original PostgreSQL error and1", recovered, callbacks)
	}
}

// Pins the narrow commit policy independently of socket-error wrapping. A
// commit postgres turned into a rollback is replayed only for a transient
// recorded statement error.
func TestTxCommitRetryRequiresKnownRollback(t *testing.T) {
	for _, testCase := range []struct {
		err   error
		retry bool
	}{
		{err: &pgconn.PgError{Code: "40001"}, retry: true},
		{err: &pgconn.PgError{Code: "40P01"}, retry: true},
		{err: &pgconn.PgError{Code: "23505"}, retry: true},
		{err: pgx.ErrTxCommitRollback},
		{err: fmt.Errorf("synthetic commit: %w", pgx.ErrTxCommitRollback)},
		{err: &txAbortedError{statementErr: &pgconn.PgError{Code: "40001"}, err: pgx.ErrTxCommitRollback}, retry: true},
		{err: &txAbortedError{statementErr: &pgconn.PgError{Code: "23505"}, err: pgx.ErrTxCommitRollback}, retry: true},
		{err: &txAbortedError{statementErr: &pgconn.PgError{Code: "23502"}, err: pgx.ErrTxCommitRollback}},
		{err: &txAbortedError{err: pgx.ErrTxCommitRollback}},
		{err: &pgconn.PgError{Code: "23514"}},
		{err: &pgconn.PgError{Code: "40003"}},
		{err: &pgconn.PgError{Code: "08007"}},
		{err: &pgconn.PgError{Code: "42601"}},
		{err: pgconn.ErrConnClosed},
		{err: context.DeadlineExceeded},
		{err: io.EOF},
	} {
		if retry := canRetryCommitError(testCase.err); retry != testCase.retry {
			t.Errorf("commit error %v retry=%t, want %t", testCase.err, retry, testCase.retry)
		}
	}
}

// A minimal transport reports an exact partial-write boundary without network
// timing. Only Write is used; the embedded methods are deliberately unused.
type pgCountedTestConn struct {
	net.Conn
	byteCount int
	err       error
}

// Reports the synthetic byte count exactly as a real net.Conn write would.
func (self *pgCountedTestConn) Write(p []byte) (int, error) {
	return min(self.byteCount, len(p)), self.err
}

// Zero-byte, partial, and complete writes have distinct proof boundaries; the
// same counter is discoverable beneath TLS without wrapping the TLS object.
func TestPgWriteSnapshotTracksPartialWritesAndTls(t *testing.T) {
	for _, byteCount := range []int{0, 1, 4} {
		conn := &pgWriteTrackedConn{Conn: &pgCountedTestConn{byteCount: byteCount, err: io.ErrUnexpectedEOF}}
		plainSnapshot := snapshotPgWrites(conn)
		tlsConn := tls.Client(conn, &tls.Config{ServerName: "synthetic-pg.example", MinVersion: tls.VersionTLS12})
		tlsSnapshot := snapshotPgWrites(tlsConn)
		if !plainSnapshot.unchanged() || !tlsSnapshot.unchanged() {
			t.Fatal("new tracked transport was not recognized before its first write")
		}
		_, _ = conn.Write([]byte("test"))
		if plainSnapshot.unchanged() != (byteCount == 0) || tlsSnapshot.unchanged() != (byteCount == 0) {
			t.Errorf("write proof failed for %d accepted bytes", byteCount)
		}
	}
}

// An uninstrumented or custom-wrapped transport supplies no zero-write proof.
func TestPgWriteSnapshotUnknownTransportFailsClosed(t *testing.T) {
	if snapshotPgWrites(&pgCountedTestConn{}).unchanged() || (pgWriteSnapshot{}).unchanged() {
		t.Fatal("unknown transport authorized callback replay without write evidence")
	}
}
