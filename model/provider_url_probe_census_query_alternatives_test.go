// These are counterfactual query controls, not a production query change.
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

// Retain the exact admission, security, maturity, aggregate and diagnostic
// expressions. Only the current per-provider latest-ten lookup is replaced.
// The materialized eligible set retains provider identity separately from the
// nullable cycle identity: missing cycles must remain in the census.
func testingUrlCensusGroupedHistoryQuery(t testing.TB, policy int) string {
	t.Helper()
	current := providerUrlProbeFleetSql(policy)
	boundary := ")" + providerUrlProbeMatureDeficitCtesSql(policy)
	if strings.Count(current, boundary) != 1 {
		t.Fatal("census cohort/diagnostic boundary changed")
	}
	head, tail, _ := strings.Cut(current, boundary)
	replace := func(old, next string) {
		if strings.Count(head, old) != 1 {
			t.Fatalf("census grouped-history anchor changed: %q", old)
		}
		head = strings.Replace(head, old, next, 1)
	}
	replace("WITH cohort AS MATERIALIZED (", "WITH admitted AS MATERIALIZED (")
	replace("SELECT cycle.client_id,cycle.cycle_started_at,cycle.next_attempt_at,recent.run_count,",
		"SELECT provider.client_id AS history_client_id,cycle.client_id,cycle.cycle_started_at,cycle.next_attempt_at,")
	replace("\n\t\t\tCROSS JOIN LATERAL ("+providerUrlProbeRunWindowSql("provider.client_id", "$1", policy)+") AS recent", "")
	return head + fmt.Sprintf(`), accepted_counts AS MATERIALIZED (
		SELECT history.client_id,LEAST(COUNT(*),%d)::integer AS run_count
		FROM provider_egress_health_history AS history
		JOIN admitted ON admitted.history_client_id=history.client_id
		WHERE history.url_probe AND history.url_probe_policy_version=%d
		AND history.total_count=1 AND (history.ok_count=0 OR history.ok_count=1)
		AND history.measured_at>$1::timestamp-interval '%d seconds'
		AND history.measured_at<=$1::timestamp
		GROUP BY history.client_id
	), cohort AS MATERIALIZED (
		SELECT admitted.client_id,admitted.cycle_started_at,admitted.next_attempt_at,
			COALESCE(accepted_counts.run_count,0) AS run_count,
			admitted.security_exception,admitted.unknown_security_target
		FROM admitted LEFT JOIN accepted_counts ON accepted_counts.client_id=admitted.history_client_id
	)`, ProviderUrlProbeRunTarget, policy, int(ProviderEgressProbeRefreshAge/time.Second)) +
		providerUrlProbeMatureDeficitCtesSql(policy) + tail
}

