// Fleet-sized controls exercise the exact indexed maintenance and claim SQL.
package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type testingUrlCompletedPlanNode struct {
	NodeType      string                        `json:"Node Type"`
	RelationName  string                        `json:"Relation Name"`
	IndexName     string                        `json:"Index Name"`
	IndexCond     string                        `json:"Index Cond"`
	Filter        string                        `json:"Filter"`
	ActualRows    float64                       `json:"Actual Rows"`
	ActualLoops   float64                       `json:"Actual Loops"`
	RowsRemoved   float64                       `json:"Rows Removed by Filter"`
	RowsRechecked float64                       `json:"Rows Removed by Index Recheck"`
	SharedHits    float64                       `json:"Shared Hit Blocks"`
	SharedReads   float64                       `json:"Shared Read Blocks"`
	Plans         []testingUrlCompletedPlanNode `json:"Plans"`
}

type testingUrlCompletedPlan struct {
	Plan          testingUrlCompletedPlanNode
	PlanningTime  float64 `json:"Planning Time"`
	ExecutionTime float64 `json:"Execution Time"`
}

func testingExplainUrlCompleted(t testing.TB, query string, args ...any) testingUrlCompletedPlan {
	t.Helper()
	var plans []testingUrlCompletedPlan
	server.Db(t.Context(), func(conn server.PgConn) {
		var raw []byte
		server.Raise(conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&raw))
		server.Raise(json.Unmarshal(raw, &plans))
	})
	if len(plans) != 1 {
		t.Fatalf("missing owning SQL plan: %d", len(plans))
	}
	return plans[0]
}

func testingWalkUrlCompletedPlan(node testingUrlCompletedPlanNode, inspect func(testingUrlCompletedPlanNode)) {
	inspect(node)
	for _, child := range node.Plans {
		testingWalkUrlCompletedPlan(child, inspect)
	}
}

type testingUrlCompletedClaimWork struct {
	Indexed, Merged        bool
	ReadyRows, Filtered    float64
	CycleExamined, Buffers float64
	BaseExamined           float64
}

// Dense queues must retain ordered ready-index admission. An empty queue can
// instead use the existing next-attempt index to prove an empty range. Judge
// that path by actual work, never just zero output or a particular index name.
func testingCheckUrlCompletedClaimPlan(plan testingUrlCompletedPlan, limit, shardCount int, empty bool) (testingUrlCompletedClaimWork, error) {
	indexName, ownedSlots := "provider_probe_cycle_completed_ready", 1
	if shardCount > 1 {
		indexName, ownedSlots = "provider_probe_cycle_slot_completed_ready", (ProviderUrlProbeSlotCount+shardCount-1)/shardCount
	}
	work := testingUrlCompletedClaimWork{Buffers: plan.Plan.SharedHits + plan.Plan.SharedReads}
	var failure error
	testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
		examined := (node.ActualRows + node.RowsRemoved + node.RowsRechecked) * node.ActualLoops
		if node.RelationName != "" && node.NodeType != "ModifyTable" {
			work.BaseExamined += examined
			if examined > float64(12*limit+ownedSlots) {
				failure = fmt.Errorf("priority admission read unbounded %s rows: %.0f", node.RelationName, examined)
			}
		}
		if node.IndexName == indexName {
			work.Indexed = true
			work.ReadyRows += node.ActualRows * node.ActualLoops
			work.Filtered += (node.RowsRemoved + node.RowsRechecked) * node.ActualLoops
		}
		if node.NodeType == "Merge Append" {
			work.Merged = true
		}
		if node.NodeType == "Sort" && node.ActualRows*node.ActualLoops > float64(limit) {
			failure = fmt.Errorf("priority sorted a population instead of a bounded head")
		}
		if node.RelationName == "provider_egress_probe_cycle" && node.NodeType != "ModifyTable" {
			work.CycleExamined += examined
			if strings.Contains(node.NodeType, "Seq Scan") && examined > float64(limit+ownedSlots) {
				failure = fmt.Errorf("priority admission scanned the cycle population")
			}
		}
		if shardCount > 1 && node.IndexName == "provider_probe_cycle_completed_ready" {
			failure = fmt.Errorf("sharded priority admission scanned the global ready head")
		}
	})
	if failure != nil {
		return work, failure
	}
	if empty {
		if plan.Plan.ActualRows != 0 || work.BaseExamined > float64(ownedSlots) || work.Buffers > float64(12*ownedSlots+32) {
			return work, fmt.Errorf("empty owned head read population-sized work: output=%.0f examined=%.0f buffers=%.0f", plan.Plan.ActualRows, work.CycleExamined, work.Buffers)
		}
	} else if work.BaseExamined > float64(32*limit+ownedSlots) {
		return work, fmt.Errorf("priority admission exceeded bounded base-table work: %.0f", work.BaseExamined)
	} else if !work.Indexed || shardCount > 1 && !work.Merged || work.Filtered != 0 || work.ReadyRows > float64(limit+ownedSlots) {
		return work, fmt.Errorf("lost indexed bounded ordering: indexed=%t merged=%t scanned=%.0f filtered=%.0f", work.Indexed, work.Merged, work.ReadyRows, work.Filtered)
	}
	return work, nil
}

