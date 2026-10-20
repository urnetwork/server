// A caller that stops while one of its transaction's statements is in flight
// must not destroy the pooled connection. pgx tears down a connection whose
// round trip it interrupts, and before closing it forwards a cancel request.
// Under transaction pooling that request waits for its own pooler slot, for up
// to pgx's 15 second bound, while the interrupted transaction keeps its server
// linked and idle in transaction; the pooler then destroys that server. Each
// abandoned transaction therefore pins a PostgreSQL backend for seconds and
// forces a replacement startup, which starves every other pooled client. This
// is the same failure `beginTx` prevents for BEGIN.
//
// A statement of a server-owned transaction instead runs on a context the
// caller's stop cannot interrupt mid round trip. It completes on its
// connection and reports the caller's stop as its outcome, so the callback
// unwinds as it did when the stop interrupted the statement, and the
// transaction owner rolls back on its detached context, which returns the
// server at once. A statement that has not finished `PgStatementStopTimeout`
// after the stop is interrupted as before. A caller that stopped before a
// statement keeps its own context: pgx refuses that statement without writing
// it. Once any statement has reported the stop, the owner never commits, even
// if the callback ignored the error.
//
// Like the pgx transaction it wraps, a server-owned transaction is used by one
// goroutine at a time.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
)

// PgStatementStopTimeout bounds how long a statement of a server-owned
// transaction already in flight may outlive its caller's stop. An ordinary
// statement completes well within it; one still waiting at the bound, for
// example on a row lock, is interrupted as it was before the stop was bounded.
const PgStatementStopTimeout = PgPingTimeout

// Bounds a transaction statement after the caller stops, in place of
// `PgStatementStopTimeout`. Only tests supply it, so a bound can be proven
// without waiting on wall time.
type txStatementStopTimeout time.Duration

// Counts transaction statements in flight when their caller stopped: those
// that completed on their connection, which keeps the session, and those the
// bound interrupted, which pgx tears down as before.
var pgStatementStopCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "db",
		Name:      "transaction_statement_stops_total",
		Help:      "Transaction statements in flight when their caller stopped, by whether they completed on the connection or the bound interrupted them",
	},
	[]string{"outcome"},
)

// Registers the statement stop metric with the default registry.
func init() {
	prometheus.MustRegister(pgStatementStopCounter)
}

// How a stop-bounded statement ended relative to its caller's stop.
type pgStatementStop int

const (
	// the caller did not stop while the statement was in flight
	pgStatementRan pgStatementStop = iota
	// the caller stopped and the statement completed on its connection
	pgStatementStopped
	// the caller stopped and the bound interrupted the statement
	pgStatementInterrupted
)

// Starts one statement on a context the caller's stop cannot interrupt for
// `statementStopTimeout`. `end` must be called once the statement's reply is
// complete, and reports whether the caller stopped while it was in flight.
// `bounded` is false when the statement keeps the caller's context: the
// transaction is not server-owned, the caller cannot stop, or it has already
// stopped, in which case pgx refuses the statement without writing it.
func (self *postCommitPgTx) beginStatement(ctx context.Context) (statementCtx context.Context, end func() pgStatementStop, bounded bool) {
	if !self.statementStopBounded || ctx.Done() == nil {
		return ctx, nil, false
	}
	if err := ctx.Err(); err != nil {
		self.reportStatementStop(err)
		return ctx, nil, false
	}
	statementCtx, statementCancel := context.WithCancel(context.WithoutCancel(ctx))
	timeout := self.statementStopTimeout
	stopBound := context.AfterFunc(ctx, func() {
		select {
		case <-statementCtx.Done():
		case <-time.After(timeout):
			statementCancel()
		}
	})
	ended := false
	end = func() pgStatementStop {
		if ended {
			return pgStatementRan
		}
		ended = true
		stop := pgStatementRan
		if !stopBound() {
			// the stop bound started, so the caller stopped in flight
			stop = pgStatementStopped
			if statementCtx.Err() != nil {
				stop = pgStatementInterrupted
			}
		}
		statementCancel()
		return stop
	}
	return statementCtx, end, true
}

// Records the first stop reported to the callback. The owner rolls back
// instead of committing once a statement has reported the stop.
func (self *postCommitPgTx) reportStatementStop(err error) {
	if self.statementStopErr == nil {
		self.statementStopErr = err
	}
}

// The outcome a statement reports to the callback. A statement that completed
// after its caller stopped reports the stop, joined with any failure it raced;
// a statement the bound interrupted reports the stop in place of the bound's
// own cancellation.
func (self *postCommitPgTx) statementOutcome(ctx context.Context, stop pgStatementStop, err error) error {
	switch stop {
	case pgStatementStopped:
		pgStatementStopCounter.WithLabelValues("completed").Inc()
		if err == nil {
			err = ctx.Err()
		} else {
			err = errors.Join(err, ctx.Err())
		}
	case pgStatementInterrupted:
		pgStatementStopCounter.WithLabelValues("interrupted").Inc()
		if err == nil || errors.Is(err, context.Canceled) {
			err = ctx.Err()
		} else {
			err = errors.Join(err, ctx.Err())
		}
	default:
		return err
	}
	self.reportStatementStop(ctx.Err())
	return err
}

