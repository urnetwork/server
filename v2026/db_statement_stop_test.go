// A caller that stops while a statement of its transaction is in flight must
// not destroy its pooled connection. pgx would interrupt the round trip, send a
// cancel request through the pooler and only then close, so the server stays
// idle in transaction until the pooler destroys it. These tests drive real pgx
// pool connections on the owned in-memory wire fixture: the fixture holds the
// statement's reply, the test stops the caller at that barrier, and the
// statement's own context decides the outcome. No proof waits on wall time.
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
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A bound no test reaches, so only a caller's stop can affect a statement.
const statementStopTestTimeout = time.Hour

// Hands the test the exact context pgx receives for the held statement.
type statementStopTracer struct {
	prefix   string
	contexts chan context.Context
}

// Captures a query's context before pgx writes the statement.
func (self *statementStopTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, self.prefix) {
		self.capture(ctx)
	}
	return ctx
}

// Completes pgx's query tracer interface; query ends need no observation.
func (self *statementStopTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Captures a batch's context before pgx writes it.
func (self *statementStopTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if 0 < len(data.Batch.QueuedQueries) && strings.HasPrefix(data.Batch.QueuedQueries[0].SQL, self.prefix) {
		self.capture(ctx)
	}
	return ctx
}

// Completes pgx's batch tracer interface.
func (self *statementStopTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

// Completes pgx's batch tracer interface.
func (self *statementStopTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// Keeps the first matching context; later ones are not needed.
func (self *statementStopTracer) capture(ctx context.Context) {
	select {
	case self.contexts <- ctx:
	default:
	}
}

// One pooled connection whose statement with the held prefix waits for the
// test's reply, plus the evidence of a cancel request or another startup.
type statementStopFixture struct {
	fixture      *pgPoolWireFixture
	pool         *safePgPool
	first        *pgconn.PgConn
	tracer       *statementStopTracer
	arrived      chan struct{}
	reply        chan bool
	cancelDialed chan struct{}
	rawDials     *atomic.Int32
}

// Validates the only pooled connection before any statement is held.
func newStatementStopFixture(t *testing.T, prefix string) *statementStopFixture {
	t.Helper()
	var observe atomic.Bool
	rawDials := &atomic.Int32{}
	cancelDialed := make(chan struct{})
	var cancelOnce sync.Once
	arrived := make(chan struct{})
	var arrivedOnce sync.Once
	reply := make(chan bool, 1)
	tracer := &statementStopTracer{prefix: prefix, contexts: make(chan context.Context, 1)}
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if observe.Load() && strings.HasPrefix(query, prefix) {
			// the pooler holds this statement's reply until the test decides
			arrivedOnce.Do(func() { close(arrived) })
			return <-reply
		}
		return true
	}, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network string, address string) (net.Conn, error) {
			rawDials.Add(1)
			// pgx dials its cancel request over the fixture's pipe network
			// only after it has interrupted a round trip
			if network == "pipe" {
				cancelOnce.Do(func() { close(cancelDialed) })
			}
			return dial(ctx, network, address)
		}
		config.ConnConfig.Tracer = tracer
	})
	fixture.queryRows = func(_ int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
		if strings.HasPrefix(query, prefix) && strings.HasPrefix(prefix, "SELECT") {
			return []pgproto3.FieldDescription{{Name: []byte("synthetic_value"), DataTypeOID: 25, Format: 0}},
				[][][]byte{{[]byte("first")}, {[]byte("second")}}
		}
		return nil, nil
	}
	// Validate the only connection first so the stop cannot race its startup.
	warm, err := pool.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first := warm.Conn().PgConn()
	warm.Release()
	observe.Store(true)
	return &statementStopFixture{
		fixture:      fixture,
		pool:         pool,
		first:        first,
		tracer:       tracer,
		arrived:      arrived,
		reply:        reply,
		cancelDialed: cancelDialed,
		rawDials:     rawDials,
	}
}

// Stops the caller while the held statement is in flight, then answers it.
// Returns whether the stop reached the statement's own context, in which case
// pgx interrupted the round trip and dialed its cancel request.
func (self *statementStopFixture) stopInFlight(stop context.CancelFunc) (interrupted bool) {
	statementCtx := <-self.tracer.contexts
	<-self.arrived
	stop()
	if statementCtx.Err() != nil {
		// The connection is lost either way; dropping the held reply only
		// shortens its cleanup.
		<-self.cancelDialed
		self.reply <- false
		return true
	}
	self.reply <- true
	return false
}

