// Generic prepared plans are a separate diagnostic from the normal-planner
// 100k controls. The setting is transaction-local; no planner method is disabled.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// PREPARE retains the owning SQL and infers its actual parameter types. Only
// synthetic typed literals reach EXECUTE; a session witness proves this was a
// generic plan rather than merely another custom EXPLAIN with parameters.
func testingExplainUrlCompletedGeneric(t testing.TB, name, query, arguments string) testingUrlCompletedPlan {
	t.Helper()
	var plans []testingUrlCompletedPlan
	server.Tx(t.Context(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `SET LOCAL plan_cache_mode=force_generic_plan`))
		statement := `"fp2_generic_` + server.NewId().String() + `"`
		server.RaisePgResult(tx.Exec(t.Context(), "PREPARE "+statement+" AS "+query))
		var raw []byte
		server.Raise(tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE "+statement+"("+arguments+")").Scan(&raw))
		// One line retains every real plan while keeping gate artifacts compact.
		var compact strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			compact.WriteString(strings.TrimSpace(line))
		}
		t.Logf("generic %s EXPLAIN JSON: %s", name, compact.String())
		server.Raise(json.Unmarshal(raw, &plans))
		var generic, custom int64
		server.Raise(tx.QueryRow(t.Context(), `SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1`, strings.Trim(statement, `"`)).Scan(&generic, &custom))
		server.RaisePgResult(tx.Exec(t.Context(), "DEALLOCATE "+statement))
		if generic != 1 || custom != 0 {
			t.Fatalf("prepared-path witness was not exactly one generic plan: generic=%d custom=%d", generic, custom)
		}
	})
	if len(plans) != 1 {
		t.Fatalf("missing generic owning SQL plan: %d", len(plans))
	}
	return plans[0]
}

// A future timestamp is a value, never SQL supplied by a request. Source uses
// timestamp-without-time-zone in UTC and the fixture pins microseconds.
func testingUrlCompletedTimestampLiteral(at time.Time) string {
	return "'" + at.UTC().Format("2006-01-02 15:04:05.999999") + "'::timestamp"
}

// Populate every admission side table: an empty fixture cannot expose a generic
// hash subplan that silently reads the complete reliability or security fleet.
func testingSeedUrlCompletedGenericEvidence(t testing.TB, now time.Time) {
	t.Helper()
	server.Tx(t.Context(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO client_connection_reliability_score
			(client_id,lookback_index,independent_reliability_score,independent_reliability_weight,
			reliability_score,reliability_weight,min_block_number,max_block_number)
			SELECT client_id,i,1,1,1,1,1,1 FROM provider_egress_probe_cycle CROSS JOIN generate_series(0,2) AS i`))
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO provider_egress_health
			(client_id,measured_at,ok_count,total_count,class_results,reputation_ok,reputation_total)
			SELECT client_id,$1,1,1,'{}'::jsonb,0,0 FROM provider_egress_probe_cycle`, now))
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO provider_egress_url_security
			(client_id,url_key,destination,measured_at,tls_failure)
			SELECT client_id,'synthetic','{}'::jsonb,$1,true FROM provider_egress_probe_cycle`, now))
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO provider_egress_health_history
			(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
			SELECT md5(client_id::text || '-generic-success')::uuid,client_id,$1,1,1,'{}'::jsonb,false,true,1
			FROM provider_egress_probe_cycle`, now))
		for _, table := range []string{"client_connection_reliability_score", "provider_egress_health", "provider_egress_url_security", "provider_egress_health_history"} {
			server.RaisePgResult(tx.Exec(t.Context(), "ANALYZE "+table))
		}
	})
}