// An immutable duplicate run is one credit. Strict expiry, a future clock,
// alternate policy and setup-only/grouped reports
// remain excluded. Missing/future admission ages and security keep their exact
// existing partitions even when a provider has more than ten accepted runs.
func TestUrlProbeCensusGroupedHistoryTruthAndQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		ids := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 16)
		server.Tx(ctx, func(tx server.PgTx) {
			counts := []int{0, 1, 1, 9, 10, 11, 0, 5, 9, 10, 10, 20, 10, 10, 10, 10}
			for index, count := range counts {
				if count == 0 {
					continue
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					SELECT md5($1::text||'-grouped-truth-'||i)::uuid,$1::uuid,$2::timestamp-i*interval '1 second',
						CASE WHEN $4::integer=2 THEN 0 ELSE i%2 END,1,'{}',false,true,1
					FROM generate_series(1,$3::integer) AS i`, ids[index], now, count, index))
			}
			// Repeat exactly one committed run identity without adding credit.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
				VALUES(md5($1::text||'-grouped-truth-1')::uuid,$1::uuid,$2::timestamp-interval '1 second',1,1,'{}',false,true,1)
				ON CONFLICT(run_id) DO NOTHING`, ids[1], now))
			for index, row := range []struct {
				age               time.Duration
				ok, total, policy int
				url               bool
			}{
				{0, 1, 1, 1, true}, {4*time.Hour - time.Microsecond, 0, 1, 1, true},
				{4 * time.Hour, 0, 1, 1, true}, {-time.Microsecond, 1, 1, 1, true},
				{time.Minute, 1, 1, 2, true}, {time.Minute, 0, 0, 1, true},
				{time.Minute, 1, 2, 1, true}, {time.Minute, 2, 2, 1, true},
				{time.Minute, 1, 1, 1, false},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					VALUES(md5($1::text||'-grouped-boundary-'||$7::integer)::uuid,$1::uuid,$2,$3,$4,'{}',false,$5,$6)`,
					ids[6], now.Add(-row.age), row.ok, row.total, row.url, row.policy, index))
			}
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, ids[7]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2::timestamp-interval '4 hours'+interval '1 microsecond' WHERE client_id=$1`, ids[8], now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2::timestamp+interval '1 microsecond' WHERE client_id=$1`, ids[9], now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health
				(client_id,measured_at,ok_count,total_count,class_results,reputation_ok,reputation_total,legacy_tls_authentication_failure)
				VALUES($1,$2,1,1,'{}',0,0,true)`, ids[10], now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, ids[11]))
			for _, index := range []int{12, 13} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_connection_reliability_score
					(client_id,independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight,lookback_index)
					VALUES($1,0.1,0.1,1,1,1)`, ids[index]))
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_intent_probe_priority(client_id,priority_since,update_time) VALUES($1,$2,$2)`, ids[12], now))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=true WHERE client_id=$1`, ids[14]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET ipv4_proven=false,ipv6_proven=true WHERE client_id=$1`, ids[15]))
		})
		for _, policy := range []int{1, -1} {
			current, currentErr := testingUrlCensusBoundedValues(t, providerUrlProbeFleetSql(policy), now)
			grouped, groupedErr := testingUrlCensusBoundedValues(t, testingUrlCensusGroupedHistoryQuery(t, policy), now)
			if currentErr != nil || groupedErr != nil || len(current) != 22 || !reflect.DeepEqual(current, grouped) {
				t.Fatalf("grouped history changed exact22 fields for policy%d: current_error=%v grouped_error=%v", policy, currentErr, groupedErr)
			}
			if policy == -1 && current[8] != int64(0) {
				t.Fatal("invalid policy manufactured quota credit")
			}
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		fleet := GetProviderUrlProbeFleet(readCtx, now)
		if fleet.Eligible != 12 || fleet.QuotaComplete != 5 || fleet.Complete != 4 || fleet.RunsNeeded != 43 ||
			fleet.SecurityExceptions != 1 || fleet.SecurityUnknownTargets != 1 || fleet.MissingCycles != 1 ||
			fleet.MatureEligible != 9 || fleet.MatureQuotaComplete != 4 || fleet.MatureRunsNeeded != 37 ||
			fleet.WarmingEligible != 1 || fleet.WarmingQuotaComplete != 0 || fleet.WarmingRunsNeeded != 1 ||
			fleet.EligibilityAgeUnknown != 2 || fleet.AgeUnknownQuotaComplete != 1 || fleet.AgeUnknownRunsNeeded != 5 ||
			fleet.MatureDeficitDiagnostics.Selected != 5 || fleet.MatureDeficitDiagnostics.RunsNeeded != 37 {
			t.Fatalf("independent boundary fixture quota differs: %+v", fleet)
		}
		server.Tx(ctx, func(tx server.PgTx) { server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false`)) })
		current, currentErr := testingUrlCensusBoundedValues(t, providerUrlProbeFleetSql(1), now)
		grouped, groupedErr := testingUrlCensusBoundedValues(t, testingUrlCensusGroupedHistoryQuery(t, 1), now)
		if currentErr != nil || groupedErr != nil || len(current) != 22 || current[0] != int64(0) || !reflect.DeepEqual(current, grouped) {
			t.Fatalf("empty grouped census differs: current_error=%v grouped_error=%v", currentErr, groupedErr)
		}
	})
}

type testingUrlCensusAlternativePlan struct {
	testingUrlCompletedPlan
	JIT struct {
		Functions int `json:"Functions"`
		Timing    struct {
			Total float64 `json:"Total"`
		} `json:"Timing"`
	} `json:"JIT"`
}

// JIT and preparation settings are transaction-local; no pool/global setting
// changes. A separate prepared-statement witness certifies the selected mode.
func testingUrlCensusAlternativeExplain(t testing.TB, label, mode, jit, query string, now time.Time) (testingUrlCensusAlternativePlan, error) {
	t.Helper()
	var plans []testingUrlCensusAlternativePlan
	err := testingUrlCensusBoundedRead(t, func(ctx context.Context, tx server.PgTx) {
		if mode != "custom" && mode != "generic" || jit != "on" && jit != "off" {
			panic("unknown synthetic census setting")
		}
		server.RaisePgResult(tx.Exec(ctx, "SET LOCAL plan_cache_mode=force_"+mode+"_plan"))
		server.RaisePgResult(tx.Exec(ctx, "SET LOCAL jit="+jit))
		var actualJit string
		server.Raise(tx.QueryRow(ctx, "SHOW jit").Scan(&actualJit))
		if actualJit != jit {
			panic("census fixture JIT setting was not applied")
		}
		statement := `"fp2_census_alternative_` + server.NewId().String() + `"`
		server.RaisePgResult(tx.Exec(ctx, "PREPARE "+statement+" AS "+query))
		arguments := fmt.Sprintf("%s,%d,%d,%s", testingUrlCompletedTimestampLiteral(now), ProvideModePublic,
			ProviderUrlProbeRunTarget, testingUrlCompletedTimestampLiteral(now.Add(-ProviderEgressProbeRefreshAge)))
		var raw []byte
		server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE "+statement+"("+arguments+")").Scan(&raw))
		if len(raw) > 1024*1024 {
			panic("census alternative plan exceeds one MiB")
		}
		server.Raise(json.Unmarshal(raw, &plans))
		var generic, custom int64
		server.Raise(tx.QueryRow(ctx, `SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1`, strings.Trim(statement, `"`)).Scan(&generic, &custom))
		server.RaisePgResult(tx.Exec(ctx, "DEALLOCATE "+statement))
		if mode == "generic" && (generic != 1 || custom != 0) || mode == "custom" && (custom != 1 || generic != 0) {
			panic(fmt.Sprintf("census alternative plan mode differs: generic=%d custom=%d", generic, custom))
		}
		var compact strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			compact.WriteString(strings.TrimSpace(line))
		}
		t.Logf("census alternative %s %s jit=%s EXPLAIN JSON: %s", mode, label, jit, compact.String())
	})
	if err != nil {
		return testingUrlCensusAlternativePlan{}, err
	}
	if len(plans) != 1 || jit == "off" && plans[0].JIT.Functions != 0 {
		return testingUrlCensusAlternativePlan{}, fmt.Errorf("invalid census plan/JIT witness: plans=%d jit=%s", len(plans), jit)
	}
	return plans[0], nil
}

