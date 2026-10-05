package model

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func publicAdmissionKeys() map[ProvideMode][]byte {
	return map[ProvideMode][]byte{ProvideModePublic: bytes.Repeat([]byte{7}, 32)}
}

func publicAdmissionCycle(t testing.TB, ctx context.Context, clientId server.Id) (bool, bool) {
	t.Helper()
	var exists, eligible bool
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_egress_probe_cycle WHERE client_id=$1),
			COALESCE((SELECT eligible FROM provider_egress_probe_cycle WHERE client_id=$1),false)`, clientId).Scan(&exists, &eligible))
	})
	return exists, eligible
}

// The public-mode transaction, without a later fleet rollup, must make an
// otherwise eligible identity reachable by the real claim owner.
func TestUrlProbePublicAdmissionSeedsImmediately(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provide_key WHERE client_id=$1`, clientId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId))
		})
		if before := GetProviderUrlProbeFleet(ctx, server.NowUtc()); before.Eligible != 0 {
			t.Fatalf("non-public fixture entered denominator: %+v", before)
		}
		SetProvide(ctx, clientId, publicAdmissionKeys())
		at := server.NowUtc().Add(time.Second)
		after := GetProviderUrlProbeFleet(ctx, at)
		claimed := ClaimProviderUrlProbeDue(ctx, at, 1, 0, 1)
		if after.Eligible != 1 || after.MissingCycles != 0 || after.RunsNeeded != 10 || len(claimed) != 1 || claimed[0].ClientId != clientId {
			t.Fatalf("public admission waited for unrelated reliability publication: census=%+v claims=%+v", after, claimed)
		}
		if claimed[0].ClaimOrdinal != 1 || claimed[0].OutcomeCount != 0 {
			t.Fatalf("initialization fabricated completed work: %+v", claimed[0])
		}
		testingCompleteUrlClaim(t, ctx, claimed[0], at.Add(time.Second), "tunnel_failed")
		setupCycle := testingReadUrlCompletionCycle(t, ctx, clientId)
		if setupCycle.count != 1 || setupCycle.history != 0 || GetProviderUrlProbeFleet(ctx, at.Add(time.Second)).RunsNeeded != 10 {
			t.Fatal("completed setup failure was credited as a measurement")
		}
		next := ClaimProviderUrlProbeDue(ctx, setupCycle.next, 1, 0, 1)
		if len(next) != 1 {
			t.Fatalf("setup failure prevented the paced next attempt: %+v", next)
		}
		testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId,
			CycleStartedAt: next[0].CycleStartedAt, MeasuredAt: setupCycle.next, Total: 1, OKCount: 0})
		if measured := GetProviderUrlProbeFleet(ctx, setupCycle.next); measured.RunsNeeded != 9 || measured.QuotaComplete != 0 {
			t.Fatalf("an admitted measured failure did not count exactly once: %+v", measured)
		}
		t.Logf("actual SetProvide immediately admitted eligible=%d missing=%d runs_needed=%d claim_ordinal=%d", after.Eligible, after.MissingCycles, after.RunsNeeded, claimed[0].ClaimOrdinal)
	})
}