// Statements the fixture received, in order.
func (self *statementStopFixture) queries() []string {
	self.fixture.stateLock.Lock()
	defer self.fixture.stateLock.Unlock()
	return append([]string(nil), self.fixture.queries...)
}

// Requires that the transaction ended in a rollback on its own connection,
// without a cancel request, and that the connection is reused.
func (self *statementStopFixture) requireRolledBackAndReused(t *testing.T, statement string) {
	t.Helper()
	if self.first.IsClosed() || self.rawDials.Load() != 1 {
		t.Fatal("a stopped statement closed its connection or dialed a cancel request", self.first.IsClosed(), self.rawDials.Load())
	}
	queries := self.queries()
	if len(queries) != 4 || !strings.HasPrefix(queries[1], "begin") || !strings.HasPrefix(queries[2], statement) || queries[3] != "rollback" {
		t.Fatal("the stopped transaction was not rolled back on its own connection", queries)
	}
	requireBeginStopReuse(t, self.fixture, self.pool, self.first)
}

// The caller stops while an UPDATE waits for its reply. The statement must
// complete on its connection and report the stop, and the transaction must
// roll back at once: no interrupted socket, no cancel request, no backend left
// idle in transaction, and no callback work after the stopped statement.
func TestTxStopDuringStatementKeepsPooledConnection(t *testing.T) {
	fixture := newStatementStopFixture(t, "UPDATE synthetic_row")
	completedBefore := testutil.ToFloat64(pgStatementStopCounter.WithLabelValues("completed"))
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	afterStatement := 0
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			txWithPool(ctx, fixture.pool, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, "UPDATE synthetic_row SET synthetic_value = 1"))
				afterStatement++
			}, TxReadCommitted, OptNoRetry(), txStatementStopTimeout(statementStopTestTimeout))
		})
	}()
	interrupted := fixture.stopInFlight(stop)
	recovered := <-done
	if interrupted {
		t.Fatal("the caller's stop interrupted a transaction statement in flight; pgx destroyed the pooled connection and sent a cancel request while its server stayed idle in transaction")
	}
	if err, _ := recovered.(error); !errors.Is(err, context.Canceled) || afterStatement != 0 {
		t.Fatal("the stopped statement lost its caller's stop or the callback went on", recovered, afterStatement)
	}
	if completed := testutil.ToFloat64(pgStatementStopCounter.WithLabelValues("completed")) - completedBefore; completed != 1 {
		t.Fatal("the completed stopped statement was not counted once", completed)
	}
	fixture.requireRolledBackAndReused(t, "UPDATE synthetic_row")
}

// A caller context the test expires explicitly. Like a passed deadline, it
// reports `context.DeadlineExceeded`, without any timer deciding when.
type expiringTestContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

// Keeps the parent's values; only `expire` ends it.
func newExpiringTestContext(parent context.Context) *expiringTestContext {
	return &expiringTestContext{Context: parent, done: make(chan struct{})}
}

// Closed by `expire`.
func (self *expiringTestContext) Done() <-chan struct{} {
	return self.done
}

// A passed deadline once expired.
func (self *expiringTestContext) Err() error {
	select {
	case <-self.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// Ends the context; safe to repeat.
func (self *expiringTestContext) expire() {
	self.once.Do(func() { close(self.done) })
}

// The caller's deadline passes while a single-row query waits for its reply.
// The rows must be drained on the connection, the scan must report the
// deadline, and the transaction must roll back on the same connection.
func TestTxDeadlineDuringQueryRowKeepsPooledConnection(t *testing.T) {
	fixture := newStatementStopFixture(t, "SELECT synthetic_value")
	ctx := newExpiringTestContext(t.Context())
	defer ctx.expire()
	afterStatement := 0
	var scanErr error
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			txWithPool(ctx, fixture.pool, func(tx PgTx) {
				var value string
				scanErr = tx.QueryRow(ctx, "SELECT synthetic_value FROM synthetic_row").Scan(&value)
				Raise(scanErr)
				afterStatement++
			}, TxReadCommitted, OptNoRetry(), txStatementStopTimeout(statementStopTestTimeout))
		})
	}()
	interrupted := fixture.stopInFlight(ctx.expire)
	recovered := <-done
	if interrupted {
		t.Fatal("the caller's deadline interrupted a transaction query in flight; pgx destroyed the pooled connection and sent a cancel request while its server stayed idle in transaction")
	}
	if err, _ := recovered.(error); !errors.Is(scanErr, context.DeadlineExceeded) || !errors.Is(err, context.DeadlineExceeded) || afterStatement != 0 {
		t.Fatal("the stopped query did not report its caller's deadline", scanErr, recovered, afterStatement)
	}
	fixture.requireRolledBackAndReused(t, "SELECT synthetic_value")
}

