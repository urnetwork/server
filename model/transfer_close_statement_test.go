package model

// Root cause of the Main xmin horizon hold: the daily transfer audit rollup and
// the clock backfill summed a whole window of transfer_contract x
// contract_close in one statement, and the rollup ran it inside its
// REPEATABLE READ write transaction, so one snapshot stayed open for the whole
// scan (5 to 58 minutes for one day on Main). These tests observe every
// statement the real code paths issue and require each statement that reads
// both tables to run outside an explicit transaction and to read at most one
// bounded chunk of contracts, however many contracts the window holds.

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

// The most contracts one statement may read. The seeded windows hold several
// times this, so a statement that reads a whole window exceeds it.
const transferCloseStatementContractLimit = 2000

// Contracts seeded inside the observed window: two full chunks and a partial.
const transferCloseStatementWindowCount = 5037

type transferCloseStatementsKey struct{}

// A statement issued under the observed context, with its arguments and the
// connection's transaction status ('I' idle, 'T' in a transaction) when it
// started.
type transferCloseStatement struct {
	sql      string
	args     []any
	txStatus byte
}

// Records the statements issued under its marked context, so other users of
// the fixture pools cannot add statements. Safe for concurrent use.
type transferCloseStatements struct {
	stateLock  sync.Mutex
	statements []transferCloseStatement
}

func (self *transferCloseStatements) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if marker, _ := ctx.Value(transferCloseStatementsKey{}).(*transferCloseStatements); marker != self {
		return ctx
	}
	statement := transferCloseStatement{
		sql:      data.SQL,
		args:     append([]any(nil), data.Args...),
		txStatus: conn.PgConn().TxStatus(),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.statements = append(self.statements, statement)
	}()
	return ctx
}

func (self *transferCloseStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// A plan node of EXPLAIN (FORMAT JSON).
type transferCloseStatementNode struct {
	Relation string                       `json:"Relation Name"`
	Rows     float64                      `json:"Actual Rows"`
	Loops    float64                      `json:"Actual Loops"`
	Removed  float64                      `json:"Rows Removed by Filter"`
	Recheck  float64                      `json:"Rows Removed by Index Recheck"`
	Plans    []transferCloseStatementNode `json:"Plans"`
}

// Seeds transferCloseStatementWindowCount closed contracts in [start, end):
// groups of seven share a close time so chunk boundaries split ties, every
// thirteenth outcome is NULL, every eleventh is canceled and every
// seventeenth has no destination close. Also seeds the window edges and
// unrelated open contracts and closes, so a whole-table read is far larger
// than one chunk. Settled rows carry an empty synthetic usage snapshot, which
// the settlement guard requires.
func seedTransferCloseStatementWindow(t testing.TB, ctx context.Context, start time.Time, end time.Time) {
	t.Helper()
	tieStep := end.Sub(start) / (transferCloseStatementWindowCount/7 + 2)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time, outcome, provider_usage
			)
			SELECT
				lpad(to_hex(1000000 + g), 32, '0')::uuid,
				lpad('a1', 32, '0')::uuid, lpad('a2', 32, '0')::uuid,
				lpad('a3', 32, '0')::uuid, lpad('a4', 32, '0')::uuid,
				1000, $1::timestamp - interval '1 hour',
				$1::timestamp + (g / 7) * $3::bigint * interval '1 microsecond',
				CASE WHEN g % 13 = 0 THEN NULL WHEN g % 11 = 0 THEN 'canceled' ELSE 'settled' END,
				'{"version":1,"byte_count":0,"providers":[]}'::jsonb
			FROM generate_series(1, $2::int) g
			`,
			start,
			transferCloseStatementWindowCount,
			tieStep.Microseconds(),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
			SELECT lpad(to_hex(1000000 + g), 32, '0')::uuid, $1::timestamp, 'destination', g % 1000 + 1
			FROM generate_series(1, $2::int) g
			WHERE g % 17 <> 0
			UNION ALL
			SELECT lpad(to_hex(1000000 + g), 32, '0')::uuid, $1::timestamp, 'source', 999
			FROM generate_series(1, $2::int) g
			`,
			start,
			transferCloseStatementWindowCount,
		))
		// window edges: the minimum id exactly at the start is inside; just
		// before the start and exactly at the end are outside
		for _, edge := range []struct {
			contractId string
			closeTime  time.Time
			byteCount  int64
		}{
			{contractId: "00000000-0000-0000-0000-000000000000", closeTime: start, byteCount: 70001},
			{contractId: "00000000-0000-0000-0000-0000000000e1", closeTime: start.Add(-time.Microsecond), byteCount: 70003},
			{contractId: "00000000-0000-0000-0000-0000000000e2", closeTime: end.Add(-time.Microsecond), byteCount: 70007},
			{contractId: "00000000-0000-0000-0000-0000000000e3", closeTime: end, byteCount: 70009},
		} {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id, destination_network_id, destination_id,
					transfer_byte_count, create_time, close_time, outcome, provider_usage
				)
				VALUES ($1, $3, $3, $3, $3, 1000, $2::timestamp - interval '1 hour', $2, 'settled', '{"version":1,"byte_count":0,"providers":[]}'::jsonb)
				`,
				edge.contractId,
				edge.closeTime,
				server.NewId(),
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
				VALUES ($1, $2, 'destination', $3)
				`,
				edge.contractId,
				edge.closeTime,
				edge.byteCount,
			))
		}
		// open contracts are in neither close-time index; their checkpoint
		// closes are unrelated report history
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time, outcome
			)
			SELECT
				lpad(to_hex(3000000 + g), 32, '0')::uuid,
				lpad('b1', 32, '0')::uuid, lpad('b2', 32, '0')::uuid,
				lpad('b3', 32, '0')::uuid, lpad('b4', 32, '0')::uuid,
				1000, $1::timestamp, NULL, NULL
			FROM generate_series(1, 60000) g
			`,
			start,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count, checkpoint)
			SELECT lpad(to_hex(3000000 + g), 32, '0')::uuid, $1::timestamp, 'source', 555, true
			FROM generate_series(1, 60000) g
			`,
			start,
		))
	})
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_contract; ANALYZE contract_close`))
	})
}

// Runs body with every statement it issues recorded, and returns the recorded
// statements that read both transfer_contract and contract_close.
func observeTransferCloseStatements(t testing.TB, ctx context.Context, body func(context.Context)) []transferCloseStatement {
	t.Helper()
	statements := &transferCloseStatements{}
	scope, err := server.NewTestPgQueryScope(ctx, statements)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		body(context.WithValue(ctx, transferCloseStatementsKey{}, statements))
	}()

	reads := []transferCloseStatement{}
	for _, statement := range statements.statements {
		normalized := strings.ToLower(statement.sql)
		if strings.Contains(normalized, "transfer_contract") && strings.Contains(normalized, "contract_close") {
			reads = append(reads, statement)
		}
	}
	return reads
}

// Requires each statement to run outside an explicit transaction, so its
// snapshot ends with the statement, and to read at most one chunk of
// transfer_contract rows by EXPLAIN ANALYZE of the recorded statement and
// arguments.
func assertTransferCloseStatementsBounded(t testing.TB, ctx context.Context, reads []transferCloseStatement) {
	t.Helper()
	if len(reads) == 0 {
		t.Fatal("observed no statement reading transfer_contract and contract_close")
	}
	for index, statement := range reads {
		if statement.txStatus != 'I' {
			t.Errorf("statement %d ran inside an explicit transaction (status %q), so its snapshot outlived the statement", index, statement.txStatus)
		}

		var contractReads float64
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
			server.Raise(err)
			defer func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_ = tx.Rollback(cleanup)
			}()
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL max_parallel_workers_per_gather = 0; SET LOCAL jit = off`))
			var raw []byte
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, TIMING OFF, FORMAT JSON) `+statement.sql, statement.args...).Scan(&raw))
			var plans []struct{ Plan transferCloseStatementNode }
			server.Raise(json.Unmarshal(raw, &plans))
			var inspect func(transferCloseStatementNode)
			inspect = func(node transferCloseStatementNode) {
				if node.Relation == "transfer_contract" {
					contractReads += node.Loops * (node.Rows + node.Removed + node.Recheck)
				}
				for _, child := range node.Plans {
					inspect(child)
				}
			}
			for _, plan := range plans {
				inspect(plan.Plan)
			}
		})
		t.Logf("statement %d: transaction status %q, %.0f transfer_contract rows read", index, statement.txStatus, contractReads)
		// a merge of two index branches may read one look-ahead key per branch
		if transferCloseStatementContractLimit+2 < contractReads {
			t.Errorf("statement %d read %.0f transfer_contract rows, more than one chunk of %d", index, contractReads, transferCloseStatementContractLimit)
		}
	}
}

// The single-statement sum the bounded reads must equal.
func transferCloseStatementExpectedSum(ctx context.Context, start time.Time, end time.Time, terminalOnly bool) ByteCount {
	var byteCount ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT COALESCE(SUM(contract_close.used_transfer_byte_count), 0)::bigint
			FROM transfer_contract
			INNER JOIN contract_close ON
				contract_close.contract_id = transfer_contract.contract_id AND
				contract_close.party = 'destination'
			WHERE
				$1 <= transfer_contract.close_time AND
				transfer_contract.close_time < $2 AND
				(NOT $3 OR transfer_contract.outcome IS NOT NULL)
			`,
			start,
			end,
			terminalOnly,
		).Scan(&byteCount))
	})
	return byteCount
}

