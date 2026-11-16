package server

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A held session may run successive top-level transactions, but cannot begin a
// nested transaction or savepoint. All work must stay on the original socket.
func TestTxInConnKeepsSequentialOwnerAndRejectsNesting(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx := context.Background()
	conn, err := pool.open().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	callbacks := 0
	for range 3 {
		DbInConn(ctx, conn, func(read PgConn) { RaisePgResult(read.Exec(ctx, "SELECT 1")) })
		TxInConn(ctx, conn, func(tx PgTx) {
			callbacks++
			nested := false
			recovered := captureDbErrorPanic(func() {
				TxInConn(ctx, conn, func(PgTx) { nested = true })
			})
			if recovered == nil || nested {
				t.Fatal("nested transaction was admitted", recovered, nested)
			}
			RaisePgResult(tx.Exec(ctx, "INSERT INTO synthetic_effect VALUES (1)"))
		})
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	begins, commits := 0, 0
	for _, query := range fixture.queries {
		if strings.HasPrefix(query, "begin") {
			begins++
		}
		if query == "commit" {
			commits++
		}
	}
	if callbacks != 3 || begins != 3 || commits != 3 || fixture.dialCount != 1 {
		t.Fatal("sequential transactions changed ownership", callbacks, begins, commits, fixture.dialCount)
	}
}

// A lost commit acknowledgement must remain uncertain. Replacing the socket
// would also discard its session advisory lock, so replay is never permitted.
func TestTxInConnLostCommitDoesNotReplaceOwnerOrReplay(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool { return query != "commit" }, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx := context.Background()
	conn, err := pool.open().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	callbacks := 0
	recovered := captureDbErrorPanic(func() {
		TxInConn(ctx, conn, func(tx PgTx) {
			callbacks++
			RaisePgResult(tx.Exec(ctx, "INSERT INTO synthetic_effect VALUES (1)"))
		})
	})
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if recovered == nil || callbacks != 1 || fixture.dialCount != 1 {
		t.Fatal("unknown commit replaced or replayed its owner", recovered, callbacks, fixture.dialCount)
	}
}