// Zero output alone is not a boundedness witness: both a sequential scan and
// an index scan can reject the whole population. Exercise those independently
// of the local planner's choice, including a scan with few reported buffers.
func TestUrlCompletedPriorityEmptyPlanOracleRejectsPopulationWork(t *testing.T) {
	healthy := testingUrlCompletedPlan{Plan: testingUrlCompletedPlanNode{
		NodeType: "Sort", ActualLoops: 1, SharedHits: 3,
		Plans: []testingUrlCompletedPlanNode{{
			NodeType: "Index Scan", RelationName: "provider_egress_probe_cycle",
			IndexName: "provider_egress_probe_cycle_eligible_next_attempt", ActualLoops: 1, SharedHits: 3,
		}},
	}}
	if _, err := testingCheckUrlCompletedClaimPlan(healthy, 100, 1, true); err != nil {
		t.Fatalf("bounded empty next-attempt seek was rejected: %v", err)
	}
	if _, err := testingCheckUrlCompletedClaimPlan(healthy, 100, 1, false); err == nil {
		t.Fatal("dense oracle accepted missing ready-index ordering")
	}
	for _, kind := range []string{"Seq Scan", "Index Scan", "Bitmap Heap Scan"} {
		mutant := healthy
		mutant.Plan.Plans = []testingUrlCompletedPlanNode{{
			NodeType: kind, RelationName: "provider_egress_probe_cycle", ActualLoops: 1, RowsRemoved: 100000,
		}}
		if _, err := testingCheckUrlCompletedClaimPlan(mutant, 100, 1, true); err == nil {
			t.Fatalf("empty oracle accepted zero-output %s over the population", kind)
		}
	}
	for _, relation := range []string{"provide_key", "network_client", "network_client_location_reliability", "client_connection_reliability_score", "provider_egress_health", "provider_egress_url_security", "provider_egress_health_history", "provider_url_probe_run"} {
		mutant := healthy
		mutant.Plan.Plans = append([]testingUrlCompletedPlanNode{}, healthy.Plan.Plans...)
		mutant.Plan.Plans = append(mutant.Plan.Plans, testingUrlCompletedPlanNode{
			NodeType: "Index Scan", RelationName: relation, ActualLoops: 1, ActualRows: 100000,
		})
		if _, err := testingCheckUrlCompletedClaimPlan(mutant, 100, 1, true); err == nil {
			t.Fatalf("empty oracle ignored population work in %s", relation)
		}
	}
	mutant := healthy
	mutant.Plan.SharedHits = 100000
	if _, err := testingCheckUrlCompletedClaimPlan(mutant, 100, 1, true); err == nil {
		t.Fatal("empty oracle accepted population-sized buffer work")
	}
	mutant = healthy
	mutant.Plan.ActualRows = 1
	if _, err := testingCheckUrlCompletedClaimPlan(mutant, 100, 1, true); err == nil {
		t.Fatal("empty oracle accepted an unexpected result")
	}
}

