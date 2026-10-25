package model

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func testingLegacyRenewalHistory(t testing.TB) (server.Id, time.Time) {
	t.Helper()
	ctx := t.Context()
	now := server.NowUtc().Truncate(time.Microsecond)
	client := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
	first := now.Add(-4*time.Hour + 10*time.Minute)
	for i := range 10 {
		at := first.Add(time.Duration(i) * 20 * time.Minute)
		due := testingClaimUrlCompletion(t, ctx, client, at.Add(-time.Second))
		testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: client, CycleStartedAt: due.CycleStartedAt, MeasuredAt: at, OKCount: 1, Total: 1})
		testingCompleteUrlClaim(t, ctx, due, at.Add(time.Microsecond), "")
	}
	oldDeadline := testingRestoreLegacyQuotaDeadline(t, client, now)
	if !oldDeadline.Equal(first.Add(4 * time.Hour)) {
		t.Fatal("fixture did not restore the exact deployed deadline")
	}
	return client, oldDeadline.Add(-ProviderUrlProbeRenewalHeadroom)
}

// Reconstitute the deployed quota-full deadline from the same accepted-history
// projection used by the old writer. The earlier parent RED independently
// proves that the actual old writer persists this exact four-hour boundary.
func testingRestoreLegacyQuotaDeadline(t testing.TB, client server.Id, at time.Time) time.Time {
	t.Helper()
	var next time.Time
	server.Tx(t.Context(), func(tx server.PgTx) {
		server.Raise(tx.QueryRow(t.Context(), `WITH recent AS (`+providerUrlProbeRunWindowSql("$1", "$2")+`)
 UPDATE provider_egress_probe_cycle cycle SET next_attempt_at=recent.oldest_run_at + interval '4 hours'
 FROM recent WHERE cycle.client_id=$1 AND recent.run_count=10 RETURNING cycle.next_attempt_at`, client, at).Scan(&next))
	})
	return next
}

func TestUrlProbeRenewalDiscoversLegacyParkedDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		client, now := testingLegacyRenewalHistory(t)
		due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		t.Logf("legacy_deadline_remaining=%s admitted_replacements=%d", ProviderUrlProbeRenewalHeadroom, len(due))
		if len(due) != 1 || due[0].ClientId != client || due[0].RunsNeeded != 0 {
			t.Fatal("legacy quota-full deadline hides required replacement before expiry")
		}
	})
}

func TestUrlProbeRenewalLegacyRepairRequiresQuietOwnership(t *testing.T) {
	for _, scenario := range []string{"missing_receipt", "uncompleted_receipt", "recent_completion", "recent_attempt", "new_deadline", "different_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := t.Context()
				client, now := testingLegacyRenewalHistory(t)
				server.Tx(ctx, func(tx server.PgTx) {
					switch scenario {
					case "missing_receipt":
						server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_url_probe_run WHERE client_id=$1 AND claim_ordinal=10`, client))
					case "uncompleted_receipt":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_url_probe_run SET completed_at=NULL,received_at=NULL,counted=false WHERE client_id=$1 AND claim_ordinal=10`, client))
					case "recent_completion":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_url_probe_run SET completed_at=$2,received_at=$2 WHERE client_id=$1 AND claim_ordinal=10`, client, now.Add(-time.Second)))
					case "recent_attempt":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_attempt SET attempt_at=$2,probe_failure='setup_failed' WHERE client_id=$1`, client, now.Add(-time.Second)))
					case "new_deadline":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=next_attempt_at-interval '6 minutes' WHERE client_id=$1`, client))
					case "different_deadline":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=next_attempt_at-interval '1 microsecond' WHERE client_id=$1`, client))
					}
				})
				if scenario == "new_deadline" {
					now = now.Add(-time.Microsecond)
				}
				before := testingReadUrlCompletionCycle(t, ctx, client)
				if got := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1); len(got) != 0 {
					t.Fatal("repair admitted uncertain ownership or a nonlegacy deadline")
				}
				if after := testingReadUrlCompletionCycle(t, ctx, client); after.next != before.next || after.ordinal != before.ordinal || after.history != before.history {
					t.Fatal("repair changed a guarded deadline or measurement history")
				}
			})
		})
	}
}

func TestUrlProbeRenewalLegacyRepairKeepsConfiguredBackoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		client, now := testingLegacyRenewalHistory(t)
		// A longer configured success delay is also ownership, even when its
		// timestamp coincides exactly with the old quota expiry.
		pop := server.Config.PushSimpleResource(ProviderEgressProbeResourceName, []byte("url_success_interval_seconds: 3600\n"))
		currentProviderEgressRules.Store(nil)
		t.Cleanup(func() { pop(); currentProviderEgressRules.Store(nil) })
		before := testingReadUrlCompletionCycle(t, ctx, client)
		if got := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1); len(got) != 0 {
			t.Fatal("repair borrowed the configured successful-turn interval")
		}
		if after := testingReadUrlCompletionCycle(t, ctx, client); after.next != before.next || after.ordinal != before.ordinal {
			t.Fatal("configured backoff changed")
		}
	})
}

