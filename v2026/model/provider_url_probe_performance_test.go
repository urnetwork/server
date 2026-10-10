// A synthetic hundred-thousand-provider queue measures the production claim.
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

// Population setup uses one transaction; only bounded claim calls are timed.
// The plan is reported so sequential scans and growing buffer work stay visible.
func TestUrlProbeDueHundredThousandProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 100000)
		seen := map[server.Id]bool{}
		startedAt := time.Now()
		for range 10 {
			due := ClaimProviderUrlProbeDue(ctx, now, 100, 0, 1)
			if len(due) != 100 {
				t.Fatalf("bounded claim returned %d, want 100", len(due))
			}
			for _, provider := range due {
				if seen[provider.ClientId] {
					t.Fatal("bounded claim duplicated a reserved provider")
				}
				seen[provider.ClientId] = true
			}
		}
		t.Logf("100k providers: ten 100-provider claims took %s (%s per claim); 1000 unique reservations", time.Since(startedAt), time.Since(startedAt)/10)
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+providerUrlProbeDueSql(0, 1),
				now, ProvideModePublic, 100, 1, 0, ProviderUrlProbeRunTarget, now.Add(ProviderEgressProbeAttemptBackoff))
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var line string
					server.Raise(rows.Scan(&line))
					t.Log(line)
				}
			})
		})
		// Existing cycle rows may become ineligible after their first seed.
		// Measure that adversarial head separately from the optimistic plan.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$1`, now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=true
				WHERE client_id IN (SELECT client_id FROM provider_egress_probe_cycle ORDER BY next_attempt_at,client_id LIMIT 99000)`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE network_client_location_reliability`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_probe_cycle`))
		})
		startedAt = time.Now()
		if due := ClaimProviderUrlProbeDue(ctx, now, 100, 0, 1); len(due) != 0 {
			t.Fatalf("stale eligibility hint passed an authoritative risk exclusion: got=%d", len(due))
		}
		t.Logf("100k providers with stale eligibility hints: bounded authoritative rejection took %s", time.Since(startedAt))
		server.Tx(ctx, func(tx server.PgTx) {
			updateProviderUrlProbeEligibility(ctx, tx)
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_probe_cycle`))
		})
		startedAt = time.Now()
		if due := ClaimProviderUrlProbeDue(ctx, now, 100, 0, 1); len(due) != 100 {
			t.Fatalf("indexed eligibility head hid eligible URL turns: got=%d", len(due))
		}
		t.Logf("100k providers after eligibility-hint publication: one 100-provider claim took %s", time.Since(startedAt))
		testingUrlProbeBoundedPlan(t, now)
		// The elected shard's census must read the entire eligible cohort and
		// actual rolling evidence, not a cached quota counter or one due page.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=false`))
			updateProviderUrlProbeEligibility(ctx, tx)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT md5(client_id::text || '-census-success-' || i)::uuid,client_id,
					$1::timestamp-i*interval '20 minutes',1,1,'{}'::jsonb,false,true,1
				FROM provider_egress_probe_cycle CROSS JOIN generate_series(0,9) AS i`, now))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_health_history`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE network_client_location_reliability`))
		})
		snapshotCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		startedAt = time.Now()
		fleet := testingGetUrlProbeFleet(t, snapshotCtx, now)
		t.Logf("100k eligible providers with one million accepted successes: complete rolling/security census took %s", time.Since(startedAt))
		if fleet.Eligible != 100000 || fleet.QuotaComplete != 100000 || fleet.Complete != 100000 || fleet.RunsNeeded != 0 || fleet.SecurityExceptions != 0 {
			t.Fatalf("100k full census lost accepted rolling evidence: %+v", fleet)
		}
	})
}

// Bulk fixture setup is outside every timed claim or census.
func testingSeedUrlProbeFleet(t testing.TB, now time.Time, count int) {
	t.Helper()
	ctx := t.Context()
	city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
	networkId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client(client_id,network_id)
			SELECT md5('synthetic-url-provider-' || i)::uuid,$1 FROM generate_series(1,$2::integer) AS i`, networkId, count))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provide_key(client_id,provide_mode,secret_key)
			SELECT client_id,$1,'synthetic-key'::bytea FROM network_client`, ProvideModePublic))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability
			(client_id,network_id,update_block_number,country_location_id,client_address_hash_count,location_count,connected)
			SELECT client_id,$1,1,$2,1,1,true FROM network_client`, networkId, city.CountryLocationId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible)
			SELECT client_id,$1,$1,true FROM network_client`, now))
		for _, table := range []string{"network_client", "provide_key", "network_client_location_reliability", "provider_egress_probe_cycle"} {
			server.RaisePgResult(tx.Exec(ctx, "ANALYZE "+table))
		}
	})
}

// Slot seeks and lazy merging bound empty, dense, and highly uneven due heads.
// The old residual hash filter scanned 75k/99k foreign rows for empty4/256 shards.
func TestUrlProbeDueEmptyShardPlan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 100000)
		for _, mode := range []string{"normal", "generic"} {
			for _, shardCount := range []int{4, 256} {
				for _, scenario := range []struct {
					name  string
					limit int
				}{{"empty", 100}, {"dense", 100}, {"large", 1000}, {"hot_slot", 8}} {
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=
						CASE WHEN $3='empty' AND slot_id % $2=0 THEN $1::timestamp+interval '4 hours'
						WHEN $3='hot_slot' AND slot_id=0 THEN $1::timestamp-interval '1 hour' ELSE $1 END`, now, shardCount, scenario.name))
						server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_probe_cycle`))
					})
					// A separate cold-update audit measured MVCC cleanup cost. This
					// plan isolates live routing work after ordinary vacuum cleanup.
					server.Db(ctx, func(conn server.PgConn) {
						server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_egress_probe_cycle`))
					})
					var plan testingUrlCompletedPlan
					if mode == "generic" {
						arguments := fmt.Sprintf("%s,%d,%d,%d,0,%d,%s", testingUrlCompletedTimestampLiteral(now),
							ProvideModePublic, scenario.limit, shardCount, ProviderUrlProbeRunTarget,
							testingUrlCompletedTimestampLiteral(now.Add(ProviderEgressProbeAttemptBackoff)))
						plan = testingExplainUrlCompletedGeneric(t, scenario.name, providerUrlProbeDueSql(0, shardCount), arguments)
					} else {
						plan = testingExplainUrlCompleted(t, providerUrlProbeDueSql(0, shardCount),
							now, ProvideModePublic, scenario.limit, shardCount, 0, ProviderUrlProbeRunTarget, now.Add(ProviderEgressProbeAttemptBackoff))
					}
					var slotRows, filtered float64
					indexed, merged := false, false
					var inspect func(testingUrlCompletedPlanNode)
					inspect = func(node testingUrlCompletedPlanNode) {
						if node.NodeType == "Merge Append" {
							merged = true
						}
						if node.IndexName == "provider_egress_probe_cycle_eligible_next_attempt" {
							if err := testingCheckUrlProbeKeyedRecheck(node, scenario.limit); err != nil {
								t.Fatalf("%s shard0/%d %s limit%d: %v", mode, shardCount, scenario.name, scenario.limit, err)
							}
						}
						if node.IndexName == "provider_egress_probe_cycle_slot_next_attempt" {
							indexed = true
							slotRows += node.ActualRows * node.ActualLoops
							filtered += node.RowsRemoved * node.ActualLoops
						}
						for _, child := range node.Plans {
							inspect(child)
						}
					}
					inspect(plan.Plan)
					t.Logf("100k stored, %s shard0/%d %s limit%d: %.0f slot rows, %.0f filtered, %.0f buffers, plan%.3fms execute%.3fms", mode, shardCount,
						scenario.name, scenario.limit, slotRows, filtered, plan.Plan.SharedHits+plan.Plan.SharedReads, plan.PlanningTime, plan.ExecutionTime)
					if scenario.name == "empty" && plan.Plan.SharedHits+plan.Plan.SharedReads > float64(8*ProviderUrlProbeSlotCount/shardCount) {
						t.Fatalf("empty shard read population-sized buffers: %.0f", plan.Plan.SharedHits+plan.Plan.SharedReads)
					}
					if !indexed || !merged || filtered != 0 || slotRows > float64(scenario.limit+ProviderUrlProbeSlotCount/shardCount) {
						t.Fatalf("slot claim lost bounded lazy merging: indexed=%t merged=%t rows=%.0f filtered=%.0f", indexed, merged, slotRows, filtered)
					}
					if scenario.name == "empty" {
						if due := ClaimProviderUrlProbeDue(ctx, now, 100, 0, shardCount); len(due) != 0 {
							t.Fatalf("empty shard claimed a foreign provider: %+v", due)
						}
					}
				}
			}
		}
	})
}

