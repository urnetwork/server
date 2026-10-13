// Statement hooks observe a settlement's real owned transaction from the driver.
// Financial writers take transaction ownership only on the server's own
// transaction, so a test cannot wrap that transaction; it binds hooks to that
// transaction's connection through the context the settlement uses instead.
package model

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Callbacks for the statements one transaction sends with a hooked context. A
// batch reports one statement whose text lists its queued statements.
type legacyStatementHook struct {
	// Runs before the statement or batch is sent.
	before func(ctx context.Context, sql string, queued []string)
	// Runs before a single statement is sent, with its arguments, after before.
	beforeQuery func(ctx context.Context, sql string, args []any)
	// Runs after its results are consumed, with the elapsed time and error.
	after func(sql string, elapsed time.Duration, err error)
	// Runs after a single statement succeeds, with its command tag.
	completed func(sql string, tag pgconn.CommandTag)
}

// The hooks bound to one transaction's connection until released. Statements
// other connections send with the same context pass through, and so do
// statements after release, such as posts that captured the context and run
// on the same pooled connection once the transaction has ended.
type legacyStatementHookScope struct {
	conn     *pgx.Conn
	hooks    []*legacyStatementHook
	released atomic.Bool
}

// Context key of the hook scope.
type legacyStatementHookKey struct{}

// Routes the statements tx sends with the returned context to hooks, in order,
// after any hooks the context already binds to the same transaction. Release
// before the transaction ends; the deferred release in its callback does.
func withLegacyStatementHooks(ctx context.Context, tx server.PgTx, hooks ...*legacyStatementHook) (context.Context, func()) {
	scope := &legacyStatementHookScope{conn: tx.Conn()}
	if existing, ok := ctx.Value(legacyStatementHookKey{}).(*legacyStatementHookScope); ok && existing.conn == scope.conn && !existing.released.Load() {
		scope.hooks = append(scope.hooks, existing.hooks...)
	}
	scope.hooks = append(scope.hooks, hooks...)
	return context.WithValue(ctx, legacyStatementHookKey{}, scope), func() { scope.released.Store(true) }
}

// The hooks for a statement conn sends with ctx.
func legacyStatementHooksFor(ctx context.Context, conn *pgx.Conn) []*legacyStatementHook {
	scope, ok := ctx.Value(legacyStatementHookKey{}).(*legacyStatementHookScope)
	if !ok || scope.conn != conn || scope.released.Load() {
		return nil
	}
	return scope.hooks
}

// One traced statement's text and start, carried from start to end.
type legacyStatementStart struct {
	sql     string
	started time.Time
	hooks   []*legacyStatementHook
}

// Context key of a hooked statement's start.
type legacyStatementStartKey struct{}

// Dispatches each traced statement to the hooks its context binds to the
// statement's connection. Every other statement, including fixture setup and
// other pages, passes through.
type legacyStatementTracer struct{}

// Runs the before hooks and remembers the statement for its end.
func (legacyStatementTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return legacyStatementHookStart(ctx, conn, data.SQL, nil, data.Args)
}

// Runs the after hooks once the statement's results are consumed.
func (legacyStatementTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	legacyStatementHookEnd(ctx, &data.CommandTag, data.Err)
}

// Runs the before hooks once for the whole batch.
func (legacyStatementTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	queued := make([]string, 0, len(data.Batch.QueuedQueries))
	for _, query := range data.Batch.QueuedQueries {
		queued = append(queued, query.SQL)
	}
	sql := fmt.Sprintf("BATCH %d", len(queued))
	for _, query := range queued {
		sql += " | " + query
	}
	return legacyStatementHookStart(ctx, conn, sql, queued, nil)
}

// Each queued result belongs to the batch's single round trip.
func (legacyStatementTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

// Runs the after hooks once the batch is closed.
func (legacyStatementTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	legacyStatementHookEnd(ctx, nil, data.Err)
}

// Shared start of a statement or batch. The start carries its own hooks, so
// its end runs exactly the hooks that saw it begin. A batch has no arguments
// of its own; its queued statements carry them.
func legacyStatementHookStart(ctx context.Context, conn *pgx.Conn, sql string, queued []string, args []any) context.Context {
	hooks := legacyStatementHooksFor(ctx, conn)
	if len(hooks) == 0 {
		return ctx
	}
	for _, hook := range hooks {
		if hook.before != nil {
			hook.before(ctx, sql, queued)
		}
		if hook.beforeQuery != nil && queued == nil {
			hook.beforeQuery(ctx, sql, args)
		}
	}
	return context.WithValue(ctx, legacyStatementStartKey{}, legacyStatementStart{sql: sql, started: time.Now(), hooks: hooks})
}

// Shared end of a statement or batch. A batch has no single command tag.
func legacyStatementHookEnd(ctx context.Context, tag *pgconn.CommandTag, err error) {
	start, ok := ctx.Value(legacyStatementStartKey{}).(legacyStatementStart)
	if !ok {
		return
	}
	elapsed := time.Since(start.started)
	for _, hook := range start.hooks {
		if hook.after != nil {
			hook.after(start.sql, elapsed, err)
		}
		if hook.completed != nil && tag != nil && err == nil {
			hook.completed(start.sql, *tag)
		}
	}
}

// Observes every pool statement until the returned stop runs. Start it before
// the test borrows a connection, and stop it after every borrower returns. The
// stop is idempotent: defer it, and call it early before another scope starts.
func startLegacyStatementHooks(t testing.TB, ctx context.Context) func() {
	t.Helper()
	scope, err := server.NewTestPgQueryScope(ctx, legacyStatementTracer{})
	if err != nil {
		t.Fatal("statement hooks could not observe the fixture pools", err)
	}
	return func() { server.Raise(scope.Close()) }
}
