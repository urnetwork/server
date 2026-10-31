// A whole-window usage read is one block-ordered pass over the contract heap,
// whatever the planner would otherwise prefer for a correlated close-time range.
package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

type stWholeWindowPlanNode struct {
	Type, Relation, Index, Cte string
	Rows, Loops                float64
	TempBlocks                 float64
	HashBatches                float64
	// the indexes of a bitmap heap scan's own bitmap
	Bitmaps []string
}

func stWholeWindowPlanNodes(value any, nodes []stWholeWindowPlanNode) []stWholeWindowPlanNode {
	switch node := value.(type) {
	case map[string]any:
		if nodeType, ok := node["Node Type"].(string); ok {
			planNode := stWholeWindowPlanNode{Type: nodeType}
			planNode.Relation, _ = node["Relation Name"].(string)
			planNode.Index, _ = node["Index Name"].(string)
			planNode.Cte, _ = node["CTE Name"].(string)
			planNode.Rows, _ = node["Actual Rows"].(float64)
			planNode.Loops, _ = node["Actual Loops"].(float64)
			written, _ := node["Temp Written Blocks"].(float64)
			read, _ := node["Temp Read Blocks"].(float64)
			planNode.TempBlocks = written + read
			planNode.HashBatches, _ = node["Hash Batches"].(float64)
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				if child, ok := child.(map[string]any); ok && child["Node Type"] == "Bitmap Index Scan" {
					index, _ := child["Index Name"].(string)
					planNode.Bitmaps = append(planNode.Bitmaps, index)
				}
			}
			nodes = append(nodes, planNode)
		}
		for _, child := range node {
			nodes = stWholeWindowPlanNodes(child, nodes)
		}
	case []any:
		for _, child := range node {
			nodes = stWholeWindowPlanNodes(child, nodes)
		}
	}
	return nodes
}

// Main's planner costs (2026-10-30), under which its planner reads a 7-day
// window in close-time index order.
var stWholeWindowPlanMainCosts = []string{
	`SET LOCAL random_page_cost = 1.1`,
	`SET LOCAL effective_cache_size = '768GB'`,
}

func stWholeWindowPlan(t testing.TB, ctx context.Context, whole bool, start, end time.Time) ([]stWholeWindowPlanNode, string) {
	t.Helper()
	var raw []byte
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		server.Raise(err)
		defer tx.Rollback(context.WithoutCancel(ctx))
		for _, setting := range stWholeWindowPlanMainCosts {
			server.RaisePgResult(tx.Exec(ctx, setting))
		}
		if whole {
			server.Raise(stEpochProviderUsageWholeWindowPlanInTx(ctx, tx))
		}
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+stEpochProviderUsageSql, start, end).Scan(&raw))
	})
	var plan any
	server.Raise(json.Unmarshal(raw, &plan))
	return stWholeWindowPlanNodes(plan, nil), string(raw)
}

// Under Main's costs the planner reads one correlated hour of a larger history
// by close-time index, a random heap fetch per row. The whole-window plan reads
// it by bitmap:
// each heap page once, in block order. Nothing materializes, sorts the window,
// spills, runs in parallel or touches a close report, and the archive hash
// stays in one batch.
func TestStEpochUsageWholeWindowPlan(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		// eight hours of contracts closed every 100 ms between distinct
		// endpoints, so close time follows heap order as it does on Main
		const hours, perHour = 8, 36000
		provider, network := server.NewId(), server.NewId()
		snapshot := &contractUsageSnapshot{Version: 1, ByteCount: 11, Providers: []contractProviderUsage{{ClientId: provider, NetworkId: network, ByteCount: 11}}}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_contract
					(contract_id,source_id,source_network_id,destination_id,destination_network_id,
					 transfer_byte_count,create_time,outcome,close_time,provider_usage,usage_origin_is_source)
				SELECT md5('plan-'||n)::uuid,md5('plan-source-'||n)::uuid,$2,md5('plan-destination-'||n)::uuid,$3,
					1000000,$1::timestamp-interval '1 hour','settled',$1::timestamp+n*interval '100 milliseconds',$4,true
				FROM generate_series(0,$5::int-1) n
			`, base, server.NewId(), network, snapshot, hours*perHour))
		})
		start, end := base.Add(5*time.Hour), base.Add(6*time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id IN (md5('plan-180001')::uuid,md5('plan-190002')::uuid,md5('plan-200003')::uuid)`))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `VACUUM ANALYZE transfer_contract`))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_provider_usage_archive`))
		})

		usual, usualRaw := stWholeWindowPlan(t, ctx, false, start, end)
		indexOrdered := false
		for _, node := range usual {
			indexOrdered = indexOrdered || (node.Type == "Index Scan" && node.Index == "transfer_contract_closed_usage" && node.Rows > 0)
		}
		if !indexOrdered {
			t.Fatalf("the usual plan no longer reads the window in close-time index order, so this test cannot show the whole-window plan avoids it: %s", usualRaw)
		}

		nodes, raw := stWholeWindowPlan(t, ctx, true, start, end)
		windowRows := 0.0
		windowBitmaps := []string{}
		for _, node := range nodes {
			switch {
			case node.Relation == "contract_close_report_evidence" || node.Relation == "contract_close_report":
				t.Fatalf("the whole-window read touched a close report: %s", raw)
			case node.Relation == "transfer_contract" && node.Type != "Bitmap Heap Scan":
				t.Fatalf("the whole-window read fetched contracts by %s, not by bitmap: %s", node.Type, raw)
			case node.Type == "Materialize" || node.Type == "Gather" || node.Type == "Gather Merge" || (node.Type == "CTE Scan" && node.Cte != "missing_time"):
				t.Fatalf("the whole-window read has a %s node: %s", node.Type, raw)
			case node.TempBlocks > 0:
				t.Fatalf("the whole-window read spilled %v temp blocks in %s: %s", node.TempBlocks, node.Type, raw)
			case node.Type == "Hash" && node.HashBatches != 1:
				t.Fatalf("the archive hash ran in %v batches: %s", node.HashBatches, raw)
			case node.Type == "Sort" && node.Rows != 0:
				// only the undated probes sort, and this window has none
				t.Fatalf("the whole-window read sorted %v rows: %s", node.Rows, raw)
			}
			if node.Type == "Bitmap Heap Scan" && node.Relation == "transfer_contract" && 0 < node.Rows {
				windowRows += node.Rows * node.Loops
				windowBitmaps = append(windowBitmaps, node.Bitmaps...)
			}
		}
		if windowRows != perHour-3 || len(windowBitmaps) != 1 || windowBitmaps[0] != "transfer_contract_closed_usage" {
			t.Fatalf("the whole-window read lost its index or rows (%v, %v rows): %s", windowBitmaps, windowRows, raw)
		}
	})
}