// The global ordered index is unsuitable for routing across slots, but PG18
// may use it for the later client-key recheck instead of the primary key.
// Require an index condition on that exact outer key and work proportional to
// the bounded head. A low output count alone does not bound index-page work.
func testingCheckUrlProbeKeyedRecheck(node testingUrlCompletedPlanNode, limit int) error {
	indexedKey := strings.Contains(node.IndexCond, "client_id = slot_candidates.client_id")
	examinedPerLoop := node.ActualRows + node.RowsRemoved + node.RowsRechecked
	if (node.NodeType != "Index Scan" && node.NodeType != "Index Only Scan") || !indexedKey ||
		node.ActualLoops > float64(limit) || examinedPerLoop > 1 ||
		node.SharedHits+node.SharedReads > 16*node.ActualLoops {
		return fmt.Errorf("global due index is not a bounded client-key recheck: type=%s rows=%.0f filtered=%.0f rechecked=%.0f loops=%.0f buffers=%.0f condition=%s filter=%s",
			node.NodeType, node.ActualRows, node.RowsRemoved, node.RowsRechecked, node.ActualLoops,
			node.SharedHits+node.SharedReads, node.IndexCond, node.Filter)
	}
	return nil
}

func TestUrlProbeDueKeyedRecheckPlanOracle(t *testing.T) {
	healthy := testingUrlCompletedPlanNode{NodeType: "Index Scan", IndexName: "provider_egress_probe_cycle_eligible_next_attempt",
		IndexCond: "((next_attempt_at <= $1) AND (client_id = slot_candidates.client_id))", Filter: "eligible",
		ActualRows: 1, ActualLoops: 100, SharedHits: 620}
	if err := testingCheckUrlProbeKeyedRecheck(healthy, 100); err != nil {
		t.Fatalf("measured bounded client-key recheck was rejected: %v", err)
	}
	for _, change := range []struct {
		name  string
		apply func(*testingUrlCompletedPlanNode)
	}{
		{"global_due_head", func(n *testingUrlCompletedPlanNode) { n.IndexCond = "next_attempt_at <= $1" }},
		{"key_only_in_filter", func(n *testingUrlCompletedPlanNode) { n.Filter = n.IndexCond; n.IndexCond = "next_attempt_at <= $1" }},
		{"population_filtered", func(n *testingUrlCompletedPlanNode) { n.RowsRemoved = 100000 }},
		{"population_rechecked", func(n *testingUrlCompletedPlanNode) { n.RowsRechecked = 100000 }},
		{"population_buffers", func(n *testingUrlCompletedPlanNode) { n.SharedHits = 100000 }},
		{"unbounded_outer", func(n *testingUrlCompletedPlanNode) { n.ActualLoops = 100000 }},
		{"missing_condition", func(n *testingUrlCompletedPlanNode) { n.IndexCond = "" }},
		{"sequential_scan", func(n *testingUrlCompletedPlanNode) { n.NodeType = "Seq Scan" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			mutant := healthy
			change.apply(&mutant)
			if err := testingCheckUrlProbeKeyedRecheck(mutant, 100); err == nil {
				t.Fatal("unbounded or unproved recheck was accepted")
			}
		})
	}
}