// A failed cycle write must not publish the new public key or its change event.
func TestUrlProbePublicAdmissionRollsBackWithKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := testingUrlCompletionClients(t, server.NowUtc(), 1)[0]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provide_key WHERE client_id=$1`, clientId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION public_admission_test_fail() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'synthetic cycle write failure'; END $$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER public_admission_test_failure BEFORE INSERT ON provider_egress_probe_cycle
				FOR EACH ROW EXECUTE FUNCTION public_admission_test_fail()`))
		})
		err := server.HandleError(func() { SetProvide(ctx, clientId, publicAdmissionKeys()) })
		if err == nil || !strings.Contains(fmt.Sprint(err), "synthetic cycle write failure") {
			t.Fatalf("cycle write did not fail at the intended boundary: %v", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var keys, changes, cycles int
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM provide_key WHERE client_id=$1),
				(SELECT count(*) FROM provide_key_change WHERE client_id=$1),
				(SELECT count(*) FROM provider_egress_probe_cycle WHERE client_id=$1)`, clientId).Scan(&keys, &changes, &cycles))
			if keys != 0 || changes != 0 || cycles != 0 {
				t.Fatalf("failed transaction published partial admission: keys=%d changes=%d cycles=%d", keys, changes, cycles)
			}
		})
	})
}

// Rotating keys on an already eligible provider needs no write to its busy
// cycle. The explicit lock remains held until SetProvide has returned.
func TestUrlProbePublicAdmissionKeyRotationDoesNotLockReadyCycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := testingUrlCompletionClients(t, server.NowUtc(), 1)[0]
		locked := make(chan struct{})
		release := make(chan struct{})
		done := make(chan any, 1)
		go func() {
			done <- server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					var got server.Id
					server.Raise(tx.QueryRow(ctx, `SELECT client_id FROM provider_egress_probe_cycle WHERE client_id=$1 FOR UPDATE`, clientId).Scan(&got))
					close(locked)
					<-release
				})
			})
		}()
		<-locked
		defer func() {
			close(release)
			if err := <-done; err != nil {
				t.Errorf("fixture row lock failed: %v", err)
			}
		}()
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := server.HandleError(func() { SetProvide(bounded, clientId, publicAdmissionKeys()) }); err != nil {
			t.Fatalf("key rotation waited for unchanged cycle row: %v", err)
		}
	})
}

// A populated background must not become synchronous work in the public-mode
// handler. The exact production predicate reads only the requested identity.
func TestUrlProbePublicAdmissionLoadedPointPlan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := testingUrlCompletionClients(t, server.NowUtc().Add(-8*time.Hour), 100000)
		clientId := clients[len(clients)/2]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_connection_reliability_score(client_id,lookback_index,independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight,min_block_number,max_block_number)
				SELECT client_id,1,1,1,1,1,0,1 FROM network_client`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE client_connection_reliability_score`))
		})
		plan := testingExplainUrlCompleted(t, providerUrlProbeClientEligibilitySql(), clientId, ProvideModePublic)
		reads := 0.0
		testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
			if node.RelationName == "" {
				return
			}
			reads += (node.ActualRows + node.RowsRemoved + node.RowsRechecked) * node.ActualLoops
			keyed := (node.NodeType == "Index Scan" || node.NodeType == "Index Only Scan") && strings.Contains(node.IndexCond, "client_id =")
			if node.NodeType == "Bitmap Heap Scan" && len(node.Plans) == 1 {
				child := node.Plans[0]
				keyed = child.NodeType == "Bitmap Index Scan" && strings.Contains(child.IndexCond, "client_id =")
			}
			if !keyed {
				t.Fatalf("pointwise admission scanned an unkeyed relation: %+v", node)
			}
		})
		if reads > 6 || plan.Plan.SharedHits+plan.Plan.SharedReads > 64 {
			t.Fatalf("100k background enlarged pointwise eligibility: rows=%.0f buffers=%.0f", reads, plan.Plan.SharedHits+plan.Plan.SharedReads)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provide_key WHERE client_id=$1`, clientId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId))
		})
		start := time.Now()
		SetProvide(ctx, clientId, publicAdmissionKeys())
		if exists, eligible := publicAdmissionCycle(t, ctx, clientId); !exists || !eligible {
			t.Fatal("loaded public admission did not seed its own identity")
		}
		t.Logf("100k background: exact eligibility rows=%.0f buffers=%.0f plan_execute_ms=%.3f actual_SetProvide_ms=%.3f (descriptive only)", reads, plan.Plan.SharedHits+plan.Plan.SharedReads, plan.ExecutionTime, float64(time.Since(start).Microseconds())/1000)
	})
}

