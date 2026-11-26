// A close alone reads the pending journal of its own grants only, not the leaf
// that holds another grant's backlog.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Debits of one grant's backlog in one day leaf of the journal.
const deadlineBacklogTestRows = 20000

// A statement exactly as the close sent it.
type deadlineBacklogStatement struct {
	sql  string
	args []any
}

// A journal read of the close, with the settings its transaction applied
// before it.
type deadlineBacklogRead struct {
	deadlineBacklogStatement
	settings []deadlineBacklogStatement
}

// Records each statement of the real close that reads the debit journal, and
// per connection the settings of its open transaction.
type deadlineBacklogCapture struct {
	stateLock sync.Mutex
	settings  map[*pgx.Conn][]deadlineBacklogStatement
	reads     []deadlineBacklogRead
}

func (self *deadlineBacklogCapture) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sql := strings.ToLower(strings.TrimSpace(data.SQL))
	statement := deadlineBacklogStatement{sql: data.SQL, args: append([]any(nil), data.Args...)}
	switch {
	case strings.HasPrefix(sql, "begin") || strings.HasPrefix(sql, "commit") || strings.HasPrefix(sql, "rollback"):
		self.settings[conn] = nil
	case strings.HasPrefix(sql, "set ") || strings.Contains(sql, "set_config("):
		self.settings[conn] = append(self.settings[conn], statement)
	case strings.HasPrefix(sql, "select") && strings.Contains(sql, "transfer_debit_journal"):
		self.reads = append(self.reads, deadlineBacklogRead{
			deadlineBacklogStatement: statement,
			settings:                 append([]deadlineBacklogStatement(nil), self.settings[conn]...),
		})
	}
	return ctx
}