func testingSeedUrlCompletedRetainedRuns(t testing.TB, now time.Time) {
	t.Helper()
	server.Tx(t.Context(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO provider_url_probe_run
			(client_id,claim_ordinal,claimed_at,completed_at,received_at,counted)
			SELECT client_id,i,$1::timestamp-interval '2 days',$1::timestamp-interval '2 days',$1::timestamp-interval '2 days',false
			FROM provider_egress_probe_cycle CROSS JOIN generate_series(1,4) AS i`, now))
		server.RaisePgResult(tx.Exec(t.Context(), `UPDATE provider_egress_probe_cycle SET claim_ordinal=4`))
		server.RaisePgResult(tx.Exec(t.Context(), `ANALYZE provider_url_probe_run`))
	})
}

func TestUrlCompletedPriority100kGenericClaimPlans(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 100000)
		testingSeedUrlCompletedGenericEvidence(t, now)
		for _, scenario := range []struct {
			name       string
			shardCount int
			limit      int
		}{
			{name: "dense", shardCount: 1, limit: 100},
			{name: "all_future", shardCount: 1, limit: 100},
			{name: "dense", shardCount: 4, limit: 100},
			{name: "dense", shardCount: 3, limit: 100},
			{name: "empty_owned", shardCount: 4, limit: 100},
			{name: "hot_slot", shardCount: 4, limit: 8},
			{name: "dense", shardCount: 256, limit: 100},
			{name: "empty_owned", shardCount: 256, limit: 100},
			{name: "ineligible", shardCount: 1, limit: 100},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=$1`, scenario.name == "ineligible"))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET
					eligible=true,
					completed_run_count=CASE WHEN $3='hot_slot' AND slot_id=0 THEN 0 ELSE 1+slot_id%11 END,
					next_attempt_at=CASE WHEN $3='all_future' OR ($3='empty_owned' AND slot_id%$2=0)
						THEN $1::timestamp+interval '4 hours' ELSE $1 END`, now, scenario.shardCount, scenario.name))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET completed_priority_ready=next_attempt_at<=$1`, now))
			})
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_egress_probe_cycle`))
				server.RaisePgResult(conn.Exec(ctx, `ANALYZE provider_url_probe_run`))
			})
			arguments := fmt.Sprintf("%s,%d,%d,%d,0,%d,%s", testingUrlCompletedTimestampLiteral(now),
				ProvideModePublic, scenario.limit, scenario.shardCount, ProviderUrlProbeRunTarget,
				testingUrlCompletedTimestampLiteral(now.Add(ProviderEgressProbeAttemptBackoff)))
			plan := testingExplainUrlCompletedGeneric(t, scenario.name, providerUrlProbeDueSql(0, scenario.shardCount, true), arguments)
			empty := scenario.name == "all_future" || scenario.name == "empty_owned"
			work, err := testingCheckUrlCompletedClaimPlan(plan, scenario.limit, scenario.shardCount, empty)
			if err != nil {
				t.Fatalf("100k generic %s shard0/%d: %v", scenario.name, scenario.shardCount, err)
			}
			want := scenario.limit
			if empty || scenario.name == "ineligible" {
				want = 0
			}
			if plan.Plan.ActualRows != float64(want) {
				t.Fatalf("generic claim returned %.0f rows, want %d", plan.Plan.ActualRows, want)
			}
			t.Logf("100k generic %s shard0/%d limit%d: ready-rows=%.0f filtered=%.0f buffers=%.0f planning=%.3fms execution=%.3fms",
				scenario.name, scenario.shardCount, scenario.limit, work.ReadyRows, work.Filtered,
				work.Buffers, plan.PlanningTime, plan.ExecutionTime)
		}
	})
}

// Pending probes execute on every mature claim, including quiet queues. A
// generic EXISTS plan must not walk100k rows merely to establish that none is
// due. An ordered maintenance head and its no-work probe are checked separately.
func TestUrlCompletedPriority100kGenericMaintenancePlans(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 100000)
		testingSeedUrlCompletedRetainedRuns(t, now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET
				next_attempt_at=$1::timestamp+interval '1 hour',
				completed_next_expiry_at=$1::timestamp+interval '1 hour',completed_priority_ready=false`, now))
		})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_egress_probe_cycle`))
		})
		for _, shardCount := range []int{1, 3, 4, 256} {
			for _, source := range []struct {
				name, predicate, column, query string
				limit                          int
			}{
				{name: "expiry_head", query: providerUrlProbeExpirySql(0, shardCount), limit: providerUrlProbeExpiryClients},
				{name: "promotion_head", query: providerUrlProbePromoteSql(0, shardCount), limit: providerUrlProbePromoteLimit},
				{name: "expiry_pending", predicate: "cycle.completed_next_expiry_at<=$1", column: "completed_next_expiry_at"},
				{name: "promotion_pending", predicate: "cycle.eligible AND NOT cycle.completed_priority_ready AND cycle.next_attempt_at<=$1", column: "next_attempt_at"},
			} {
				arguments := testingUrlCompletedTimestampLiteral(now)
				query := source.query
				if source.predicate != "" {
					query = providerUrlProbeMaintenancePendingSql(source.predicate, source.column, 0, shardCount)
				} else {
					arguments += fmt.Sprintf(",%d", source.limit)
				}
				plan := testingExplainUrlCompletedGeneric(t, source.name, query, arguments)
				ownedSlots := 1
				if shardCount > 1 {
					ownedSlots = (ProviderUrlProbeSlotCount + shardCount - 1) / shardCount
				}
				var examined float64
				testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
					if node.RelationName != "" && node.NodeType != "ModifyTable" {
						examined += (node.ActualRows + node.RowsRemoved + node.RowsRechecked) * node.ActualLoops
					}
				})
				if examined > float64(ownedSlots) || plan.Plan.SharedHits+plan.Plan.SharedReads > float64(16*ownedSlots+64) {
					t.Fatalf("generic no-work maintenance scanned population: source=%s shard0/%d rows=%.0f buffers=%.0f", source.name, shardCount, examined, plan.Plan.SharedHits+plan.Plan.SharedReads)
				}
				t.Logf("100k generic %s shard0/%d: rows=%.0f buffers=%.0f planning=%.3fms execution=%.3fms",
					source.name, shardCount, examined, plan.Plan.SharedHits+plan.Plan.SharedReads, plan.PlanningTime, plan.ExecutionTime)
			}
		}
		plan := testingExplainUrlCompletedGeneric(t, "retention_empty", providerUrlProbeRunRetentionSql,
			testingUrlCompletedTimestampLiteral(now.Add(-providerUrlProbeRunRetention))+",100")
		testingCheckUrlCompletedMaintenanceWork(t, "retention_empty", plan, 0, 0, 64)
	})
}

func testingCheckUrlCompletedMaintenanceWork(t testing.TB, name string, plan testingUrlCompletedPlan, cycleBound, runBound, bufferBound int) {
	t.Helper()
	var cycleRows, runRows float64
	testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
		// RETURNING repeats the write result, not another base-table read.
		// Root buffers still include all tuple and index write work.
		if node.NodeType == "ModifyTable" {
			return
		}
		examined := (node.ActualRows + node.RowsRemoved + node.RowsRechecked) * node.ActualLoops
		switch node.RelationName {
		case "provider_egress_probe_cycle":
			cycleRows += examined
		case "provider_url_probe_run":
			runRows += examined
		default:
			if node.RelationName != "" && examined > 0 {
				t.Fatalf("%s read unexpected relation %s: %.0f", name, node.RelationName, examined)
			}
		}
	})
	buffers := plan.Plan.SharedHits + plan.Plan.SharedReads
	if cycleRows > float64(cycleBound) || runRows > float64(runBound) || buffers > float64(bufferBound) {
		t.Fatalf("generic %s exceeded bounded work: cycles=%.0f/%d receipts=%.0f/%d buffers=%.0f/%d",
			name, cycleRows, cycleBound, runRows, runBound, buffers, bufferBound)
	}
	t.Logf("generic %s: cycles=%.0f receipts=%.0f buffers=%.0f planning=%.3fms execution=%.3fms",
		name, cycleRows, runRows, buffers, plan.PlanningTime, plan.ExecutionTime)
}

func TestUrlCompletedPriority100kGenericPopulatedMaintenance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 100000)
		testingSeedUrlCompletedRetainedRuns(t, now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run
				(client_id,claim_ordinal,claimed_at,completed_at,received_at,counted)
				SELECT client_id,i,$1::timestamp-(15-2*i)*interval '1 hour',
					$1::timestamp-(15-2*i)*interval '1 hour',$1::timestamp-(15-2*i)*interval '1 hour',true
				FROM provider_egress_probe_cycle CROSS JOIN generate_series(5,6) AS i`, now))
		})
		for _, shardCount := range []int{1, 3, 4, 256} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_url_probe_run SET counted=true WHERE claim_ordinal=5`))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET
					claim_ordinal=6,completed_run_count=2,completed_next_expiry_at=$1::timestamp-interval '1 hour',
					next_attempt_at=$1,completed_priority_ready=false`, now))
			})
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_egress_probe_cycle`))
				server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_url_probe_run`))
			})
			ownedSlots := 1
			if shardCount > 1 {
				ownedSlots = (ProviderUrlProbeSlotCount + shardCount - 1) / shardCount
			}
			name := fmt.Sprintf("expiry_populated_shards%d", shardCount)
			plan := testingExplainUrlCompletedGeneric(t, name, providerUrlProbeExpirySql(0, shardCount),
				testingUrlCompletedTimestampLiteral(now)+fmt.Sprintf(",%d", providerUrlProbeExpiryClients))
			testingCheckUrlCompletedMaintenanceWork(t, name, plan,
				3*providerUrlProbeExpiryClients+ownedSlots, 4*providerUrlProbeExpiryClients,
				128*providerUrlProbeExpiryClients+16*ownedSlots)
			server.Db(ctx, func(conn server.PgConn) {
				var adjusted, retired int
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_egress_probe_cycle
					WHERE completed_run_count=1 AND completed_next_expiry_at=$1::timestamp+interval '1 hour'`, now).Scan(&adjusted))
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_url_probe_run WHERE claim_ordinal=5 AND NOT counted`).Scan(&retired))
				if adjusted != providerUrlProbeExpiryClients || retired != adjusted {
					t.Fatalf("expiry page lost exact count/next-expiry: adjusted=%d retired=%d", adjusted, retired)
				}
			})
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET
					completed_next_expiry_at=$1::timestamp+interval '1 hour',completed_priority_ready=false`, now))
			})
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_egress_probe_cycle`))
			})
			name = fmt.Sprintf("promotion_populated_shards%d", shardCount)
			plan = testingExplainUrlCompletedGeneric(t, name, providerUrlProbePromoteSql(0, shardCount),
				testingUrlCompletedTimestampLiteral(now)+fmt.Sprintf(",%d", providerUrlProbePromoteLimit))
			testingCheckUrlCompletedMaintenanceWork(t, name, plan,
				3*providerUrlProbePromoteLimit+ownedSlots, 0, 64*providerUrlProbePromoteLimit+16*ownedSlots)
			server.Db(ctx, func(conn server.PgConn) {
				var expected, promoted int
				server.Raise(conn.QueryRow(ctx, `SELECT LEAST(COUNT(*),$2) FROM provider_egress_probe_cycle WHERE slot_id%$1=0`,
					shardCount, providerUrlProbePromoteLimit).Scan(&expected))
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_egress_probe_cycle WHERE completed_priority_ready`).Scan(&promoted))
				if promoted != expected {
					t.Fatalf("promotion changed %d rows, want %d", promoted, expected)
				}
			})
		}
		plan := testingExplainUrlCompletedGeneric(t, "retention_populated", providerUrlProbeRunRetentionSql,
			testingUrlCompletedTimestampLiteral(now.Add(-24*time.Hour))+",100")
		testingCheckUrlCompletedMaintenanceWork(t, "retention_populated", plan, 0, 200, 6400)
		server.Db(ctx, func(conn server.PgConn) {
			var retained int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_url_probe_run WHERE claim_ordinal<=4`).Scan(&retained))
			if retained != 400000-100 {
				t.Fatalf("bounded retention left %d rows, want %d", retained, 400000-100)
			}
		})
	})
}
