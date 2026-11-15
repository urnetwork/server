package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

type reliabilityObservationPlanNode struct {
	Type        string                           `json:"Node Type"`
	Relation    string                           `json:"Relation Name"`
	ActualRows  float64                          `json:"Actual Rows"`
	ActualLoops float64                          `json:"Actual Loops"`
	Filtered    float64                          `json:"Rows Removed by Filter"`
	Rechecked   float64                          `json:"Rows Removed by Index Recheck"`
	Hits        float64                          `json:"Shared Hit Blocks"`
	Reads       float64                          `json:"Shared Read Blocks"`
	Plans       []reliabilityObservationPlanNode `json:"Plans"`
}

func reliabilityObservationExamined(node reliabilityObservationPlanNode) float64 {
	rows := 0.0
	if node.Relation == "client_reliability" {
		rows = (node.ActualRows + node.Filtered + node.Rechecked) * node.ActualLoops
	}
	for _, child := range node.Plans {
		rows += reliabilityObservationExamined(child)
	}
	return rows
}

// A two-minute rolling delta must read those minutes, including invalid rows,
// rather than the stored history. Both prepared-plan modes use the production
// covering-index shape; no planner method is disabled or hinted.
func TestReliabilityObservationMillionRowBoundedPlans(t *testing.T) {
	if testing.Short() {
		t.Skip("one-million-row reliability rolling plan control")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO client_reliability(block_number,client_address_hash,network_id,client_id,
					connection_established_count,provide_enabled_count,receive_message_count,valid)
				SELECT block,decode(md5(provider::text),'hex'),
					md5('synthetic-network')::uuid,md5(provider::text)::uuid,1,1,
					CASE WHEN provider%4=0 OR (provider%4=1 AND block%2=0) THEN 0 ELSE 1 END,
					client_reliability_valid(0,1,1,0,
						CASE WHEN provider%4=0 OR (provider%4=1 AND block%2=0) THEN 0 ELSE 1 END,$1)
				FROM generate_series(1,100) AS block CROSS JOIN generate_series(1,10000) AS provider`,
				ReliabilityAllowDisconnectCountPerBlock))
			var count, valid, invalid int64
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*),COUNT(*) FILTER(WHERE valid),
				COUNT(*) FILTER(WHERE NOT valid) FROM client_reliability`).Scan(&count, &valid, &invalid))
			if count != 1000000 || valid != 625000 || invalid != 375000 {
				t.Fatalf("invalid volume fixture: rows=%d valid=%d invalid=%d", count, valid, invalid)
			}
			server.RaisePgResult(conn.Exec(ctx, `CREATE INDEX reliability_observation_plan_covering
				ON client_reliability `+clientReliabilitySecondaryIndexShape))
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) client_reliability`))
		})
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode))
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='30s'`))
				for _, control := range []struct {
					name     string
					query    string
					wantRows float64
					bounded  bool
				}{
					{"valid_only_baseline", reliabilityRunningAggSql, 7500, true},
					{"observed", reliabilityRunningObservedAggSql, 10000, true},
					// This counterfactual removes only the invalid-branch time
					// bounds. It still returns the same clients and must fail
					// the work oracle, so output cardinality cannot fake safety.
					{"unbounded_invalid", strings.Replace(reliabilityRunningObservedAggSql,
						"valid=false AND $1<=block_number AND block_number<$2", "valid=false", 1), 10000, false},
				} {
					statement := `"reliability_plan_` + server.NewId().String() + `"`
					server.RaisePgResult(tx.Exec(ctx, `PREPARE `+statement+` (bigint,bigint,bigint[]) AS `+control.query))
					var raw []byte
					server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE `+
						statement+`(99,101,'{}')`).Scan(&raw))
					var plans []struct {
						Plan          reliabilityObservationPlanNode
						ExecutionTime float64 `json:"Execution Time"`
					}
					server.Raise(json.Unmarshal(raw, &plans))
					if len(plans) != 1 {
						t.Fatalf("%s %s: missing plan", mode, control.name)
					}
					plan := plans[0]
					examined := reliabilityObservationExamined(plan.Plan)
					buffers := plan.Plan.Hits + plan.Plan.Reads
					bounded := examined <= 20000 && buffers <= 4000
					if plan.Plan.ActualRows != control.wantRows || bounded != control.bounded {
						t.Fatalf("%s %s: output=%.0f/%g examined=%.0f buffers=%.0f bounded=%t/%t",
							mode, control.name, plan.Plan.ActualRows, control.wantRows, examined, buffers, bounded, control.bounded)
					}
					var generic, custom int64
					server.Raise(tx.QueryRow(ctx, `SELECT generic_plans,custom_plans
						FROM pg_prepared_statements WHERE name=$1`, strings.Trim(statement, `"`)).Scan(&generic, &custom))
					wantGeneric := int64(0)
					if mode == "force_generic_plan" {
						wantGeneric = 1
					}
					if generic != wantGeneric || custom != 1-wantGeneric {
						t.Fatalf("%s prepared-path witness: generic=%d custom=%d", mode, generic, custom)
					}
					t.Logf("%s %s: output=%.0f examined=%.0f buffers=%.0f execution=%.3fms",
						mode, control.name, plan.Plan.ActualRows, examined, buffers, plan.ExecutionTime)
					server.RaisePgResult(tx.Exec(ctx, `DEALLOCATE `+statement))
				}
			})
		}
	})
}