// Grouping may read more than ten history rows per provider; the exact fixture
// relation size is its bound. Count rejected rows and loop multiplication so
// low output cannot hide repeated scans. This is a finite experiment ceiling,
// not an improvement criterion or a bound on Main's immutable history size.
func testingCheckUrlCensusAlternativeWork(plan testingUrlCompletedPlan, providers, historyRows int) (testingUrlCensusDeadlineWork, error) {
	work := testingUrlCensusDeadlineWork{Buffers: plan.Plan.SharedHits + plan.Plan.SharedReads}
	testingWalkUrlCompletedPlan(plan.Plan, func(node testingUrlCompletedPlanNode) {
		rows := (node.ActualRows + node.RowsRemoved + node.RowsRechecked) * node.ActualLoops
		switch node.RelationName {
		case "client_connection_reliability_score":
			work.ScoreRows += rows
		case "provider_intent_probe_priority":
			work.IntentRows += rows
		case "provider_egress_health_history":
			work.HistoryRows += rows
		}
	})
	if plan.Plan.ActualRows != 1 || plan.Plan.ActualLoops != 1 || work.ScoreRows > float64(12*providers) ||
		work.IntentRows > float64(4*providers) || work.HistoryRows > float64(3*historyRows+128*ProviderUrlProbeRunTarget) ||
		work.Buffers > float64(128*providers+4*historyRows) {
		return work, fmt.Errorf("census alternative exceeded finite fixture work: %+v", work)
	}
	return work, nil
}

func TestUrlProbeCensusAlternativeWorkOracleRejectsAmplification(t *testing.T) {
	const providers, historyRows = 80000, 2000000
	valid := testingUrlCompletedPlan{Plan: testingUrlCompletedPlanNode{
		NodeType: "Aggregate", ActualRows: 1, ActualLoops: 1, SharedHits: 2000000,
		Plans: []testingUrlCompletedPlanNode{{RelationName: "provider_egress_health_history", ActualRows: historyRows, ActualLoops: 1}},
	}}
	if _, err := testingCheckUrlCensusAlternativeWork(valid, providers, historyRows); err != nil {
		t.Fatal(err)
	}
	for _, rows := range []testingUrlCompletedPlanNode{
		{RelationName: "provider_egress_health_history", ActualRows: 0, RowsRemoved: historyRows, ActualLoops: providers},
		{RelationName: "client_connection_reliability_score", ActualRows: providers, ActualLoops: providers},
		{RelationName: "provider_intent_probe_priority", ActualRows: 0, RowsRemoved: providers, ActualLoops: providers},
	} {
		bad := valid
		bad.Plan.Plans = []testingUrlCompletedPlanNode{rows}
		if _, err := testingCheckUrlCensusAlternativeWork(bad, providers, historyRows); err == nil {
			t.Fatalf("work oracle accepted repeated population %s", rows.RelationName)
		}
	}
	bad := valid
	bad.Plan.ActualRows = 0
	if _, err := testingCheckUrlCensusAlternativeWork(bad, providers, historyRows); err == nil {
		t.Fatal("work oracle accepted absent aggregate output")
	}
	bad = valid
	bad.Plan.SharedReads = 20000000
	if _, err := testingCheckUrlCensusAlternativeWork(bad, providers, historyRows); err == nil {
		t.Fatal("work oracle accepted excessive buffers")
	}
}