// Public re-entry repairs only the hint. A live lease, its token and empirical
// counters survive removal, re-entry, repeated key rotation and reconciliation.
func TestUrlProbePublicAdmissionPreservesExistingProgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clientId := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 1)[0]
		due := testingClaimUrlCompletion(t, ctx, clientId, now)
		before := testingReadUrlCompletionCycle(t, ctx, clientId)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{})
		if exists, eligible := publicAdmissionCycle(t, ctx, clientId); !exists || eligible {
			t.Fatalf("public removal did not atomically withdraw existing hint: exists=%t eligible=%t", exists, eligible)
		}
		for range 2 {
			SetProvide(ctx, clientId, publicAdmissionKeys())
			if exists, eligible := publicAdmissionCycle(t, ctx, clientId); !exists || !eligible {
				t.Fatalf("public re-entry left a false hint: exists=%t eligible=%t", exists, eligible)
			}
			after := testingReadUrlCompletionCycle(t, ctx, clientId)
			if after.ordinal != before.ordinal || !after.next.Equal(before.next) || after.count != before.count || after.history != before.history || after.successes != before.successes || after.errors != before.errors {
				t.Fatalf("public change rewrote progress or a live lease: before=%+v after=%+v", before, after)
			}
		}
		if duplicate := ClaimProviderUrlProbeDue(ctx, now.Add(time.Second), 1, 0, 1); len(duplicate) != 0 {
			t.Fatalf("public change released current claim: %+v", duplicate)
		}
		receipt := testingCompleteUrlClaim(t, ctx, due, now.Add(2*time.Second), "tunnel_failed")
		if receipt.Replay || testingReadUrlCompletionCycle(t, ctx, clientId).history != 0 {
			t.Fatal("setup completion fabricated quota credit")
		}
	})
}

func TestUrlProbePublicAdmissionUsesCurrentEligibility(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clients := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 9)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle`))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provide_key`))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET connected=false WHERE client_id=$1`, clients[1]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET location_count=0 WHERE client_id=$1`, clients[2]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, clients[3]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id=$2 WHERE client_id=$1`, clients[4], clients[0]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=true WHERE client_id=$1`, clients[5]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET ipv4_proven=false,ipv6_proven=true WHERE client_id=$1`, clients[6]))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_connection_reliability_score(client_id,lookback_index,independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight,min_block_number,max_block_number)
				VALUES($1,1,0,0,0,0,0,1)`, clients[7]))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security(client_id,url_key,destination,measured_at,tls_failure)
				VALUES($1,'synthetic-security-target','{}',$2,true)`, clients[8], now))
		})
		for i, clientId := range clients {
			SetProvide(ctx, clientId, publicAdmissionKeys())
			exists, eligible := publicAdmissionCycle(t, ctx, clientId)
			want := i == 0 || i == 8 // A quarantined identity remains probe-eligible.
			if exists != want || eligible != want {
				t.Fatalf("gate case%d: exists=%t eligible=%t want=%t", i, exists, eligible, want)
			}
		}
		fleet := GetProviderUrlProbeFleet(ctx, server.NowUtc())
		if fleet.Eligible != 2 || fleet.MissingCycles != 0 || fleet.SecurityExceptions != 1 || fleet.RunsNeeded != 20 {
			t.Fatalf("pointwise admission changed cohort or security gates: %+v", fleet)
		}
	})
}

// Census history joins by provider identity independently of cycle presence.
// A missing cycle can be quota-complete; aggregate subtraction cannot infer
// overlap or which of the missing cycles contributes the current deficit.
func TestUrlProbeMissingCycleQuotaOverlapIsIndependent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		clients := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 2)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT md5('public-admission-history-'||i)::uuid,$1,$2::timestamp-i*interval '1 minute',i%2,1,'{}',false,true,1 FROM generate_series(1,10)i`, clients[0], now))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle`))
		})
		before := GetProviderUrlProbeFleet(ctx, now)
		if before.Eligible != 2 || before.MissingCycles != 2 || before.QuotaComplete != 1 || before.RunsNeeded != 10 {
			t.Fatalf("missing-cycle/history independence lost: %+v", before)
		}
		if claimed := ClaimProviderUrlProbeDue(ctx, now, 2, 0, 1); len(claimed) != 0 {
			t.Fatalf("a provider without a cycle was claimable: %+v", claimed)
		}
		UpdateClientReliabilityScores(ctx, now, false)
		after := GetProviderUrlProbeFleet(ctx, server.NowUtc())
		if after.Eligible != before.Eligible || after.MissingCycles != 0 || after.QuotaComplete != before.QuotaComplete || after.RunsNeeded != before.RunsNeeded {
			t.Fatalf("existing score publication changed measurement credit: before=%+v after=%+v", before, after)
		}
		t.Logf("missing_cycle_overlap: missing=%d quota_complete_among_missing=1 deficient_among_missing=1 runs_needed=%d; real score publication repairs cycles only", before.MissingCycles, before.RunsNeeded)
	})
}