func (self *deadlineBacklogCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// A plan node of EXPLAIN (ANALYZE, FORMAT JSON).
type deadlineBacklogPlanNode struct {
	NodeType string                    `json:"Node Type"`
	Relation string                    `json:"Relation Name"`
	Rows     float64                   `json:"Actual Rows"`
	Loops    float64                   `json:"Actual Loops"`
	Removed  float64                   `json:"Rows Removed by Filter"`
	Recheck  float64                   `json:"Rows Removed by Index Recheck"`
	Plans    []deadlineBacklogPlanNode `json:"Plans"`
}

// The rows of leaf a plan examined, returned or filtered out, over all loops,
// and its scans of the leaf by node type.
func deadlineBacklogLeafRows(raw []byte, leaf string) (examined float64, scans []string) {
	var plans []struct{ Plan deadlineBacklogPlanNode }
	server.Raise(json.Unmarshal(raw, &plans))
	var visit func(node deadlineBacklogPlanNode)
	visit = func(node deadlineBacklogPlanNode) {
		if node.Relation == leaf {
			examined += node.Loops * (node.Rows + node.Removed + node.Recheck)
			scans = append(scans, fmt.Sprintf("%s x%.0f", node.NodeType, node.Loops))
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	for _, plan := range plans {
		visit(plan.Plan)
	}
	return
}

// Replays a statement under EXPLAIN ANALYZE with the given settings applied
// first, and returns the leaf rows it examined.
func explainDeadlineBacklogLeafRows(t testing.TB, ctx context.Context, read deadlineBacklogStatement, settings []deadlineBacklogStatement, leaf string) (examined float64, scans []string) {
	t.Helper()
	var raw []byte
	server.Tx(ctx, func(tx server.PgTx) {
		for _, statement := range settings {
			server.RaisePgResult(tx.Exec(ctx, statement.sql, statement.args...))
		}
		server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+read.sql, read.args...).Scan(&raw))
	}, server.TxReadCommitted)
	return deadlineBacklogLeafRows(raw, leaf)
}

// On 2026-11-26 a day leaf of the journal held one grant's backlog, so it
// analyzed to n_distinct=1 for balance_id. A close alone summed each of its
// grants' pending debits in a subquery correlated to its escrow row, which the
// planner planned for no grant in particular: with one distinct grant in the
// leaf it expected every row of the leaf and seq-scanned the whole leaf once per
// escrow row, for grants with no debit in it. Here a contract over two grants
// of its payer closes at its deadline next to another grant's backlog of
// 20,000 pending debits in today's leaf, with the leaf's statistics pinned at
// n_distinct=1. Every journal read the close sends, replayed under EXPLAIN
// ANALYZE, must examine at most a hundredth of that leaf. Before the change the
// escrow read seq-scanned the leaf once per escrow row and examined it twice.
// A prepared pending read of a grant outside the backlog keeps its custom plan
// after repeated use, so the plan cache does not bring the seq scan back.
func TestDeadlineCloseAloneDoesNotScanAnotherGrantsBacklogLeaf(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		server.Testing_PartitionTransferDebitJournal(ctx, server.TestingTransferDebitJournalCutoverDay(server.NowUtc()))

		// a contract over both grants of its payer: 1,000 bytes each, 1,500
		// escrowed
		f := newNetEscrowOrderingTestFixture(t, ctx)
		server.Raise(AddBasicTransferBalance(ctx, f.sourceNetworkId, 1000, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(2*time.Hour)))
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 1500)
		server.RunPosts(ctx, posts...)
		var escrowGrants int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE contract_id=$1`, contract.ContractId).Scan(&escrowGrants))
		})
		if escrowGrants != 2 {
			t.Fatalf("the contract has %d escrow rows, want one per grant of its payer", escrowGrants)
		}
		server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 1200, true))

		// another grant's backlog of pending debits of today's contracts
		backlogBalanceId := server.NewId()
		backlogContractIds := make([]server.Id, deadlineBacklogTestRows)
		for index := range backlogContractIds {
			backlogContractIds[index] = server.NewId()
		}
		var leaf string
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal (contract_id,balance_id,debit_byte_count,shard)
				SELECT contract_id,$2,100,$3 FROM unnest($1::uuid[]) AS backlog(contract_id)`,
				backlogContractIds, backlogBalanceId, transferDebitShard(backlogBalanceId)))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT DISTINCT tableoid::regclass::text FROM transfer_debit_journal WHERE balance_id=$1`, backlogBalanceId).Scan(&leaf))
			identifier := pgx.Identifier{leaf}.Sanitize()
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE `+identifier+` SET (autovacuum_enabled = false)`))
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE `+identifier+` ALTER COLUMN balance_id SET (n_distinct = 1)`))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE `+identifier))
			var nDistinct float64
			server.Raise(conn.QueryRow(ctx, `SELECT n_distinct FROM pg_stats WHERE tablename=$1 AND attname='balance_id'`, leaf).Scan(&nDistinct))
			if nDistinct != 1 {
				t.Fatalf("the backlog leaf %s analyzed to n_distinct=%v for balance_id, want 1", leaf, nDistinct)
			}
		})
		if !strings.HasPrefix(leaf, "transfer_debit_journal_p") {
			t.Fatalf("the backlog is in %s, want a day leaf of the journal", leaf)
		}

		capture := &deadlineBacklogCapture{settings: map[*pgx.Conn][]deadlineBacklogStatement{}}
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
			result, err := ReconcileContractAtDeadline(ctx, contract.ContractId, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil || result == nil || result.AlreadyClosed || result.Missing || result.Charged != 1200 {
				t.Fatal("the deadline did not close the open contract with its usage charged", result, err)
			}
		}()
		if len(capture.reads) == 0 {
			t.Fatal("the close sent no journal read")
		}
		var examined float64
		lines := []string{}
		for _, read := range capture.reads {
			readExamined, scans := explainDeadlineBacklogLeafRows(t, ctx, read.deadlineBacklogStatement, read.settings, leaf)
			examined += readExamined
			lines = append(lines, fmt.Sprintf("%.0f rows %v: %.80s", readExamined, scans, strings.Join(strings.Fields(read.sql), " ")))
		}
		t.Logf("journal reads of the close:\n%s", strings.Join(lines, "\n"))
		if deadlineBacklogTestRows/100 < examined {
			t.Fatalf("the close examined %.0f rows of the backlog leaf %s for grants with no debit in it, want at most %d:\n%s",
				examined, leaf, deadlineBacklogTestRows/100, strings.Join(lines, "\n"))
		}

		// a pending read of a grant outside the backlog, prepared and used
		// repeatedly as the pool's statement cache does, stays a custom plan;
		// the backlog's own grant reads the leaf it fills (the control)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `PREPARE deadline_backlog_pending(uuid) AS `+deadlinePendingDebitSql))
			defer func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_, _ = conn.Exec(cleanup, `DEALLOCATE deadline_backlog_pending`)
			}()
			for range 12 {
				server.RaisePgResult(conn.Exec(ctx, `EXECUTE deadline_backlog_pending('`+f.balanceId.String()+`')`))
			}
			explainExecute := func(balanceId server.Id) (float64, []string) {
				var raw []byte
				server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE deadline_backlog_pending('`+balanceId.String()+`')`, pgx.QueryExecModeSimpleProtocol).Scan(&raw))
				return deadlineBacklogLeafRows(raw, leaf)
			}
			if rows, scans := explainExecute(f.balanceId); deadlineBacklogTestRows/100 < rows {
				t.Fatalf("after repeated use the prepared pending read of a grant outside the backlog examined %.0f rows of the leaf (%v)", rows, scans)
			}
			if rows, scans := explainExecute(backlogBalanceId); rows < deadlineBacklogTestRows {
				t.Fatalf("control: the backlog grant's pending read examined %.0f rows of the leaf (%v), want all %d", rows, scans, deadlineBacklogTestRows)
			}
		})
	})
}
