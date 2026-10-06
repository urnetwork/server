package model

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Explicit policy capture must not change SQL emitted by any existing caller.
// The captured branch remains stable even if a later resource read is invalid.
func TestUrlProbeMatureDeficitPolicyCapture(t *testing.T) {
	pop := server.Config.PushSimpleResource(ProviderEgressProbeResourceName, []byte("url_probe_result_version: 1\n"))
	defer pop()
	currentProviderEgressRules.Store(nil)
	defer currentProviderEgressRules.Store(nil)
	want := fmt.Sprintf(`SELECT COUNT(*)::integer AS run_count, MIN(measured_at) AS oldest_run_at
		FROM (SELECT measured_at FROM provider_egress_health_history
			WHERE client_id=provider.client_id AND url_probe AND url_probe_policy_version=1
			AND total_count=1 AND (ok_count=0 OR ok_count=1)
			AND measured_at > $1::timestamp - interval '%d seconds'
			AND measured_at <= $1::timestamp
			ORDER BY measured_at DESC LIMIT %d) AS recent_runs`, int(ProviderEgressProbeRefreshAge/time.Second), ProviderUrlProbeRunTarget)
	if got := providerUrlProbeRunWindowSql("provider.client_id", "$1"); got != want {
		t.Fatalf("default quota SQL changed: %s", got)
	}
	if got := providerUrlProbeRunWindowSql("provider.client_id", "$1", 1); got != want {
		t.Fatalf("captured quota SQL differs: %s", got)
	}
	popInvalid := server.Config.PushSimpleResource(ProviderEgressProbeResourceName, []byte("url_probe_result_version: 2\n"))
	defer popInvalid()
	currentProviderEgressRules.Store(nil)
	if SelectedProviderUrlProbePolicyVersion() != -1 {
		t.Fatal("invalid mounted policy was silently replaced")
	}
	if got := providerUrlProbeRunWindowSql("provider.client_id", "$1", 1); got != want {
		t.Fatal("captured policy re-read configuration")
	}
	if query := providerUrlProbeFleetSql(-1); strings.Count(query, "url_probe_policy_version=-1") != 2 {
		t.Fatal("stock census and diagnostic history did not share the selected policy")
	}
}

func testingUrlMatureDeficitQueryValues(t testing.TB, ctx context.Context, query string, now time.Time) []any {
	t.Helper()
	var values []any
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, query, now, ProvideModePublic, ProviderUrlProbeRunTarget, now.Add(-ProviderEgressProbeRefreshAge))
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				t.Fatal("missing census row")
			}
			values, err = rows.Values()
			server.Raise(err)
		})
	})
	return values
}

// Remove only the new CTEs and trailing diagnostic column. This retains the
// exact pre-extension cohort and all21 original aggregate expressions.
func testingUrlMatureDeficitStockSql() string {
	query := providerUrlProbeFleetSql(1)
	start := strings.Index(query, ",\n\t\tmature_deficit_head AS MATERIALIZED")
	end := strings.Index(query, "\n\t\t\tSELECT COUNT(*)")
	query = query[:start] + query[end:]
	start = strings.Index(query, "\n\t\t\t\t,(SELECT COALESCE(jsonb_agg(diagnostic")
	end = strings.LastIndex(query, "\n\t\t\tFROM cohort")
	return query[:start] + query[end:]
}