// Query-plan rows, rather than a fragile wall-clock threshold, enforce the
// indexed head contract when nearly the entire stored population is excluded.
func testingUrlProbeBoundedPlan(t testing.TB, now time.Time) {
	t.Helper()
	type planNode struct {
		IndexName   string     `json:"Index Name"`
		ActualRows  float64    `json:"Actual Rows"`
		ActualLoops float64    `json:"Actual Loops"`
		Plans       []planNode `json:"Plans"`
	}
	var plans []struct {
		Plan          planNode
		ExecutionTime float64 `json:"Execution Time"`
	}
	server.Db(t.Context(), func(conn server.PgConn) {
		rows, err := conn.Query(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+providerUrlProbeDueSql(0, 1),
			now, ProvideModePublic, 100, 1, 0, ProviderUrlProbeRunTarget, now.Add(ProviderEgressProbeAttemptBackoff))
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				t.Fatal("missing due query plan")
			}
			var raw []byte
			server.Raise(rows.Scan(&raw))
			if err := json.Unmarshal(raw, &plans); err != nil {
				t.Fatal(err)
			}
		})
	})
	indexed := false
	var inspect func(planNode)
	inspect = func(node planNode) {
		if node.IndexName == "provider_egress_probe_cycle_eligible_next_attempt" {
			indexed = true
			if node.ActualRows*node.ActualLoops > 100 {
				t.Fatalf("eligible index examined unbounded candidate rows: %+v", node)
			}
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	for _, plan := range plans {
		inspect(plan.Plan)
		t.Logf("99k excluded/1k eligible indexed plan executed in %.3fms", plan.ExecutionTime)
	}
	if !indexed {
		t.Fatal("due claim did not use the partial eligibility index")
	}
}

// Errors and expired successes cannot make each rolling quota lookup grow with
// provider history. The same lookup serves ingestion, admission, and the census.
func TestUrlProbeRollingHistoryLookupIsBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT md5('synthetic-url-history-' || i)::uuid,$1,
					CASE WHEN i<=50000 THEN $2::timestamp-interval '1 hour'
						WHEN i<=99980 THEN $2::timestamp-interval '5 hours'
						ELSE $2::timestamp-(100000-i)*interval '1 minute' END,
					CASE WHEN i<=50000 THEN 0 ELSE 1 END,1,'{}'::jsonb,false,true,1
				FROM generate_series(1,100000) AS i`, clientId, now))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_health_history`))
		})
		query := providerUrlProbeSuccessWindowSql("$1", "$2")
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, query, clientId, now)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing rolling aggregate")
				}
				var successes int
				var oldest time.Time
				server.Raise(rows.Scan(&successes, &oldest))
				if successes != 10 || !oldest.Equal(now.Add(-9*time.Minute)) {
					t.Fatalf("rolling lookup did not select the ten newest successes: successes=%d oldest=%s", successes, oldest)
				}
			})
			type planNode struct {
				IndexName   string     `json:"Index Name"`
				ActualRows  float64    `json:"Actual Rows"`
				ActualLoops float64    `json:"Actual Loops"`
				Plans       []planNode `json:"Plans"`
			}
			var plans []struct {
				Plan          planNode
				ExecutionTime float64 `json:"Execution Time"`
			}
			rows, err = conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, clientId, now)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing rolling history plan")
				}
				var raw []byte
				server.Raise(rows.Scan(&raw))
				server.Raise(json.Unmarshal(raw, &plans))
			})
			indexed := false
			var inspect func(planNode)
			inspect = func(node planNode) {
				if node.IndexName == "provider_egress_health_history_url_success" {
					indexed = true
					if node.ActualRows*node.ActualLoops > ProviderUrlProbeRunTarget {
						t.Fatalf("rolling quota examined more than ten success rows: %+v", node)
					}
				}
				for _, child := range node.Plans {
					inspect(child)
				}
			}
			for _, plan := range plans {
				inspect(plan.Plan)
				t.Logf("100k mixed history outcomes: latest-ten indexed lookup executed in %.3fms", plan.ExecutionTime)
			}
			if !indexed {
				t.Fatal("rolling quota did not use its partial success index")
			}
		})
	})
}

