// A scheduled close reads its grants while the payer's grant owner is held, so
// those reads must never pay for JIT compilation, whatever their estimated cost.
package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// One statement exactly as the real close sent it.
type deadlineJitStatement struct {
	sql  string
	args []any
}

// A grant read of the close and the settings its transaction applied before
// it.
type deadlineJitRead struct {
	deadlineJitStatement
	settings []deadlineJitStatement
}

// Records the deadline grant reads, the escrow read and each grant's pending
// sum, and the settings their transaction applied before them, per connection,
// from the real close's statements.
type deadlineJitCapture struct {
	stateLock    sync.Mutex
	connSettings map[*pgx.Conn][]deadlineJitStatement
	escrowReads  []deadlineJitRead
	pendingReads []deadlineJitRead
}

func (self *deadlineJitCapture) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sql := strings.ToLower(strings.TrimSpace(data.SQL))
	statement := deadlineJitStatement{sql: data.SQL, args: append([]any(nil), data.Args...)}
	read := func() deadlineJitRead {
		return deadlineJitRead{deadlineJitStatement: statement, settings: append([]deadlineJitStatement(nil), self.connSettings[conn]...)}
	}
	switch {
	case strings.HasPrefix(sql, "begin"):
		self.connSettings[conn] = nil
	case strings.HasPrefix(sql, "set ") || strings.Contains(sql, "set_config("):
		self.connSettings[conn] = append(self.connSettings[conn], statement)
	case data.SQL == deadlineEscrowRowsSql:
		self.escrowReads = append(self.escrowReads, read())
	case data.SQL == deadlinePendingDebitSql:
		self.pendingReads = append(self.pendingReads, read())
	}
	return ctx
}

func (self *deadlineJitCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// On Main the escrow read's planner cost passed the JIT inline and optimize
// thresholds: the row limit multiplies two debit journal probes per estimated
// escrow row, and dead journal tuples made each probe a few hundred pages. Every
// close then compiled the read, 5.7 of 66 database cores. A zero threshold
// stands in for that cost here. The escrow read and each grant's pending sum,
// which a close alone reads after it (readDeadlineEscrowRoomsInTx), must still
// run without JIT.
func TestDeadlineCloseBalanceReadRunsWithoutJit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 17, true))

		capture := &deadlineJitCapture{connSettings: map[*pgx.Conn][]deadlineJitStatement{}}
		func() {
			scope, err := server.NewTestPgQueryScope(ctx, capture)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := scope.Close(); err != nil {
					t.Error(err)
				}
			}()
			deadline := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
			result, err := ReconcileContractAtDeadline(ctx, contract.ContractId, deadline)
			if err != nil || result == nil || result.AlreadyClosed || result.Missing {
				t.Fatal("deadline did not close the open contract", result, err)
			}
		}()
		if len(capture.escrowReads) != 1 {
			t.Fatalf("the deadline close sent %d escrow reads, want one", len(capture.escrowReads))
		}
		// the fixture's one grant has a positive balance, so its room is read
		if len(capture.pendingReads) != 1 {
			t.Fatalf("the deadline close sent %d pending sums for its one grant, want one", len(capture.pendingReads))
		}

		// Replay each read with the settings its transaction applied before it.
		for _, read := range append(capture.escrowReads, capture.pendingReads...) {
			var jitAvailable bool
			var jitSetting, planJson string
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL jit_above_cost = 0`))
				for _, statement := range read.settings {
					server.RaisePgResult(tx.Exec(ctx, statement.sql, statement.args...))
				}
				server.Raise(tx.QueryRow(ctx, `SELECT pg_jit_available(), current_setting('jit')`).Scan(&jitAvailable, &jitSetting))
				server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+read.sql, read.args...).Scan(&planJson))
			})
			var plans []map[string]any
			if err := json.Unmarshal([]byte(planJson), &plans); err != nil || len(plans) != 1 {
				t.Fatalf("unreadable plan %q: %v", planJson, err)
			}
			if jit, compiled := plans[0]["JIT"]; compiled {
				t.Fatalf("a deadline grant read compiled JIT functions inside the close: %v\n%s", jit, read.sql)
			}
			if jitSetting != "off" {
				t.Fatalf("a deadline grant read ran with jit=%s (jit available=%t)\n%s", jitSetting, jitAvailable, read.sql)
			}
		}
	})
}
