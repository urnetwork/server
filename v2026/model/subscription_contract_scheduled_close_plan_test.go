// The deadline escrow read keeps a small plan under misestimated statistics.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The statement shape used before the bounded read. Kept only as the control
// that reproduces the production plan under the same retained statistics.
const deadlineEscrowRowsUnboundedSql = `SELECT e.balance_id,e.balance_byte_count,COALESCE(b.start_balance_byte_count,0),
	COALESCE(b.net_revenue_nano_cents,0),b.network_id,
	GREATEST(0,COALESCE(b.balance_byte_count,0)::numeric-COALESCE((SELECT sum(debit_byte_count)
	FROM transfer_debit_journal j WHERE j.balance_id=e.balance_id AND NOT j.applied),0))::bigint,
	e.settled,e.payout_byte_count,e.redis_reserved,
	(SELECT debit_byte_count FROM transfer_debit_journal j WHERE j.balance_id=e.balance_id AND j.contract_id=e.contract_id)
	FROM transfer_escrow e LEFT JOIN transfer_balance b USING(balance_id)
	WHERE e.contract_id=$1 ORDER BY b.end_time NULLS LAST,e.balance_id`

// Executed plan facts that add fixed overhead to one probe.
type deadlineEscrowPlan struct {
	totalCost    float64
	gathers      int
	jitFunctions int
}

func explainDeadlineEscrowRows(t testing.TB, ctx context.Context, sql string, contractId server.Id) (plan deadlineEscrowPlan) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		// The production planner settings that produced the parallel JIT plan.
		for _, setting := range []string{"jit=on", "jit_above_cost=100000", "max_parallel_workers_per_gather=4",
			"parallel_setup_cost=1000", "parallel_tuple_cost=0.1", "min_parallel_index_scan_size='512kB'", "random_page_cost=1.1"} {
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL "+setting))
		}
		var raw []byte
		server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, contractId).Scan(&raw))
		var explained []struct {
			Plan map[string]any `json:"Plan"`
			Jit  *struct {
				Functions int `json:"Functions"`
			} `json:"JIT"`
		}
		server.Raise(json.Unmarshal(raw, &explained))
		if len(explained) != 1 {
			t.Fatal("unexpected explain shape")
		}
		var visit func(node map[string]any)
		visit = func(node map[string]any) {
			if kind, _ := node["Node Type"].(string); kind == "Gather" || kind == "Gather Merge" {
				plan.gathers++
			}
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				if childNode, ok := child.(map[string]any); ok {
					visit(childNode)
				}
			}
		}
		visit(explained[0].Plan)
		plan.totalCost, _ = explained[0].Plan["Total Cost"].(float64)
		if explained[0].Jit != nil {
			plan.jitFunctions = explained[0].Jit.Functions
		}
	}, server.TxReadCommitted)
	return
}

// Production statistics estimate tens of thousands of escrow rows per contract
// id. The unbounded read then crosses the JIT cost threshold, and on the full
// table also plans parallel workers, for a one-row probe held under the grant
// owner. The bounded read does neither and returns the same rows in order.
func TestDeadlineEscrowReadStaysSmallUnderMisestimatedStatistics(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "synthetic-escrow-plan-payer", server.NewId())
		server.Raise(AddBasicTransferBalance(ctx, networkId, 1000, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(time.Hour)))
		server.Raise(AddBasicTransferBalance(ctx, networkId, 1000, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(2*time.Hour)))
		balances := GetActiveTransferBalances(ctx, networkId)
		if len(balances) != 2 {
			t.Fatal("synthetic payer did not retain two grants")
		}
		contractId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			// Retained settled history gives the table the volume behind the estimate.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count,settled)
				SELECT gen_random_uuid(),$1,0,true FROM generate_series(1,120000)`, balances[0].BalanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count)
				VALUES ($1,$2,30),($1,$3,40)`, contractId, balances[0].BalanceId, balances[1].BalanceId))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE transfer_escrow ALTER COLUMN contract_id SET (n_distinct=4)`))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_escrow`))
		})
		control := explainDeadlineEscrowRows(t, ctx, deadlineEscrowRowsUnboundedSql, contractId)
		if control.jitFunctions == 0 || control.totalCost < 100000 {
			t.Fatalf("control did not reproduce the compiled plan: jit=%d gathers=%d cost=%.0f", control.jitFunctions, control.gathers, control.totalCost)
		}
		bounded := explainDeadlineEscrowRows(t, ctx, deadlineEscrowRowsSql, contractId)
		if bounded.jitFunctions != 0 || bounded.gathers != 0 || 100000 <= bounded.totalCost {
			t.Fatalf("deadline escrow read compiled or planned workers: jit=%d gathers=%d cost=%.0f", bounded.jitFunctions, bounded.gathers, bounded.totalCost)
		}
		read := func(sql string) (rows string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT array_to_string(array_agg(r::text),';') FROM (`+sql+`) AS r`, contractId).Scan(&rows))
			})
			return
		}
		if control, bounded := read(deadlineEscrowRowsUnboundedSql), read(deadlineEscrowRowsSql); control != bounded {
			t.Fatal("bounded escrow read changed rows or funding order", control, bounded)
		}
	})
}