// Runs the statement on the stop-bounded context.
func (self *postCommitPgTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	statementCtx, end, bounded := self.beginStatement(ctx)
	tag, err := self.PgTx.Exec(statementCtx, sql, arguments...)
	if bounded {
		err = self.statementOutcome(ctx, end(), err)
	}
	return tag, err
}

// Runs the query on the stop-bounded context; its rows hold the bound until
// they are closed or read to the end.
func (self *postCommitPgTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	statementCtx, end, bounded := self.beginStatement(ctx)
	rows, err := self.PgTx.Query(statementCtx, sql, args...)
	if !bounded {
		return rows, err
	}
	if rows == nil {
		return nil, self.statementOutcome(ctx, end(), err)
	}
	boundedRows := &pgStatementStopRows{Rows: rows, ctx: ctx, end: end, tx: self}
	if err != nil {
		// pgx closed the rows with the error it returns
		boundedRows.finish(pgStatementRan)
		return boundedRows, boundedRows.err
	}
	return boundedRows, nil
}

// Runs the single-row query on the stop-bounded context.
func (self *postCommitPgTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if !self.statementStopBounded {
		return self.PgTx.QueryRow(ctx, sql, args...)
	}
	rows, err := self.Query(ctx, sql, args...)
	if rows == nil {
		return pgStatementErrRow{err: err}
	}
	return pgStatementStopRow{rows: rows}
}

// Sends the batch on the stop-bounded context; its results hold the bound
// until they are closed.
func (self *postCommitPgTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	statementCtx, end, bounded := self.beginStatement(ctx)
	results := self.PgTx.SendBatch(statementCtx, batch)
	if !bounded {
		return results
	}
	if results == nil {
		self.statementOutcome(ctx, end(), nil)
		return nil
	}
	return &pgStatementStopBatchResults{BatchResults: results, ctx: ctx, end: end, tx: self}
}

// Runs the copy on the stop-bounded context.
func (self *postCommitPgTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	statementCtx, end, bounded := self.beginStatement(ctx)
	count, err := self.PgTx.CopyFrom(statementCtx, tableName, columnNames, rowSrc)
	if bounded {
		err = self.statementOutcome(ctx, end(), err)
	}
	return count, err
}

// Prepares on the stop-bounded context.
func (self *postCommitPgTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	statementCtx, end, bounded := self.beginStatement(ctx)
	description, err := self.PgTx.Prepare(statementCtx, name, sql)
	if bounded {
		err = self.statementOutcome(ctx, end(), err)
	}
	return description, err
}

// Rows of a stop-bounded query. Once the caller stops, no further row is
// returned: the remainder is drained on the connection and Err reports the
// stop, as the interrupted read reported it before.
type pgStatementStopRows struct {
	pgx.Rows
	ctx   context.Context
	end   func() pgStatementStop
	tx    *postCommitPgTx
	ended bool
	err   error
}

// Ends at the first row boundary after the caller's stop.
func (self *pgStatementStopRows) Next() bool {
	if self.ended {
		return false
	}
	if self.ctx.Err() != nil {
		// rows withheld from a stopped caller are never reported complete,
		// even if its stop has not reached the bound yet
		self.Rows.Close()
		self.finish(pgStatementStopped)
		return false
	}
	if self.Rows.Next() {
		return true
	}
	self.finish(pgStatementRan)
	return false
}

// Closes the rows, draining any remainder, and ends the statement's bound.
func (self *pgStatementStopRows) Close() {
	self.Rows.Close()
	self.finish(pgStatementRan)
}

// Reports the stop once the rows are closed.
func (self *pgStatementStopRows) Err() error {
	if self.ended {
		return self.err
	}
	return self.Rows.Err()
}

// Ends the bound with at least the given stop.
func (self *pgStatementStopRows) finish(minimum pgStatementStop) {
	if self.ended {
		return
	}
	self.ended = true
	stop := max(self.end(), minimum)
	self.err = self.tx.statementOutcome(self.ctx, stop, self.Rows.Err())
}

// Mirrors pgx's single-row scan over stop-bounded rows.
type pgStatementStopRow struct {
	rows pgx.Rows
}

// Scans the first row and closes the rows, as pgx's QueryRow does.
func (self pgStatementStopRow) Scan(dest ...any) error {
	rows := self.rows
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range dest {
		if _, ok := d.(*pgtype.DriverBytes); ok {
			rows.Close()
			return fmt.Errorf("cannot scan into *pgtype.DriverBytes from QueryRow")
		}
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	rows.Scan(dest...)
	rows.Close()
	return rows.Err()
}

// A single-row query that produced no rows to scan.
type pgStatementErrRow struct {
	err error
}

// Returns the query's error.
func (self pgStatementErrRow) Scan(...any) error {
	return self.err
}

// Results of a stop-bounded batch. Close reports the caller's stop.
type pgStatementStopBatchResults struct {
	pgx.BatchResults
	ctx   context.Context
	end   func() pgStatementStop
	tx    *postCommitPgTx
	ended bool
	err   error
}

// Closes the results and ends the batch's bound.
func (self *pgStatementStopBatchResults) Close() error {
	err := self.BatchResults.Close()
	if !self.ended {
		self.ended = true
		self.err = self.tx.statementOutcome(self.ctx, self.end(), err)
	}
	return self.err
}