// A cold 100k ready-set promotion is paged explicitly, then mature request
// paths seek only their own indexed heads. No planner method is disabled.
func TestUrlCompletedPriority100kBoundedPromotionAndClaimPlans(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		testingSeedUrlProbeFleet(t, now, 100000)
		startedAt := time.Now()
		pages := 0
		for {
			result := ClaimProviderUrlProbeDueWithStatus(ctx, now, 100, 0, 1)
			pages++
			if pages > (100000+providerUrlProbePromoteLimit-1)/providerUrlProbePromoteLimit {
				t.Fatal("bounded cold promotion failed to make progress")
			}
			if !result.PriorityMaintenancePending {
				if len(result.Providers) != 100 {
					t.Fatalf("completed cold promotion returned %d providers", len(result.Providers))
				}
				break
			}
			if len(result.Providers) != 0 {
				t.Fatal("partial promotion claimed a biased ready subset")
			}
			var ready int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_egress_probe_cycle WHERE completed_priority_ready`).Scan(&ready))
			})
			if ready != pages*providerUrlProbePromoteLimit {
				t.Fatalf("promotion exceeded its per-call bound: page=%d ready=%d", pages, ready)
			}
		}
		t.Logf("100k cold priority rows: %d bounded maintenance calls in %s (includes test-only progress census)", pages, time.Since(startedAt))

		for _, scenario := range []struct {
			name       string
			shardCount int
			limit      int
		}{
			{name: "dense", shardCount: 1, limit: 100},
			{name: "all_future", shardCount: 1, limit: 100},
			{name: "dense", shardCount: 4, limit: 100},
			{name: "empty_owned", shardCount: 4, limit: 100},
			{name: "hot_slot", shardCount: 4, limit: 8},
			{name: "dense", shardCount: 256, limit: 100},
			{name: "empty_owned", shardCount: 256, limit: 100},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET
					completed_run_count=CASE WHEN $3='hot_slot' AND slot_id=0 THEN 0 ELSE 1+slot_id%11 END,
					next_attempt_at=CASE WHEN $3='all_future' OR ($3='empty_owned' AND slot_id%$2=0)
						THEN $1::timestamp+interval '4 hours' ELSE $1 END`, now, scenario.shardCount, scenario.name))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET completed_priority_ready=next_attempt_at<=$1`, now))
			})
			// Distinguish live routing cost from dead-tuple cleanup after this
			// fixture's whole-table rewrites; production must monitor both.
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_egress_probe_cycle`))
				server.RaisePgResult(conn.Exec(ctx, `ANALYZE provider_url_probe_run`))
			})
			plan := testingExplainUrlCompleted(t, providerUrlProbeDueSql(0, scenario.shardCount, true),
				now, ProvideModePublic, scenario.limit, scenario.shardCount, 0,
				ProviderUrlProbeRunTarget, now.Add(ProviderEgressProbeAttemptBackoff))
			empty := scenario.name == "all_future" || scenario.name == "empty_owned"
			work, err := testingCheckUrlCompletedClaimPlan(plan, scenario.limit, scenario.shardCount, empty)
			if err != nil {
				t.Fatalf("100k %s shard0/%d: %v; plan=%+v", scenario.name, scenario.shardCount, err, plan)
			}
			t.Logf("100k %s shard0/%d limit%d: rows=%.0f filtered=%.0f buffers=%.0f planning=%.3fms execution=%.3fms",
				scenario.name, scenario.shardCount, scenario.limit, work.ReadyRows, work.Filtered,
				work.Buffers, plan.PlanningTime, plan.ExecutionTime)
			if scenario.name == "all_future" {
				// Defeat both useful range constraints without changing the empty
				// result or disabling a planner method. The actual owning-query
				// mutation must examine100k rows and be rejected by the same oracle.
				query := providerUrlProbeDueSql(0, 1, true)
				if strings.Count(query, " AND cycle.completed_priority_ready") != 1 || strings.Count(query, "cycle.next_attempt_at <= $1") != 1 {
					t.Fatal("empty-plan counterfactual lost its owning predicate anchor")
				}
				query = strings.Replace(query, " AND cycle.completed_priority_ready", "", 1)
				query = strings.Replace(query, "cycle.next_attempt_at <= $1", "(cycle.next_attempt_at + interval '0 seconds') <= $1", 1)
				mutant := testingExplainUrlCompleted(t, query, now, ProvideModePublic, scenario.limit, 1, 0,
					ProviderUrlProbeRunTarget, now.Add(ProviderEgressProbeAttemptBackoff))
				mutantWork, mutantErr := testingCheckUrlCompletedClaimPlan(mutant, scenario.limit, 1, true)
				if mutant.Plan.ActualRows != 0 || mutantWork.CycleExamined < 100000 || mutantErr == nil {
					t.Fatalf("empty oracle did not reject actual zero-output population work: work=%+v err=%v plan=%+v", mutantWork, mutantErr, mutant)
				}
				t.Logf("100k empty counterfactual rejected: examined=%.0f buffers=%.0f reason=%v", mutantWork.CycleExamined, mutantWork.Buffers, mutantErr)
			}
		}
	})
}