func TestUrlProbeRenewalLegacyRepairIsShardOwnedAndSingleClaim(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		client, now := testingLegacyRenewalHistory(t)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		var shard int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT slot_id % 8 FROM provider_egress_probe_cycle WHERE client_id=$1`, client).Scan(&shard))
		})
		if got := ClaimProviderUrlProbeDue(ctx, now, 1, (shard+1)%8, 8); len(got) != 0 {
			t.Fatal("foreign shard repaired a legacy deadline")
		}
		var wg sync.WaitGroup
		results := make(chan []ProviderUrlProbeDue, 8)
		for range 8 {
			wg.Add(1)
			go func() { defer wg.Done(); results <- ClaimProviderUrlProbeDue(ctx, now, 1, shard, 8) }()
		}
		wg.Wait()
		close(results)
		issued := 0
		for got := range results {
			for _, due := range got {
				issued++
				if due.ClientId != client || due.RunsNeeded != 0 || due.ClaimOrdinal != 11 {
					t.Fatal("repair lost measured quota or issued-claim identity")
				}
			}
		}
		if issued != 1 {
			t.Fatalf("concurrent repair issued %d replacements, want one", issued)
		}
	})
}

func TestUrlProbeRenewalLegacyRepairBoundsFuturePopulation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now, 100000)
		// Populate each keyed side relation, so a generic hash/scan cannot hide
		// behind empty ownership or measurement tables. One measurement is
		// deliberately insufficient for repair; all selected joins still run.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=1,latest_result_at=$1`, now.Add(-time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run(client_id,claim_ordinal,claimed_at,completed_at,received_at,counted)
 SELECT client_id,1,$1,$1,$1,true FROM provider_egress_probe_cycle`, now.Add(-time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_attempt(client_id,attempt_at,update_time) SELECT client_id,$1,$1 FROM provider_egress_probe_cycle`, now.Add(-time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
 (run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
 SELECT md5(client_id::text||'-repair-cost')::uuid,client_id,$1,1,1,'{}'::jsonb,false,true,1 FROM provider_egress_probe_cycle`, now.Add(-time.Hour)))
			for _, table := range []string{"provider_url_probe_run", "provider_egress_probe_attempt", "provider_egress_health_history"} {
				server.RaisePgResult(tx.Exec(ctx, "ANALYZE "+table))
			}
		})
		for _, scenario := range []string{"outside_horizon", "ambiguous_head"} {
			server.Tx(ctx, func(tx server.PgTx) {
				deadline := now.Add(time.Hour)
				if scenario == "ambiguous_head" {
					deadline = now.Add(time.Minute)
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$1`, deadline))
			})
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) provider_egress_probe_cycle`))
			})
			for _, shards := range []int{1, 8} {
				for _, mode := range []string{"custom", "generic"} {
					var plan testingUrlCompletedPlan
					if mode == "generic" {
						args := fmt.Sprintf("%s,128,%s", testingUrlCompletedTimestampLiteral(now), testingUrlCompletedTimestampLiteral(now.Add(-22*time.Minute)))
						plan = testingExplainUrlCompletedGeneric(t, "legacy_repair", providerUrlProbeLegacyDeadlineRepairSql(0, shards), args)
					} else {
						plan = testingExplainUrlCompleted(t, providerUrlProbeLegacyDeadlineRepairSql(0, shards), now, 128, now.Add(-22*time.Minute))
					}
					var examined float64
					indexed := 0
					var failure string
					testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
						if node.RelationName != "" && node.NodeType != "ModifyTable" {
							rows := (node.ActualRows + node.RowsRemoved + node.RowsRechecked) * node.ActualLoops
							examined += rows
							if rows > 256 {
								failure = fmt.Sprintf("unbounded relation %s examined %.0f rows", node.RelationName, rows)
							}
						}
						if (node.IndexName == "provider_egress_probe_cycle_eligible_next_attempt" && shards == 1) || node.IndexName == "provider_egress_probe_cycle_slot_next_attempt" {
							if strings.Contains(node.IndexCond, "next_attempt_at >") && strings.Contains(node.IndexCond, "next_attempt_at <=") {
								indexed++
							}
						}
					})
					buffers := plan.Plan.SharedHits + plan.Plan.SharedReads
					t.Logf("100k %s %s shards=%d examined=%.0f buffers=%.0f indexed_horizons=%d plan_ms=%.3f execute_ms=%.3f", scenario, mode, shards, examined, buffers, indexed, plan.PlanningTime, plan.ExecutionTime)
					if failure != "" || examined > 2048 || buffers > 8192 || indexed == 0 {
						t.Fatalf("future repair is not bounded: %s", failure)
					}
				}
			}
		}
	})
}

// Widening the head cutoff to the renewal horizon must never borrow a live
// claim's remaining reservation, even after its measured quota has expired.
func TestUrlProbeRenewalLookaheadCannotShortenLiveClaim(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		expiries := make([]time.Time, 10)
		for i := range expiries {
			expiries[i] = now.Add(ProviderUrlProbeRenewalHeadroom + time.Duration(i)*20*time.Minute)
		}
		client := testingRenewalHistory(t, now, expiries)
		due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(due) != 1 {
			t.Fatal("initial owned replacement missing")
		}
		before := testingReadUrlCompletionCycle(t, ctx, client)
		if got := ClaimProviderUrlProbeDue(ctx, now.Add(10*time.Minute), 1, 0, 1); len(got) != 0 {
			t.Fatal("future lookahead duplicated an uncompleted claim")
		}
		after := testingReadUrlCompletionCycle(t, ctx, client)
		if after.next != before.next || after.ordinal != before.ordinal {
			t.Fatal("future lookahead shortened the claim reservation")
		}
	})
}