// Increase total history from800k to2m without changing quota:400k expired,
// 400k recent unrelated identities and400k extra accepted rows on100 already
// complete eligible providers. Unknown Main cardinality/retention and bloat
// remain outside this deliberately finite, adverse background control.
func testingAddUrlCensusHistoryBackground(t testing.TB, now time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, "SET LOCAL statement_timeout='90s'"))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
			(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
			SELECT md5(client_id::text||'-expired-background-'||i)::uuid,client_id,$1::timestamp-interval '48 hours',
				0,1,'{}',false,true,1 FROM network_client CROSS JOIN generate_series(1,5) AS i`, now))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
			(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
			SELECT md5('unrelated-background-run-'||i)::uuid,md5('unrelated-background-client-'||i)::uuid,
				$1::timestamp-interval '1 hour',0,1,'{}',false,true,1 FROM generate_series(1,400000) AS i`, now))
		server.RaisePgResult(tx.Exec(ctx, `WITH dense AS (SELECT client_id FROM network_client ORDER BY client_id LIMIT 100)
			INSERT INTO provider_egress_health_history
			(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
			SELECT md5(client_id::text||'-dense-background-'||i)::uuid,client_id,
				$1::timestamp-interval '1 hour'-i*interval '1 microsecond',i%2,1,'{}',false,true,1
				FROM dense CROSS JOIN generate_series(1,4000) AS i`, now))
		server.RaisePgResult(tx.Exec(ctx, "ANALYZE provider_egress_health_history"))
		var rows int
		server.Raise(tx.QueryRow(ctx, "SELECT COUNT(*) FROM provider_egress_health_history").Scan(&rows))
		if rows != 2000000 {
			panic(fmt.Sprintf("adverse census history fixture size differs: %d", rows))
		}
	}, server.OptNoRetry())
}

// The mirrored eight-plan sequence contains separate ABBA comparisons for
// current/grouped SQL, current JIT on/off and grouped JIT on/off in both custom
// and generic modes. Background repeats only the SQL ABBA (eight more plans).
// Times/buffers are reported independently; a passing finite work gate does
// not select a production optimization or establish Main's latency cause.
func TestUrlProbeCensus80kGroupedHistoryAndJitPlans(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlCensusDeadlineFleet(t, now)
		current := providerUrlProbeFleetSql(1)
		grouped := testingUrlCensusGroupedHistoryQuery(t, 1)
		baseline, baselineErr := testingUrlCensusBoundedValues(t, current, now)
		if baselineErr != nil || len(baseline) != 22 {
			t.Fatalf("current census fixture failed: %v", baselineErr)
		}
		for _, background := range []bool{false, true} {
			historyRows := 800000
			if background {
				testingAddUrlCensusHistoryBackground(t, now)
				historyRows = 2000000
			}
			for _, query := range []string{current, grouped} {
				values, err := testingUrlCensusBoundedValues(t, query, now)
				if err != nil || len(values) != 22 || !reflect.DeepEqual(baseline, values) {
					t.Fatalf("history background=%t changed exact22 census/diagnostic fields: %v", background, err)
				}
			}
			type sample struct{ label, query, jit string }
			sequence := []sample{{"current-on-a1", current, "on"}, {"grouped-on-b1", grouped, "on"},
				{"grouped-off-c1", grouped, "off"}, {"current-off-d1", current, "off"},
				{"current-off-d2", current, "off"}, {"grouped-off-c2", grouped, "off"},
				{"grouped-on-b2", grouped, "on"}, {"current-on-a2", current, "on"}}
			if background {
				sequence = []sample{sequence[0], sequence[1], sequence[6], sequence[7]}
			}
			for _, mode := range []string{"custom", "generic"} {
				for _, sample := range sequence {
					label := fmt.Sprintf("background=%t/%s", background, sample.label)
					plan, err := testingUrlCensusAlternativeExplain(t, label, mode, sample.jit, sample.query, now)
					if err != nil {
						t.Errorf("bounded census %s %s failed: %v", mode, label, err)
						continue
					}
					work, workErr := testingCheckUrlCensusAlternativeWork(plan.testingUrlCompletedPlan, testingUrlCensusProviderCount, historyRows)
					t.Logf("census alternative %s %s jit=%s work=%+v planning_ms=%.3f execution_ms=%.3f jit_functions=%d jit_ms=%.3f",
						mode, label, sample.jit, work, plan.PlanningTime, plan.ExecutionTime, plan.JIT.Functions, plan.JIT.Timing.Total)
					if workErr != nil {
						t.Errorf("%s %s: %v", mode, label, workErr)
					}
				}
			}
		}
	})
}