func TestRollupTransferAuditReadsEachDayInBoundedStatements(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		dayEnd := startOfUtcDay(server.NowUtc())
		dayStart := dayEnd.Add(-24 * time.Hour)
		seedTransferCloseStatementWindow(t, ctx, dayStart, dayEnd)
		want := transferCloseStatementExpectedSum(ctx, dayStart, dayEnd, false)

		reads := observeTransferCloseStatements(t, ctx, func(observedCtx context.Context) {
			if dayCount := RollupTransferAuditEvents(observedCtx, dayStart, dayEnd); dayCount != 1 {
				t.Fatalf("rollup days=%d, want 1", dayCount)
			}
		})
		assertTransferCloseStatementsBounded(t, ctx, reads)

		var got ByteCount
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
				SELECT transfer_byte_count FROM audit_contract_event
				WHERE event_details = $1 AND $2 <= event_time AND event_time < $3
				`,
				AuditEventDetailsTransferRollup,
				dayStart,
				dayEnd,
			).Scan(&got))
		})
		if got != want {
			t.Fatalf("rollup day bytes=%d, want the single-statement sum %d", got, want)
		}
	})
}

func TestClockBackfillReadsTailInBoundedStatements(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		end := server.NowUtc().Add(-time.Minute)
		start := end.Add(-48 * time.Hour)
		seedTransferCloseStatementWindow(t, ctx, start, end)
		// no rollup rows exist, so the raw tail starts at the block-9 start
		want := transferCloseStatementExpectedSum(ctx, ClockSinceTime, server.NowUtc(), true)

		var got ByteCount
		reads := observeTransferCloseStatements(t, ctx, func(observedCtx context.Context) {
			got = clockBackfillCandidate(observedCtx)
		})
		assertTransferCloseStatementsBounded(t, ctx, reads)
		if got != want {
			t.Fatalf("clock candidate=%d, want the single-statement sum %d", got, want)
		}
	})
}