// A caller that stops while it iterates rows sees no further row: the rest is
// drained on the connection and Err reports the stop.
func TestTxStopDuringRowsEndsAtRowBoundary(t *testing.T) {
	fixture := newStatementStopFixture(t, "SELECT synthetic_value")
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	values := []string{}
	var rowsErr error
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			txWithPool(ctx, fixture.pool, func(tx PgTx) {
				rows, err := tx.Query(ctx, "SELECT synthetic_value FROM synthetic_row")
				Raise(err)
				defer rows.Close()
				for rows.Next() {
					var value string
					Raise(rows.Scan(&value))
					values = append(values, value)
					// the caller stops after its first row
					stop()
				}
				rows.Close()
				rowsErr = rows.Err()
				Raise(rowsErr)
			}, TxReadCommitted, OptNoRetry(), txStatementStopTimeout(statementStopTestTimeout))
		})
	}()
	// The reply is released at once; this caller stops from inside its loop.
	<-fixture.arrived
	fixture.reply <- true
	recovered := <-done
	if err, _ := recovered.(error); !errors.Is(err, context.Canceled) || !errors.Is(rowsErr, context.Canceled) {
		t.Fatal("rows withheld from a stopped caller were reported complete", recovered, rowsErr)
	}
	if len(values) != 1 || values[0] != "first" {
		t.Fatal("a stopped caller received rows after its stop", values)
	}
	fixture.requireRolledBackAndReused(t, "SELECT synthetic_value")
}

// The caller stops while a batch waits for its reply. Closing the results must
// report the stop after the reply completes on the connection.
func TestTxStopDuringBatchKeepsPooledConnection(t *testing.T) {
	fixture := newStatementStopFixture(t, "UPDATE synthetic_batch")
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	afterStatement := 0
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			txWithPool(ctx, fixture.pool, func(tx PgTx) {
				BatchInTx(ctx, tx, func(batch PgBatch) {
					batch.Queue("UPDATE synthetic_batch SET synthetic_value = 1")
				})
				afterStatement++
			}, TxReadCommitted, OptNoRetry(), txStatementStopTimeout(statementStopTestTimeout))
		})
	}()
	interrupted := fixture.stopInFlight(stop)
	recovered := <-done
	if interrupted {
		t.Fatal("the caller's stop interrupted a transaction batch in flight; pgx destroyed the pooled connection and sent a cancel request while its server stayed idle in transaction")
	}
	if err, _ := recovered.(error); !errors.Is(err, context.Canceled) || afterStatement != 0 {
		t.Fatal("the stopped batch lost its caller's stop or the callback went on", recovered, afterStatement)
	}
	fixture.requireRolledBackAndReused(t, "UPDATE synthetic_batch")
}

// A callback that ignores a statement's report of the caller's stop and
// returns normally must not commit: the owner rolls back on its connection.
func TestTxStopReportedToCallbackNeverCommits(t *testing.T) {
	fixture := newStatementStopFixture(t, "UPDATE synthetic_row")
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var ignored error
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			txWithPool(ctx, fixture.pool, func(tx PgTx) {
				_, ignored = tx.Exec(ctx, "UPDATE synthetic_row SET synthetic_value = 2")
			}, TxReadCommitted, OptNoRetry(), txStatementStopTimeout(statementStopTestTimeout))
		})
	}()
	interrupted := fixture.stopInFlight(stop)
	recovered := <-done
	if interrupted {
		t.Fatal("the caller's stop interrupted a transaction statement in flight; pgx destroyed the pooled connection and sent a cancel request while its server stayed idle in transaction")
	}
	if err, _ := recovered.(error); !errors.Is(ignored, context.Canceled) || !errors.Is(err, context.Canceled) {
		t.Fatal("the stopped transaction reported success or lost its cause", ignored, recovered)
	}
	fixture.requireRolledBackAndReused(t, "UPDATE synthetic_row")
}

