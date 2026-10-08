package server

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A barrier arms this transport only after a completed preliminary read.
// Every subsequent write fails before a byte reaches the synthetic backend,
// including asynchronous cleanup, so the protocol boundary is deterministic.
type txPreludeZeroWriteConn struct {
	net.Conn
	failWrites atomic.Bool
}

func (self *txPreludeZeroWriteConn) Write(p []byte) (int, error) {
	if self.failWrites.Load() {
		return 0, &net.OpError{Op: "write", Net: "synthetic", Err: os.ErrDeadlineExceeded}
	}
	return self.Conn.Write(p)
}

func txPreludeWirePool(t testing.TB) (*pgPoolWireFixture, *safePgPool, *atomic.Pointer[txPreludeZeroWriteConn]) {
	t.Helper()
	first := &atomic.Pointer[txPreludeZeroWriteConn]{}
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(_ *pgPoolWireFixture, config *pgxpool.Config) {
			dial := config.ConnConfig.DialFunc
			config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err != nil {
					return conn, err
				}
				wrapped := &txPreludeZeroWriteConn{Conn: conn}
				first.CompareAndSwap(nil, wrapped)
				return wrapped, nil
			}
		})
	return fixture, pool, first
}

// A completed read must not count as an ambiguous transaction write. Refuse
// the first BEGIN before its first byte and require a new read/connection.
func TestTxReadBeforeBeginRetriesSafeBeginFailure(t *testing.T) {
	fixture, pool, first := txPreludeWirePool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reads, callbacks := 0, 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(tx PgTx) {
			callbacks++
			RaisePgResult(tx.Exec(ctx, "INSERT INTO synthetic_child VALUES (1)"))
		}, TxReadBeforeBegin(func(conn PgCanQuery) {
			reads++
			result, err := conn.Query(ctx, "SELECT synthetic_entitlement")
			WithPgResult(result, err, func() {})
			if reads == 1 {
				first.Load().failWrites.Store(true)
			}
		}))
	})
	if recovered != nil || reads != 2 || callbacks != 1 {
		t.Fatalf("safe BEGIN retry: error=%T reads=%d callbacks=%d; want nil,2,1", recovered, reads, callbacks)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	readQueries, writes, begins, commits := 0, 0, 0, 0
	for _, query := range fixture.queries {
		switch {
		case query == "SELECT synthetic_entitlement":
			readQueries++
		case strings.HasPrefix(query, "INSERT INTO synthetic_child"):
			writes++
		case strings.HasPrefix(query, "begin"):
			begins++
		case query == "commit":
			commits++
		}
	}
	if fixture.dialCount != 2 || readQueries != 2 || writes != 1 || begins != 1 || commits != 1 {
		t.Fatalf("wire replay boundary: dials=%d reads=%d writes=%d begins=%d commits=%d", fixture.dialCount, readQueries, writes, begins, commits)
	}
}

// The preliminary-read exception ends before BEGIN. Even a later zero-byte
// failure cannot replay a callback that already sent its transaction writes.
func TestTxReadBeforeBeginPreservesPostWriteNoReplay(t *testing.T) {
	fixture, pool, first := txPreludeWirePool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reads, callbacks := 0, 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(tx PgTx) {
			callbacks++
			RaisePgResult(tx.Exec(ctx, "INSERT INTO synthetic_child VALUES (1)"))
			first.Load().failWrites.Store(true)
			RaisePgResult(tx.Exec(ctx, "SELECT synthetic_after_write"))
		}, TxReadBeforeBegin(func(conn PgCanQuery) {
			reads++
			result, err := conn.Query(ctx, "SELECT synthetic_entitlement")
			WithPgResult(result, err, func() {})
		}))
	})
	if recovered == nil || reads != 1 || callbacks != 1 {
		t.Fatalf("ambiguous write boundary: error=%T reads=%d callbacks=%d; want error,1,1", recovered, reads, callbacks)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	writes, commits := 0, 0
	for _, query := range fixture.queries {
		if strings.HasPrefix(query, "INSERT INTO synthetic_child") {
			writes++
		}
		if query == "commit" {
			commits++
		}
	}
	if fixture.dialCount != 1 || writes != 1 || commits != 0 {
		t.Fatalf("ambiguous write replayed: dials=%d writes=%d commits=%d", fixture.dialCount, writes, commits)
	}
}