// An expiry wave touches at most128 providers and32 receipts per provider,
// using the active per-provider index, not a correlated full-history count.
func TestUrlCompletedPriority100kExpiryPlan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 100000)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run
				(client_id,claim_ordinal,claimed_at,completed_at,received_at,counted)
				SELECT client_id,1,$1::timestamp-interval '5 hours',$1::timestamp-interval '5 hours',$1::timestamp-interval '5 hours',true
				FROM provider_egress_probe_cycle`, now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=1,
				completed_run_count=1,completed_next_expiry_at=$1::timestamp-interval '1 hour'`, now))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_probe_cycle`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_url_probe_run`))
		})
		plan := testingExplainUrlCompleted(t, providerUrlProbeExpirySql(0, 1), now, providerUrlProbeExpiryClients)
		var cycleRows, receiptRows float64
		cycleIndexed, runIndexed := false, false
		testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
			if node.IndexName == "provider_probe_cycle_completed_expiry" {
				cycleIndexed = true
				cycleRows += node.ActualRows * node.ActualLoops
			}
			if node.IndexName == "provider_url_probe_run_active" {
				runIndexed = true
				receiptRows += (node.ActualRows + node.RowsRemoved) * node.ActualLoops
			}
			if strings.Contains(node.NodeType, "Seq Scan") &&
				(node.RelationName == "provider_egress_probe_cycle" || node.RelationName == "provider_url_probe_run") &&
				(node.ActualRows+node.RowsRemoved)*node.ActualLoops > float64(providerUrlProbeExpiryClients*providerUrlProbeExpiryRunsPerClient) {
				t.Fatal("bounded expiry scanned the stored fleet/history")
			}
		})
		if !cycleIndexed || !runIndexed || cycleRows > providerUrlProbeExpiryClients || receiptRows > 2*providerUrlProbeExpiryClients*providerUrlProbeExpiryRunsPerClient {
			t.Fatalf("expiry lost bounded index reads: cycles=%.0f receipts=%.0f cycleIndex=%t runIndex=%t", cycleRows, receiptRows, cycleIndexed, runIndexed)
		}
		var counted int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_url_probe_run WHERE counted`).Scan(&counted))
		})
		if counted != 100000-providerUrlProbeExpiryClients {
			t.Fatalf("expiry page changed %d receipts", 100000-counted)
		}
		t.Logf("100k expiring receipts: cycles=%.0f active-index rows=%.0f buffers=%.0f planning=%.3fms execution=%.3fms",
			cycleRows, receiptRows, plan.Plan.SharedHits+plan.Plan.SharedReads, plan.PlanningTime, plan.ExecutionTime)
	})
}
