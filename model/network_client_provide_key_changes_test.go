package model

// Every connected client's announce loop reads its provide key changes on each
// sync. These tests drive the real read against the test database and record
// the statements it issues, so a transaction around the single count shows up
// as its BEGIN and COMMIT round trips.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

type provideKeyChangeStatementsKey struct{}

// Records the statements issued under one marked context, so other users of
// the fixture pools cannot change what is observed.
type provideKeyChangeStatements struct {
	stateLock  sync.Mutex
	statements []string
}

// Keeps a statement issued under this recorder's marked context.
func (self *provideKeyChangeStatements) record(ctx context.Context, sql string) {
	if marker, _ := ctx.Value(provideKeyChangeStatementsKey{}).(*provideKeyChangeStatements); marker != self {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statements = append(self.statements, strings.Join(strings.Fields(sql), " "))
}

// Records the statement before pgx writes it.
func (self *provideKeyChangeStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.record(ctx, data.SQL)
	return ctx
}

// Completes pgx's query tracer interface.
func (self *provideKeyChangeStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Statements recorded so far, in order.
func (self *provideKeyChangeStatements) taken() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.statements...)
}

// The single count must not hold a pooled server across BEGIN and COMMIT
// round trips, which keep it idle in transaction behind the transaction pooler.
func TestGetProvideKeyChangesReadsWithoutTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		startTime := server.NowUtc()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})

		statements := &provideKeyChangeStatements{}
		scope, err := server.NewTestPgQueryScope(ctx, statements)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		markedCtx := context.WithValue(ctx, provideKeyChangeStatementsKey{}, statements)
		changedCount, provideModes := GetProvideKeyChanges(markedCtx, clientId, startTime)
		if changedCount != 1 || !provideModes[ProvideModePublic] {
			t.Fatal("provide key changes lost their count or modes", changedCount, provideModes)
		}
		counts := 0
		for _, statement := range statements.taken() {
			lower := strings.ToLower(statement)
			if strings.HasPrefix(lower, "begin") || lower == "commit" || lower == "rollback" {
				t.Fatal("the provide key change count ran inside a transaction", statements.taken())
			}
			if strings.Contains(statement, "COUNT(*) AS changed_count") {
				counts++
			}
		}
		if counts != 1 {
			t.Fatal("the provide key change count was not read exactly once", statements.taken())
		}
	})
}
