// The destination-close byte sum reads its chunks without JIT compilation, and
// the walk's sums are unchanged.
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

// One statement exactly as the walk sent it.
type transferCloseChunkJitStatement struct {
	sql  string
	args []any
}

// A chunk read, whether a transaction was open on its connection, and the
// settings that transaction applied before it.
type transferCloseChunkJitRead struct {
	transferCloseChunkJitStatement
	inTx     bool
	settings []transferCloseChunkJitStatement
}

// Records each chunk read of one marker from the walk's real statements, with
// the transaction state of its connection.
type transferCloseChunkJitCapture struct {
	stateLock sync.Mutex
	marker    string
	inTx      map[*pgx.Conn]bool
	settings  map[*pgx.Conn][]transferCloseChunkJitStatement
	reads     []transferCloseChunkJitRead
}

func newTransferCloseChunkJitCapture(marker string) *transferCloseChunkJitCapture {
	return &transferCloseChunkJitCapture{
		marker:   marker,
		inTx:     map[*pgx.Conn]bool{},
		settings: map[*pgx.Conn][]transferCloseChunkJitStatement{},
	}
}

func (self *transferCloseChunkJitCapture) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sql := strings.ToLower(strings.TrimSpace(data.SQL))
	statement := transferCloseChunkJitStatement{sql: data.SQL, args: append([]any(nil), data.Args...)}
	switch {
	case strings.HasPrefix(sql, "begin"):
		self.inTx[conn] = true
		self.settings[conn] = nil
	case strings.HasPrefix(sql, "commit") || strings.HasPrefix(sql, "rollback"):
		self.inTx[conn] = false
		self.settings[conn] = nil
	case strings.Contains(data.SQL, "/* "+self.marker+" */"):
		self.reads = append(self.reads, transferCloseChunkJitRead{
			transferCloseChunkJitStatement: statement,
			inTx:                           self.inTx[conn],
			settings:                       append([]transferCloseChunkJitStatement(nil), self.settings[conn]...),
		})
	case strings.HasPrefix(sql, "set ") || strings.Contains(sql, "set_config("):
		if self.inTx[conn] {
			self.settings[conn] = append(self.settings[conn], statement)
		}
	}
	return ctx
}

func (self *transferCloseChunkJitCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Replays a chunk read under EXPLAIN ANALYZE with JIT forced for any cost and
// the given settings applied first, and returns the plan's JIT section, if
// any, and the jit setting the read ran with.
func explainTransferCloseChunkJit(t testing.TB, ctx context.Context, read transferCloseChunkJitStatement, settings []transferCloseChunkJitStatement) (jit any, jitSetting string) {
	t.Helper()
	var planJson string
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL jit_above_cost = 0`))
		for _, statement := range settings {
			server.RaisePgResult(tx.Exec(ctx, statement.sql, statement.args...))
		}
		server.Raise(tx.QueryRow(ctx, `SELECT current_setting('jit')`).Scan(&jitSetting))
		server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+read.sql, read.args...).Scan(&planJson))
	}, server.TxReadCommitted)
	var plans []map[string]any
	if err := json.Unmarshal([]byte(planJson), &plans); err != nil || len(plans) != 1 {
		t.Fatalf("unreadable plan %q: %v", planJson, err)
	}
	return plans[0]["JIT"], jitSetting
}

// On Main both chunk reads cost past the JIT thresholds, so every chunk
// compiled 51 to 55 functions, 42 to 48 ms of its 62 to 74 ms (2026-11-26). A
// zero threshold stands in for that cost here. Every chunk read of the rollup
// day's all-outcome walk and of the clock tail's terminal walk, first and
// resumed, must run in a transaction that turned JIT off before it, and the
// walks must still add up to the single-statement sums. Before the change the
// chunk read ran as an autocommit statement with JIT on.
func TestTransferCloseByteChunksRunWithoutJit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		end := startOfUtcDay(server.NowUtc())
		start := end.Add(-24 * time.Hour)
		contractCount := 2*transferCloseByteChunkSize + 333
		seedTransferCloseTieWindow(t, ctx, start, end, contractCount)

		var jitAvailable bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT pg_jit_available()`).Scan(&jitAvailable))
		})

		for _, terminalOnly := range []bool{false, true} {
			marker := transferAuditRollupDayMarker
			wantContractCount := int64(contractCount + 1)
			if terminalOnly {
				marker = clockUnrolledTailMarker
				wantContractCount = int64(contractCount - contractCount/5 + 1)
			}
			want := transferCloseStatementExpectedSum(ctx, start, end, terminalOnly)

			capture := newTransferCloseChunkJitCapture(marker)
			sum := TransferCloseByteSum{Start: start, End: end, TerminalOnly: terminalOnly}
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
				for chunk := 0; !sum.Complete; chunk += 1 {
					if 10 < chunk {
						t.Fatal("the walk did not complete")
					}
					sum.advance(ctx, marker, 1)
				}
			}()

			// the sums are those of the single statement
			if sum.ByteCount != want || sum.ContractCount != wantContractCount {
				t.Fatalf("terminal_only=%t walk sum=%d over %d contracts, want %d over %d",
					terminalOnly, sum.ByteCount, sum.ContractCount, want, wantContractCount)
			}

			// every chunk read, the first and the resumed ones
			if len(capture.reads) < 2 {
				t.Fatalf("terminal_only=%t captured %d chunk reads, want a first and a resumed one", terminalOnly, len(capture.reads))
			}
			for index, read := range capture.reads {
				jit, jitSetting := explainTransferCloseChunkJit(t, ctx, read.transferCloseChunkJitStatement, read.settings)
				if jit != nil {
					t.Fatalf("terminal_only=%t chunk read %d (in a transaction %t) compiled JIT functions: %v", terminalOnly, index, read.inTx, jit)
				}
				if jitSetting != "off" {
					t.Fatalf("terminal_only=%t chunk read %d (in a transaction %t) ran with jit=%s (jit available=%t)", terminalOnly, index, read.inTx, jitSetting, jitAvailable)
				}
				if !read.inTx {
					t.Fatalf("terminal_only=%t chunk read %d ran outside a transaction, so nothing could turn JIT off for it", terminalOnly, index)
				}
			}
			// the replay sees JIT where it runs: without the transaction's
			// settings the same read compiles
			if jitAvailable {
				if jit, _ := explainTransferCloseChunkJit(t, ctx, capture.reads[0].transferCloseChunkJitStatement, nil); jit == nil {
					t.Fatalf("terminal_only=%t control: the chunk read did not compile with jit on and a zero threshold", terminalOnly)
				}
			}
		}
	})
}