// Failed measurements use the same bounded latest-ten access as successful runs.
func TestUrlProbeMeasuredQuotaHistoryLookupIsBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT md5('synthetic-url-history-' || i)::uuid,$1,
					CASE WHEN i<=50000 THEN $2::timestamp-interval '1 hour'
						WHEN i<=99980 THEN $2::timestamp-interval '5 hours'
						ELSE $2::timestamp-(100000-i)*interval '1 minute' END,
					CASE WHEN i<=50000 OR i>99980 THEN 0 ELSE 1 END,1,'{}'::jsonb,false,true,1
				FROM generate_series(1,100000) AS i`, clientId, now))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_health_history`))
		})
		query := providerUrlProbeRunWindowSql("$1", "$2")
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, query, clientId, now)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing rolling aggregate")
				}
				var runs int
				var oldest time.Time
				server.Raise(rows.Scan(&runs, &oldest))
				if runs != 10 || !oldest.Equal(now.Add(-9*time.Minute)) {
					t.Fatalf("rolling lookup did not select the ten newest runs: runs=%d oldest=%s", runs, oldest)
				}
			})
			type planNode struct {
				IndexName   string     `json:"Index Name"`
				ActualRows  float64    `json:"Actual Rows"`
				ActualLoops float64    `json:"Actual Loops"`
				Plans       []planNode `json:"Plans"`
			}
			var plans []struct {
				Plan          planNode
				ExecutionTime float64 `json:"Execution Time"`
			}
			rows, err = conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, clientId, now)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing rolling history plan")
				}
				var raw []byte
				server.Raise(rows.Scan(&raw))
				server.Raise(json.Unmarshal(raw, &plans))
			})
			indexed := false
			var inspect func(planNode)
			inspect = func(node planNode) {
				if node.IndexName == "provider_egress_health_history_url_run" {
					indexed = true
					if node.ActualRows*node.ActualLoops > ProviderUrlProbeRunTarget {
						t.Fatalf("rolling quota examined more than ten measured rows: %+v", node)
					}
				}
				for _, child := range node.Plans {
					inspect(child)
				}
			}
			for _, plan := range plans {
				inspect(plan.Plan)
				t.Logf("100k mixed history outcomes: latest-ten indexed lookup executed in %.3fms", plan.ExecutionTime)
			}
			if !indexed {
				t.Fatal("rolling quota did not use its partial measured-run index")
			}
		})
	})
}