// All104 deficits lie after the70181 healthy IDs. A healthy-prefix-first cap
// would return zero, while completed-turn or deadline filters lose real rows.
func TestUrlProbeMatureDeficitSelectsAfterQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now.Add(-5*time.Hour), 70285)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `WITH ordered AS (
				SELECT client_id,row_number() OVER (ORDER BY client_id) AS n FROM provider_egress_probe_cycle
			) INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT md5(client_id::text || '-measured-' || i)::uuid,client_id,
					$1::timestamp-interval '30 minutes'-i*interval '1 second',i%2,1,'{}',false,true,1
				FROM ordered CROSS JOIN LATERAL generate_series(1,CASE WHEN n<=70181 THEN 10 WHEN n<=70280 THEN 9 ELSE 8 END) AS i`, now))
			server.RaisePgResult(tx.Exec(ctx, `WITH deficient AS (
				SELECT client_id FROM provider_egress_probe_cycle ORDER BY client_id OFFSET 70181
			) INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT md5(client_id::text || '-expired-' || i)::uuid,client_id,
					$1::timestamp-interval '4 hours'-i*interval '1 minute',0,1,'{}',false,true,1
				FROM deficient CROSS JOIN generate_series(1,2) AS i`, now))
			server.RaisePgResult(tx.Exec(ctx, `WITH deficient AS (
				SELECT client_id,row_number() OVER (ORDER BY client_id) AS n FROM
				(SELECT client_id FROM provider_egress_probe_cycle ORDER BY client_id OFFSET 70181) AS selected
			) UPDATE provider_egress_probe_cycle AS cycle SET completed_run_count=10,
				eligible=deficient.n%2=0,next_attempt_at=$1::timestamp+(deficient.n%5)*interval '5 minutes'
				FROM deficient WHERE cycle.client_id=deficient.client_id`, now))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_health_history`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE provider_egress_probe_cycle`))
		})
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		started := time.Now()
		fleet := GetProviderUrlProbeFleet(readCtx, now)
		duration := time.Since(started)
		d := fleet.MatureDeficitDiagnostics
		if fleet.MatureEligible != 70285 || fleet.MatureQuotaComplete != 70181 || fleet.MatureRunsNeeded != 109 ||
			d.Selected != 104 || d.TotalMatureDeficient != 104 || d.Capped != 0 || d.RunsNeeded != 109 ||
			d.MissingCredits != [4]int{99, 5, 0, 0} || d.HintFalse != 52 || d.CompletedCountGE10 != 104 ||
			d.AcceptedSuccesses != 515 || d.AcceptedFailures != 416 || d.RetainedFewer10 != 0 ||
			d.ExpiredAge != [3]int{104, 0, 0} {
			t.Fatalf("exact sparse mature deficit census changed: fleet=%+v diagnostic=%+v", fleet, d)
		}
		oldStarted := time.Now()
		stock := testingUrlMatureDeficitQueryValues(t, ctx, testingUrlMatureDeficitStockSql(), now)
		oldDuration := time.Since(oldStarted)
		current := testingUrlMatureDeficitQueryValues(t, ctx, providerUrlProbeFleetSql(1), now)
		if len(stock) != 21 || len(current) != 22 || !reflect.DeepEqual(stock, current[:21]) {
			t.Fatal("diagnostic enrichment changed an original census field")
		}
		plan := testingExplainUrlCompleted(t, providerUrlProbeFleetSql(1), now, ProvideModePublic, ProviderUrlProbeRunTarget, now.Add(-ProviderEgressProbeRefreshAge))
		fullHistory, selectedHistory := 0, 0
		detailRows := 0.0
		testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
			if node.RelationName == "provider_egress_health_history" {
				switch node.ActualLoops {
				case 70285:
					fullHistory++
				case 104:
					selectedHistory++
					detailRows += node.ActualRows * node.ActualLoops
					if !strings.Contains(node.IndexCond, "client_id") || node.IndexName != "provider_egress_health_history_url_run" {
						t.Fatalf("selected history lost its keyed partial index: %+v", node)
					}
				default:
					t.Fatalf("unexpected second population history traversal: %+v", node)
				}
			}
		})
		if fullHistory != 1 || selectedHistory != 1 || detailRows > 1040 {
			t.Fatalf("unbounded detail/history work: full=%d detail=%d rows=%g", fullHistory, selectedHistory, detailRows)
		}
		t.Logf("70285 mature,104 tail deficits,109 missing: original stock %s; enriched %s; actual plan %.3fms; one full history traversal plus %.0f selected history rows", oldDuration, duration, plan.ExecutionTime, detailRows)
	})
}

