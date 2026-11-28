// A caller that stops around BEGIN must not destroy its pooled connection.
// Under transaction pooling, a client that disconnects after its BEGIN reached
// a server forces the pooler to destroy that server, and pgx's cancel request
// first holds it idle in transaction. These tests drive real pgx pool
// connections on the owned in-memory wire fixture; explicit barriers decide
// every ordering, and no test waits on wall time for its proof.
package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Hands the test the exact context pgx receives for BEGIN.
type beginStopTracer struct {
	contexts chan context.Context
}

// Captures BEGIN's context before pgx writes the statement.
func (self *beginStopTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "begin") {
		select {
		case self.contexts <- ctx:
		default:
		}
	}
	return ctx
}

// Completes pgx's tracer interface; query ends need no observation.
func (self *beginStopTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Requires that the next operation reuses `first` without another startup.
func requireBeginStopReuse(t *testing.T, fixture *pgPoolWireFixture, pool *safePgPool, first *pgconn.PgConn) {
	t.Helper()
	reuse, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	dbWithPool(reuse, pool, func(conn PgConn) {
		if conn.Conn().PgConn() != first {
			t.Error("a stopped caller destroyed its healthy pooled connection")
		}
		RaisePgResult(conn.Exec(reuse, "SELECT 1"))
	}, OptNoRetry())
	fixture.stateLock.Lock()
	dials := fixture.dialCount
	fixture.stateLock.Unlock()
	if dials != 1 {
		t.Fatal("a stopped caller forced another pooled connection startup", dials)
	}
}

// The caller stops while its BEGIN waits for a server. BEGIN must complete and
// be rolled back on the same connection: no interrupted socket, no cancel
// request, no replacement connection, and no callback work for the caller.
func TestTxStopDuringBeginKeepsPooledConnection(t *testing.T) {
	var observe atomic.Bool
	var rawDials atomic.Int32
	cancelDialed := make(chan struct{})
	var cancelOnce sync.Once
	beginArrived := make(chan struct{})
	var arrivedOnce sync.Once
	reply := make(chan bool, 1)
	tracer := &beginStopTracer{contexts: make(chan context.Context, 1)}
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if observe.Load() && strings.HasPrefix(query, "begin") {
			// The pooler holds this BEGIN until the test decides.
			arrivedOnce.Do(func() { close(beginArrived) })
			return <-reply
		}
		return true
	}, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network string, address string) (net.Conn, error) {
			rawDials.Add(1)
			// pgx dials its cancel request over the fixture's pipe network
			// only after it has already interrupted a round trip.
			if network == "pipe" {
				cancelOnce.Do(func() { close(cancelDialed) })
			}
			return dial(ctx, network, address)
		}
		config.ConnConfig.Tracer = tracer
	})
	// Validate the only connection first so the stop cannot race its startup.
	warm, err := pool.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first := warm.Conn().PgConn()
	warm.Release()
	observe.Store(true)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	callbacks := 0
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(PgTx) { callbacks++ }, TxReadCommitted, OptNoRetry())
		})
	}()
	beginCtx := <-tracer.contexts
	<-beginArrived
	stop()
	interrupted := false
	if beginCtx.Err() != nil {
		// The stop reached BEGIN's own context, so pgx's watcher interrupts
		// the round trip and dials a cancel request. The connection is lost
		// either way; dropping the held reply only shortens its cleanup.
		<-cancelDialed
		interrupted = true
		reply <- false
	} else {
		reply <- true
	}
	recovered := <-done
	if interrupted {
		t.Fatal("the caller's stop interrupted BEGIN in flight; pgx destroyed the pooled connection and sent a cancel request")
	}
	recoveredErr, _ := recovered.(error)
	if !errors.Is(recoveredErr, context.Canceled) || callbacks != 0 {
		t.Fatal("stopped caller lost its cause or ran callback work", recovered, callbacks)
	}
	if first.IsClosed() || rawDials.Load() != 1 {
		t.Fatal("stopped BEGIN closed its connection or dialed a cancel request", first.IsClosed(), rawDials.Load())
	}
	fixture.stateLock.Lock()
	queries := append([]string(nil), fixture.queries...)
	fixture.stateLock.Unlock()
	if len(queries) != 3 || !strings.HasPrefix(queries[1], "begin") || queries[2] != "rollback" {
		t.Fatal("completed BEGIN was not rolled back on its own connection", queries)
	}
	requireBeginStopReuse(t, fixture, pool, first)
}

// The caller stops after acquiring its connection but before BEGIN. pgx would
// refuse BEGIN without writing it and then destroy the untouched connection;
// the stop must instead surface without any statement and keep the connection.
func TestTxStopBeforeBeginKeepsPooledConnection(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	warm, err := pool.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first := warm.Conn().PgConn()
	warm.Release()

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		// The read-before-begin seam runs on the checked-out connection
		// immediately before BEGIN; stopping there is a caller whose
		// deadline expired between acquisition and BEGIN.
		txWithPool(ctx, pool, func(PgTx) { callbacks++ },
			TxReadBeforeBegin(func(PgCanQuery) { stop() }), OptNoRetry())
	})
	recoveredErr, _ := recovered.(error)
	if !errors.Is(recoveredErr, context.Canceled) || callbacks != 0 {
		t.Fatal("stopped caller lost its cause or ran callback work", recovered, callbacks)
	}
	if first.IsClosed() {
		t.Fatal("pgx's refused BEGIN destroyed an untouched pooled connection")
	}
	fixture.stateLock.Lock()
	queries := append([]string(nil), fixture.queries...)
	fixture.stateLock.Unlock()
	if len(queries) != 1 || queries[0] != "-- ping" {
		t.Fatal("a stopped caller wrote a transaction statement", queries)
	}
	requireBeginStopReuse(t, fixture, pool, first)
}
