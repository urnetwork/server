package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestNetEscrowSettlementCacheCustomGenericPointPlan(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE UNLOGGED TABLE settlement_cache_plan_escrow (LIKE transfer_escrow INCLUDING ALL) WITH (autovacuum_enabled=false)`))
			defer conn.Exec(context.Background(), `DROP TABLE settlement_cache_plan_escrow`)
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO settlement_cache_plan_escrow(contract_id,balance_id,balance_byte_count,settled)
			 SELECT md5('cache-plan-contract-'||n)::uuid,md5('cache-plan-balance-'||n)::uuid,1,true FROM generate_series(1,100000)n`))
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE settlement_cache_plan_escrow`))
			// The unsettled partial index remains falsely estimated empty. Also
			// distort the contract estimate before the three target rows arrive.
			server.RaisePgResult(conn.Exec(ctx, `SELECT pg_clear_attribute_stats('public','settlement_cache_plan_escrow','contract_id',false)`))
			var restored bool
			server.Raise(conn.QueryRow(ctx, `SELECT pg_restore_attribute_stats(
			 'schemaname','public','relname','settlement_cache_plan_escrow','attname','contract_id',
			 'inherited',false,'n_distinct',40::real,'null_frac',0::real,'avg_width',16::integer)`).Scan(&restored))
			if !restored {
				t.Fatal("synthetic planner distortion not installed")
			}
			contractId := server.NewId()
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO settlement_cache_plan_escrow(contract_id,balance_id,balance_byte_count,settled)
			 VALUES($1,$2,17,false),($1,$3,0,false),($1,$4,23,true)`, contractId, server.NewId(), server.NewId(), server.NewId()))
			query := strings.ReplaceAll(settlementReservationRowsSQL, "transfer_escrow", "settlement_cache_plan_escrow")
			server.RaisePgResult(conn.Exec(ctx, `PREPARE settlement_cache_rows(uuid) AS `+query))
			defer conn.Exec(context.Background(), `DEALLOCATE settlement_cache_rows`)
			defer conn.Exec(context.Background(), `RESET plan_cache_mode`)
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				var raw []byte
				server.Raise(conn.QueryRow(ctx, fmt.Sprintf(`EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE settlement_cache_rows('%s'::uuid)`, contractId)).Scan(&raw))
				var plans []map[string]any
				server.Raise(json.Unmarshal(raw, &plans))
				root := plans[0]["Plan"].(map[string]any)
				points := 0
				var visit func(map[string]any)
				visit = func(n map[string]any) {
					if cond, ok := n["Index Cond"].(string); ok {
						if !strings.Contains(cond, "contract_id =") || n["Actual Loops"].(float64) != 1 || n["Actual Rows"].(float64) != 3 {
							t.Fatalf("%s escaped exact contract boundary: %v", mode, n)
						}
						points++
					}
					if n["Node Type"] == "Seq Scan" || n["Node Type"] == "Gather" || n["Node Type"] == "Gather Merge" {
						t.Fatalf("%s planned global/parallel scan", mode)
					}
					if children, ok := n["Plans"].([]any); ok {
						for _, child := range children {
							visit(child.(map[string]any))
						}
					}
				}
				visit(root)
				buffers := root["Shared Hit Blocks"].(float64) + root["Shared Read Blocks"].(float64)
				if points != 1 || root["Actual Rows"].(float64) != 3 || buffers > 32 {
					t.Fatalf("%s points=%d rows=%v buffers=%v", mode, points, root["Actual Rows"], buffers)
				}
				t.Logf("%s contract_key_points=1 target_rows=3 buffers=%v unrelated_history_rows=100000", mode, buffers)
			}
		}, server.OptReadWrite())
	})
}