func TestUrlProbeMatureDeficitCapAndEmpty(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		ids := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 129)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET eligible=false WHERE client_id=$1`, ids[128]))
		})
		d := GetProviderUrlProbeFleet(ctx, now).MatureDeficitDiagnostics
		if d.ContractVersion != 1 || d.PolicyVersion != 1 || d.SampleLimit != 128 || d.Selected != 128 ||
			d.TotalMatureDeficient != 129 || d.Capped != 1 || d.RunsNeeded != 1280 || d.HintFalse != 0 || d.Claim != [4]int{128, 0, 0, 0} {
			t.Fatalf("cap did not follow deterministic qualified IDs: %+v", d)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, ids[128]))
		})
		if equal := GetProviderUrlProbeFleet(ctx, now).MatureDeficitDiagnostics; equal.Selected != 128 || equal.TotalMatureDeficient != 128 || equal.Capped != 0 {
			t.Fatalf("exactly128 was mislabeled as truncated: %+v", equal)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false`))
		})
		d = GetProviderUrlProbeFleet(ctx, now).MatureDeficitDiagnostics
		if d.ContractVersion != 1 || d.PolicyVersion != 1 || d.Selected != 0 || d.TotalMatureDeficient != 0 || d.Capped != 0 {
			t.Fatalf("empty census is absent or stale: %+v", d)
		}
	})
}

