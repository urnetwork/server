package model

// Counts the PostgreSQL statements one call path issues against the test
// database. Only statements issued under the marked context are recorded, so
// other users of the fixture pools cannot change a count. Safe for concurrent
// use.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

type pgStatementsKey struct{}

// Batches count each queued statement.
type pgStatements struct {
	stateLock  sync.Mutex
	statements []string
}

func (self *pgStatements) record(ctx context.Context, sql string) {
	if marker, _ := ctx.Value(pgStatementsKey{}).(*pgStatements); marker != self {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statements = append(self.statements, strings.Join(strings.Fields(sql), " "))
}

func (self *pgStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.record(ctx, data.SQL)
	return ctx
}

func (self *pgStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (self *pgStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		self.record(ctx, query.SQL)
	}
	return ctx
}

func (self *pgStatements) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (self *pgStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Returns the statements recorded since the last take.
func (self *pgStatements) take() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	statements := self.statements
	self.statements = nil
	return statements
}

// Returns the marked context whose statements are recorded. The caller must
// defer the returned close inside its TestEnv callback.
func observePgStatements(t testing.TB, ctx context.Context) (context.Context, *pgStatements, func()) {
	t.Helper()
	statements := &pgStatements{}
	scope, err := server.NewTestPgQueryScope(ctx, statements)
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(ctx, pgStatementsKey{}, statements), statements, func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}