// A caller that stops between statements has its next statement refused
// without a write. A callback that ignores the refusal must not commit the
// statements before it.
func TestTxStopBetweenStatementsNeverCommitsPartialWork(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	warm, err := pool.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first := warm.Conn().PgConn()
	warm.Release()

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var refused error
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, "UPDATE synthetic_first SET synthetic_value = 1"))
			stop()
			_, refused = tx.Exec(ctx, "UPDATE synthetic_second SET synthetic_value = 1")
		}, TxReadCommitted, OptNoRetry(), txStatementStopTimeout(statementStopTestTimeout))
	})
	if err, _ := recovered.(error); !errors.Is(refused, context.Canceled) || !errors.Is(err, context.Canceled) {
		t.Fatal("partial work of a stopped caller was reported committed", refused, recovered)
	}
	fixture.stateLock.Lock()
	queries := append([]string(nil), fixture.queries...)
	fixture.stateLock.Unlock()
	if len(queries) != 4 || !strings.HasPrefix(queries[2], "UPDATE synthetic_first") || queries[3] != "rollback" {
		t.Fatal("a stopped caller's partial transaction was committed or its refused statement written", queries)
	}
	if first.IsClosed() {
		t.Fatal("a refused statement destroyed its pooled connection")
	}
	requireBeginStopReuse(t, fixture, pool, first)
}

// A statement that cannot complete after its caller stops is interrupted at the
// bound. The caller then sees its own stop and no further callback work runs.
func TestTxStopDuringStatementIsBounded(t *testing.T) {
	arrived := make(chan struct{})
	var arrivedOnce sync.Once
	release := make(chan struct{})
	_, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		if strings.HasPrefix(query, "UPDATE synthetic_stuck") {
			arrivedOnce.Do(func() { close(arrived) })
			<-release
			return false
		}
		return true
	}, neverPingTestPool)
	// Registered after the fixture, so it runs first and frees its worker.
	t.Cleanup(func() { close(release) })
	interruptedBefore := testutil.ToFloat64(pgStatementStopCounter.WithLabelValues("interrupted"))
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	afterStatement := 0
	done := make(chan any, 1)
	go func() {
		done <- captureDbErrorPanic(func() {
			txWithPool(ctx, pool, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, "UPDATE synthetic_stuck SET synthetic_value = 1"))
				afterStatement++
			}, TxReadCommitted, OptNoRetry(), txStatementStopTimeout(0))
		})
	}()
	<-arrived
	stop()
	recovered := <-done
	err, _ := recovered.(error)
	if !errors.Is(err, context.Canceled) || afterStatement != 0 {
		t.Fatal("the bounded statement lost its caller's stop or the callback went on", recovered, afterStatement)
	}
	if interrupted := testutil.ToFloat64(pgStatementStopCounter.WithLabelValues("interrupted")) - interruptedBefore; interrupted != 1 {
		t.Fatal("the interrupted statement was not counted once", interrupted)
	}
}

// A statement whose caller never stops keeps its ordinary outcome, and the
// transaction commits on its connection.
func TestTxStatementWithoutStopCommits(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, neverPingTestPool)
	fixture.queryRows = func(_ int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
		if strings.HasPrefix(query, "SELECT synthetic_value") {
			return []pgproto3.FieldDescription{{Name: []byte("synthetic_value"), DataTypeOID: 25, Format: 0}},
				[][][]byte{{[]byte("first")}}
		}
		return nil, nil
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	var value string
	txWithPool(ctx, pool, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, "UPDATE synthetic_row SET synthetic_value = 1"))
		Raise(tx.QueryRow(ctx, "SELECT synthetic_value FROM synthetic_row").Scan(&value))
		BatchInTx(ctx, tx, func(batch PgBatch) {
			batch.Queue("UPDATE synthetic_batch SET synthetic_value = 1")
		})
	}, TxReadCommitted, OptNoRetry())
	// A stop after the commit reaches no statement.
	stop()
	fixture.stateLock.Lock()
	queries := append([]string(nil), fixture.queries...)
	fixture.stateLock.Unlock()
	if value != "first" || len(queries) != 6 || queries[5] != "commit" {
		t.Fatal("a live caller's transaction did not commit its statements", value, queries)
	}
}