// Future source clocks are possible relative to the supplied comparison T.
// They remain visible but cannot be classified as known claim/completion at T.
func TestUrlProbeMatureDeficitGatesHistoryAndClocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		ids := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 16)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, change := range []struct {
				index int
				sql   string
			}{
				{1, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2::timestamp-interval '4 hours' WHERE client_id=$1`},
				{2, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2::timestamp-interval '4 hours'+interval '1 microsecond' WHERE client_id=$1`},
				{3, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2::timestamp+interval '1 microsecond' WHERE client_id=$1`},
			} {
				server.RaisePgResult(tx.Exec(ctx, change.sql, ids[change.index], now))
			}
			for _, change := range []struct {
				index int
				sql   string
			}{
				{4, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`},
				{5, `UPDATE network_client_location_reliability SET arin_risk=true WHERE client_id=$1`},
				{6, `UPDATE network_client SET active=false WHERE client_id=$1`},
				{7, `UPDATE network_client SET source_client_id=client_id WHERE client_id=$1`},
				{8, `DELETE FROM provide_key WHERE client_id=$1`},
				{9, `UPDATE network_client_location_reliability SET connected=false WHERE client_id=$1`},
				{10, `UPDATE network_client_location_reliability SET location_count=2 WHERE client_id=$1`},
				{11, `UPDATE network_client_location_reliability SET ipv4_proven=false,ipv6_proven=true WHERE client_id=$1`},
				{13, `UPDATE network_client_location_reliability SET arin_non_quality=true WHERE client_id=$1`},
				{15, `UPDATE provider_egress_probe_cycle SET eligible=false WHERE client_id=$1`},
			} {
				server.RaisePgResult(tx.Exec(ctx, change.sql, ids[change.index]))
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_connection_reliability_score
				(client_id,independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight,lookback_index)
				VALUES($1,0.9,0.9,1,1,1)`, ids[12]))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security(client_id,url_key,destination,measured_at,tls_failure)
				VALUES($1,'synthetic','{}',$2,true)`, ids[14], now))
			for i := range 9 {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					VALUES($1,$2,$3,$4,1,'{}',false,true,1)`, server.NewId(), ids[0], now.Add(-time.Minute), i%2))
			}
			// Equal-time expired ties may choose any member, but all nine current
			// credits must survive the bounded latest10 lookup.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				SELECT md5($1::text||'-tie-'||i)::uuid,$1::uuid,$2::timestamp-interval '4 hours 1 minute',0,1,'{}',false,true,1
				FROM generate_series(1,32) AS i`, ids[0], now))
			for _, row := range []struct {
				age               time.Duration
				ok, total, policy int
				url               bool
			}{
				{4*time.Hour - time.Microsecond, 0, 1, 1, true}, {0, 1, 1, 1, true},
				{4 * time.Hour, 0, 1, 1, true}, {-time.Microsecond, 1, 1, 1, true},
				{time.Second, 1, 1, 0, true}, {time.Second, 1, 1, 2, true},
				{time.Second, 0, 0, 1, true}, {time.Second, 0, 2, 1, true}, {time.Second, 1, 1, 1, false},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					VALUES($1,$2,$3,$4,$5,'{}',false,$6,$7)`, server.NewId(), ids[1], now.Add(-row.age), row.ok, row.total, row.url, row.policy))
			}
			for _, row := range []struct {
				index                                                    int
				claimed, reported, completed, received, attempt, updated time.Duration
				failure                                                  string
			}{
				{0, 5 * time.Second, 9 * time.Second, 10 * time.Second, 11 * time.Second, 20 * time.Second, 21 * time.Second, "tunnel_failed"},
				{1, -400 * time.Second, -11 * time.Second, -10 * time.Second, -5 * time.Second, -10 * time.Second, -3 * time.Second, "no_exit_ip"},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run
					(client_id,claim_ordinal,claimed_at,reported_completed_at,completed_at,received_at,probe_failure,counted)
					VALUES($1,1,$2,$3,$4,$5,$6,true)`, ids[row.index], now.Add(row.claimed), now.Add(row.reported), now.Add(row.completed), now.Add(row.received), row.failure))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=1,completed_run_count=10,
					next_attempt_at=$2::timestamp+interval '15 minutes' WHERE client_id=$1`, ids[row.index], now.Add(row.claimed)))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_attempt(client_id,attempt_at,update_time,probe_failure)
					VALUES($1,$2,$3,$4)`, ids[row.index], now.Add(row.attempt), now.Add(row.updated), row.failure))
			}
		})
		fleet := GetProviderUrlProbeFleet(ctx, now)
		d := fleet.MatureDeficitDiagnostics
		if fleet.Eligible != 8 || fleet.MatureEligible != 5 || fleet.WarmingEligible != 1 || fleet.EligibilityAgeUnknown != 2 ||
			d.Selected != 5 || d.RunsNeeded != 39 || d.AcceptedSuccesses != 5 || d.AcceptedFailures != 6 ||
			d.HintFalse != 1 || d.SecurityException != 1 || d.CompletedCountGE10 != 2 ||
			d.ClaimClockFuture != 1 || d.AttemptClockFuture != 1 || d.CompletedExact900 != 2 || d.CompletedLocalFailure != 2 ||
			d.ClaimVsExpiry != [5]int{3, 1, 1, 0, 0} || d.CompletionVsExpiry != [3]int{1, 0, 4} ||
			d.CompletionReceiveLagMaxSeconds != 5 || d.AttemptUpdateLagMaxSeconds != 7 {
			t.Fatalf("eligibility, measured quota, nearest expiry or comparison clocks drifted: fleet=%+v diagnostic=%+v", fleet, d)
		}
		if d.Attempt != [4]int{3, 2, 0, 0} || d.ExpiredAge != [3]int{2, 0, 0} || d.LatestAccepted != [3]int{3, 2, 0} {
			t.Fatalf("setup classes or accepted tie/expiry history drifted: %+v", d)
		}
	})
}

func TestUrlProbeMatureDeficitTimingBoundaries(t *testing.T) {
	value := func(v float64) *float64 { return &v }
	rows := []providerUrlProbeMatureDeficitRow{}
	for _, row := range []struct{ deadline, expired, claim float64 }{
		{0, 0, 0}, {90, 90, 450}, {360, 360, 721}, {900, 361, 361}, {901, 1, -1},
	} {
		rows = append(rows, providerUrlProbeMatureDeficitRow{Hint: true, DeadlineSeconds: row.deadline,
			ClaimState: 2, ClaimAge: value(row.claim), History: []providerUrlProbeMatureDeficitHistory{{AgeSeconds: 14400 + row.expired}}})
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	d := providerUrlProbeMatureDeficitDiagnostics(raw, 1, len(rows))
	if d.Deadline != [5]int{1, 1, 1, 1, 1} || d.ExpiredAge != [3]int{3, 1, 1} ||
		d.ClaimVsExpiry != [5]int{0, 1, 1, 1, 2} || d.ClaimClockFuture != 1 {
		t.Fatalf("inclusive deadline/expiry or six-minute renewal comparisons drifted: %+v", d)
	}
}
