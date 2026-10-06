package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The complete migrated catalog intentionally has false-zero estimates for
// global open/outcome indexes. A balance census must still look up only the
// contracts referenced by that balance's nonzero, unsettled escrow rows.
func TestNetEscrowContractLookupRemainsPointScopedWithFalseZeroStats(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		seedFalseZeroOpenContractStats(t, ctx, server.NewId(), server.NewId(), server.NewId(), server.NewId())
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 payer_network_id,transfer_byte_count,dispute,outcome,close_time)
				SELECT md5('census-point-'||n)::uuid,$1,$2,$3,$4,$1,
				 CASE WHEN n BETWEEN 13 AND 16 THEN 0 ELSE 1 END,n IN (7,8),
				 CASE WHEN n BETWEEN 9 AND 12 THEN 'canceled' ELSE NULL END,
				 CASE WHEN n BETWEEN 9 AND 12 THEN now() ELSE NULL END
				FROM generate_series(1,18) n WHERE n<>17`,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow
				(contract_id,balance_id,balance_byte_count,settled)
				SELECT md5('census-point-'||n)::uuid,$1,
				 CASE WHEN n BETWEEN 13 AND 16 THEN 0 ELSE 1 END,n=18
				FROM generate_series(1,18) n`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_escrow`))
		})
		server.Db(ctx, func(conn server.PgConn) {
			assertOpenPlanStatsAreFalseZero(t, ctx, conn)
			before := strings.Replace(netEscrowReservationPageSQL, `INNER JOIN LATERAL (
            SELECT outcome FROM transfer_contract
            WHERE contract_id = selected_escrow.contract_id
            OFFSET 0
        ) AS transfer_contract ON transfer_contract.outcome IS NULL`, `INNER JOIN transfer_contract ON
            transfer_contract.contract_id = selected_escrow.contract_id
        WHERE transfer_contract.outcome IS NULL`, 1)
			if before == netEscrowReservationPageSQL {
				t.Fatal("historical comparison did not bind runtime census")
			}
			for _, query := range []struct{ name, sql string }{
				{"census_before", before}, {"census_runtime", netEscrowReservationPageSQL},
			} {
				server.RaisePgResult(conn.Exec(ctx, `PREPARE `+query.name+` AS `+query.sql))
				defer conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE `+query.name)
			}
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				server.RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode=`+mode))
				for _, name := range []string{"census_before", "census_runtime"} {
					arguments := fmt.Sprintf("('{%s}'::uuid[])", f.balanceId)
					var raw []byte
					server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) EXECUTE `+name+arguments).Scan(&raw))
					var plans []map[string]any
					server.Raise(json.Unmarshal(raw, &plans))
					root := plans[0]["Plan"].(map[string]any)
					contractRows, contractLookups := 0, 0
					pointScoped := true
					var inspect func(map[string]any)
					inspect = func(node map[string]any) {
						if node["Relation Name"] == "transfer_contract" {
							rows := node["Actual Rows"].(float64)
							if removed, ok := node["Rows Removed by Filter"].(float64); ok {
								rows += removed
							}
							loops := int(node["Actual Loops"].(float64))
							contractRows += int(rows * float64(loops))
							contractLookups += loops
							condition, _ := node["Index Cond"].(string)
							if !strings.Contains(condition, "contract_id =") {
								pointScoped = false
							}
						}
						if children, ok := node["Plans"].([]any); ok {
							for _, child := range children {
								inspect(child.(map[string]any))
							}
						}
					}
					inspect(root)
					var balanceId server.Id
					var revision, reserved int64
					var endTime *time.Time
					server.Raise(conn.QueryRow(ctx, `EXECUTE `+name+arguments).Scan(&balanceId, &revision, &reserved, &endTime))
					if balanceId != f.balanceId || reserved != 8 || endTime == nil {
						t.Fatalf("%s changed unresolved/disputed/closed/orphan/settled/zero authority: reserved=%d", name, reserved)
					}
					t.Logf("%s %s point_scoped=%t contract_rows=%d contract_loops=%d buffers=%v execution_ms=%v",
						mode, name, pointScoped, contractRows, contractLookups, root["Shared Hit Blocks"], plans[0]["Execution Time"])
					if name == "census_before" {
						if pointScoped || contractRows < 2000 {
							t.Fatalf("fixture did not reproduce unrelated contract scan: %s", raw)
						}
					} else if !pointScoped || contractLookups != 13 {
						t.Fatalf("runtime census escaped contract point bounds: %s", raw)
					}
				}
			}
		})
	})
}
