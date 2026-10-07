// Test-only admission rewrites and finite plans distinguish local census cost
// from pool starvation. Synthetic populations never establish a Main plan.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

const testingUrlCensusProviderCount = 80000

// The bulk fixture helper must inherit the same finite setup owner.
type testingUrlCensusContext struct {
	testing.TB
	ctx context.Context
}

func (self testingUrlCensusContext) Context() context.Context {
	return self.ctx
}

// NOT EXISTS(bad) OR EXISTS(priority) is equivalent to excluding only a bad
// score whose client has no priority. The inner client key permits a distinct
// planner shape without changing any production source or admission floor.
func testingUrlCensusGuardedAdmissionSql(alias string) string {
	reliability := providerReliabilityEligibilitySql(alias + ".client_id")
	end := strings.LastIndex(reliability, "\n\t)")
	if end < 0 || end+len("\n\t)") != len(reliability) {
		panic("synthetic census admission anchor changed")
	}
	return "NOT " + alias + ".arin_risk AND " + reliability[:end] + "\n\t\tAND NOT (" +
		providerIntentProbePrioritySql("provider_reliability.client_id") + ")" + reliability[end:]
}

// Replace exactly one owning admission expression, retaining the four bound
// parameters, history window, mature partitions and bounded diagnostic head.
func testingUrlCensusGuardedQuery(t testing.TB) string {
	t.Helper()
	query := providerUrlProbeFleetSql(1)
	anchor := providerUrlProbeAdmissionSql("provider")
	if strings.Count(query, anchor) != 1 {
		t.Fatal("census no longer contains exactly one owning admission predicate")
	}
	return strings.Replace(query, anchor, testingUrlCensusGuardedAdmissionSql("provider"), 1)
}

// Each diagnostic read has a ten-second SQL cap and a twelve-second owner.
// A read-only transaction keeps settings local and rolls back within two more
// seconds; failed connection disposal retains the existing five-second bound.
func testingUrlCensusBoundedRead(t testing.TB, read func(context.Context, server.PgTx)) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	failure := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
			server.Raise(err)
			completed := false
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				defer cleanupCancel()
				cleanupErr := tx.Rollback(cleanupCtx)
				if completed {
					server.Raise(cleanupErr)
				}
			}()
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL statement_timeout='10s'"))
			server.RaisePgResult(tx.Exec(ctx, "SET LOCAL lock_timeout='500ms'"))
			read(ctx, tx)
			server.Raise(ctx.Err())
			completed = true
		}, server.OptNoRetry())
	})
	if failure == nil {
		return nil
	}
	if err, ok := failure.(error); ok {
		return err
	}
	return fmt.Errorf("synthetic census read failed: %v", failure)
}

func testingUrlCensusBoundedValues(t testing.TB, query string, now time.Time) ([]any, error) {
	t.Helper()
	var values []any
	err := testingUrlCensusBoundedRead(t, func(ctx context.Context, tx server.PgTx) {
		rows, err := tx.Query(ctx, query, now, ProvideModePublic, ProviderUrlProbeRunTarget, now.Add(-ProviderEgressProbeRefreshAge))
		server.Raise(err)
		defer rows.Close()
		if !rows.Next() {
			server.Raise(rows.Err())
			panic("synthetic census returned no row")
		}
		values, err = rows.Values()
		server.Raise(err)
		if rows.Next() {
			panic("synthetic census returned more than one row")
		}
		server.Raise(rows.Err())
	})
	return values, err
}

// Sixteen independent states cover missing, low, exact-floor and healthy
// scores with and without intent priority and ARIN exclusion. The historical
// no-grace predicate must differ in its one legitimately admitted low-score row.
func TestUrlProbeCensusGuardedAdmissionTruthAndQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlProbeFleet(t, now.Add(-5*time.Hour), 16)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `WITH numbered AS (
				SELECT client_id,row_number() OVER (ORDER BY client_id)-1 AS n FROM network_client
			) INSERT INTO client_connection_reliability_score
				(client_id,lookback_index,independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight)
				SELECT client_id,i,1,CASE WHEN n%4=1 AND i=1 THEN 0.94
					WHEN n%4=2 THEN CASE i WHEN 1 THEN 0.95 WHEN 2 THEN 0.7 ELSE 0.6 END ELSE 1 END,1,1
				FROM numbered CROSS JOIN generate_series(1,3) AS i WHERE n%4<>0`))
			server.RaisePgResult(tx.Exec(ctx, `WITH numbered AS (
				SELECT client_id,row_number() OVER (ORDER BY client_id)-1 AS n FROM network_client
			) INSERT INTO provider_intent_probe_priority(client_id,priority_since,update_time)
				SELECT client_id,$1,$1 FROM numbered WHERE n%8>=4`, now))
			server.RaisePgResult(tx.Exec(ctx, `WITH numbered AS (
				SELECT client_id,row_number() OVER (ORDER BY client_id)-1 AS n FROM network_client
			) UPDATE network_client_location_reliability AS provider SET arin_risk=numbered.n>=8
				FROM numbered WHERE numbered.client_id=provider.client_id`))
		})
		rowsSeen := 0
		err := testingUrlCensusBoundedRead(t, func(ctx context.Context, tx server.PgTx) {
			rows, err := tx.Query(ctx, `SELECT `+providerUrlProbeAdmissionSql("provider")+`,`+
				testingUrlCensusGuardedAdmissionSql("provider")+`,`+providerProbeEligibilitySql("provider")+
				` FROM network_client_location_reliability AS provider ORDER BY provider.client_id`)
			server.Raise(err)
			defer rows.Close()
			for rows.Next() {
				var current, guarded, historical bool
				server.Raise(rows.Scan(&current, &guarded, &historical))
				want := rowsSeen < 8 && (rowsSeen%4 != 1 || rowsSeen%8 >= 4)
				oldWant := rowsSeen < 8 && rowsSeen%4 != 1
				if current != want || guarded != want || historical != oldWant {
					panic(fmt.Sprintf("synthetic admission truth row %d differs: current=%t guarded=%t historical=%t", rowsSeen, current, guarded, historical))
				}
				rowsSeen++
			}
			server.Raise(rows.Err())
		})
		if err != nil || rowsSeen != 16 {
			t.Fatalf("bounded admission truth failed: rows=%d error=%v", rowsSeen, err)
		}
		current, currentErr := testingUrlCensusBoundedValues(t, providerUrlProbeFleetSql(1), now)
		guarded, guardedErr := testingUrlCensusBoundedValues(t, testingUrlCensusGuardedQuery(t), now)
		if currentErr != nil || guardedErr != nil || len(current) != 22 || !reflect.DeepEqual(current, guarded) {
			t.Fatalf("equivalent predicate changed a census/diagnostic field: current_error=%v guarded_error=%v", currentErr, guardedErr)
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		fleet := GetProviderUrlProbeFleet(readCtx, now)
		if fleet.Eligible != 7 || fleet.MatureEligible != 7 || fleet.QuotaComplete != 0 || fleet.MatureRunsNeeded != 70 {
			t.Fatalf("admission truth changed the typed quota: %+v", fleet)
		}
	})
}

// This fixture has a known near-current fleet size, not a claim about Main's
// distribution: 240k scores, 4k intent rows, 800k history rows and 160k security
// rows. Setup is outside query timing and has its own fixed two-minute owner.
func testingSeedUrlCensusDeadlineFleet(t testing.TB, now time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	testingSeedUrlProbeFleet(testingUrlCensusContext{TB: t, ctx: ctx}, now.Add(-5*time.Hour), testingUrlCensusProviderCount)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, "SET LOCAL statement_timeout='90s'"))
		server.RaisePgResult(tx.Exec(ctx, `WITH numbered AS (
			SELECT client_id,row_number() OVER (ORDER BY client_id) AS n FROM network_client
		) INSERT INTO client_connection_reliability_score
			(client_id,lookback_index,independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight)
			SELECT client_id,i,1,CASE WHEN n%10=0 AND i=1 THEN 0.1 ELSE 1 END,1,1
			FROM numbered CROSS JOIN generate_series(1,3) AS i`))
		server.RaisePgResult(tx.Exec(ctx, `WITH numbered AS (
			SELECT client_id,row_number() OVER (ORDER BY client_id) AS n FROM network_client
		) INSERT INTO provider_intent_probe_priority(client_id,priority_since,update_time)
			SELECT client_id,$1,$1 FROM numbered WHERE n%20=0`, now))
		server.RaisePgResult(tx.Exec(ctx, `WITH numbered AS (
			SELECT client_id,row_number() OVER (ORDER BY client_id) AS n FROM network_client
		) INSERT INTO provider_egress_health_history
			(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
			SELECT md5(client_id::text||'-census-credit-'||i)::uuid,client_id,
				CASE WHEN n%1000=0 AND i=10 THEN $1::timestamp-interval '4 hours 1 minute'
					ELSE $1::timestamp-interval '30 minutes'-i*interval '1 second' END,
				i%2,1,'{}',false,true,1 FROM numbered CROSS JOIN generate_series(1,10) AS i`, now))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health
			(client_id,measured_at,ok_count,total_count,class_results,reputation_ok,reputation_total)
			SELECT client_id,$1,1,1,'{}',0,0 FROM network_client`, now))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security
			(client_id,url_key,destination,measured_at,tls_failure)
			SELECT client_id,'synthetic-census','{}',$1,false FROM network_client`, now))
		for _, table := range []string{"client_connection_reliability_score", "provider_intent_probe_priority", "provider_egress_health_history", "provider_egress_health", "provider_egress_url_security"} {
			server.RaisePgResult(tx.Exec(ctx, "ANALYZE "+table))
		}
	}, server.OptNoRetry())
}

// Both modes retain the exact parameterized query, with a prepared-statement
// witness proving whether PostgreSQL used a custom or generic plan.
func testingUrlCensusDeadlinePlan(t testing.TB, label, mode, query string, now time.Time) (testingUrlCompletedPlan, error) {
	t.Helper()
	var plans []testingUrlCompletedPlan
	err := testingUrlCensusBoundedRead(t, func(ctx context.Context, tx server.PgTx) {
		if mode != "custom" && mode != "generic" {
			panic("unknown synthetic census plan mode")
		}
		server.RaisePgResult(tx.Exec(ctx, "SET LOCAL plan_cache_mode=force_"+mode+"_plan"))
		statement := `"fp2_census_` + server.NewId().String() + `"`
		server.RaisePgResult(tx.Exec(ctx, "PREPARE "+statement+" AS "+query))
		arguments := fmt.Sprintf("%s,%d,%d,%s", testingUrlCompletedTimestampLiteral(now), ProvideModePublic,
			ProviderUrlProbeRunTarget, testingUrlCompletedTimestampLiteral(now.Add(-ProviderEgressProbeRefreshAge)))
		var raw []byte
		server.Raise(tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE "+statement+"("+arguments+")").Scan(&raw))
		if len(raw) > 1024*1024 {
			panic("synthetic census plan exceeds one MiB")
		}
		server.Raise(json.Unmarshal(raw, &plans))
		var generic, custom int64
		server.Raise(tx.QueryRow(ctx, `SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1`, strings.Trim(statement, `"`)).Scan(&generic, &custom))
		server.RaisePgResult(tx.Exec(ctx, "DEALLOCATE "+statement))
		if mode == "generic" && (generic != 1 || custom != 0) || mode == "custom" && (custom != 1 || generic != 0) {
			panic(fmt.Sprintf("synthetic census plan mode differs: generic=%d custom=%d", generic, custom))
		}
		var compact strings.Builder
		for _, line := range strings.Split(string(raw), "\n") {
			compact.WriteString(strings.TrimSpace(line))
		}
		t.Logf("census %s %s EXPLAIN JSON: %s", mode, label, compact.String())
	})
	if err != nil {
		return testingUrlCompletedPlan{}, err
	}
	if len(plans) != 1 {
		return testingUrlCompletedPlan{}, fmt.Errorf("expected one census plan, got %d", len(plans))
	}
	return plans[0], nil
}

type testingUrlCensusDeadlineWork struct {
	ScoreRows, IntentRows, HistoryRows, Buffers float64
}

// Broad linear ceilings reject a repeated full-population subplan. They do
// not demand an index for a full census or equate buffers with execution time.
func testingCheckUrlCensusDeadlinePlan(plan testingUrlCompletedPlan, providers int) (testingUrlCensusDeadlineWork, error) {
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
	if work.ScoreRows > float64(12*providers) || work.IntentRows > float64(4*providers) ||
		work.HistoryRows > float64(12*providers+128*ProviderUrlProbeRunTarget) || work.Buffers > float64(128*providers) {
		return work, fmt.Errorf("census exceeded fixed linear fixture bounds: %+v", work)
	}
	return work, nil
}

// Counterfactual plans ensure the work gate cannot mistake zero output or a
// cheap-looking top node for a bounded repeated reliability/intent lookup.
func TestUrlProbeCensusPlanOracleRejectsRepeatedPopulation(t *testing.T) {
	providers := testingUrlCensusProviderCount
	healthy := testingUrlCompletedPlan{Plan: testingUrlCompletedPlanNode{
		NodeType: "Aggregate", ActualRows: 1, ActualLoops: 1, SharedHits: float64(16 * providers),
		Plans: []testingUrlCompletedPlanNode{
			{NodeType: "Seq Scan", RelationName: "client_connection_reliability_score", ActualRows: float64(3 * providers), ActualLoops: 1},
			{NodeType: "Seq Scan", RelationName: "provider_intent_probe_priority", ActualRows: float64(providers / 20), ActualLoops: 1},
			{NodeType: "Index Scan", RelationName: "provider_egress_health_history", ActualRows: 10, ActualLoops: float64(providers)},
		},
	}}
	if _, err := testingCheckUrlCensusDeadlinePlan(healthy, providers); err != nil {
		t.Fatalf("linear census rejected: %v", err)
	}
	for i := range healthy.Plan.Plans {
		mutant := healthy
		mutant.Plan.Plans = append([]testingUrlCompletedPlanNode{}, healthy.Plan.Plans...)
		mutant.Plan.Plans[i].ActualRows = 0
		mutant.Plan.Plans[i].RowsRemoved = float64(providers)
		mutant.Plan.Plans[i].ActualLoops = float64(providers)
		if _, err := testingCheckUrlCensusDeadlinePlan(mutant, providers); err == nil {
			t.Fatalf("work gate accepted repeated rejected population %s", mutant.Plan.Plans[i].RelationName)
		}
	}
	mutant := healthy
	mutant.Plan.SharedReads = float64(129 * providers)
	if _, err := testingCheckUrlCensusDeadlinePlan(mutant, providers); err == nil {
		t.Fatal("work gate accepted excessive buffer work")
	}
}

// ABBA ordering reduces simple cache/order confounding; every query retains its
// own work and SQL time. A local pass does not prove Main's data or pool is fast.
func TestUrlProbeCensus80kAdmissionPlansAndQuota(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		now := server.NowUtc().Truncate(time.Microsecond)
		testingSeedUrlCensusDeadlineFleet(t, now)
		current := providerUrlProbeFleetSql(1)
		guarded := testingUrlCensusGuardedQuery(t)
		for _, mode := range []string{"custom", "generic"} {
			for index, query := range []string{current, guarded, guarded, current} {
				label := []string{"current-a1", "guarded-b1", "guarded-b2", "current-a2"}[index]
				plan, err := testingUrlCensusDeadlinePlan(t, label, mode, query, now)
				if err != nil {
					t.Errorf("bounded %s %s census failed: %v", mode, label, err)
					continue
				}
				work, workErr := testingCheckUrlCensusDeadlinePlan(plan, testingUrlCensusProviderCount)
				t.Logf("census %s %s work=%+v planning_ms=%.3f execution_ms=%.3f", mode, label, work, plan.PlanningTime, plan.ExecutionTime)
				if workErr != nil {
					t.Errorf("%s %s: %v", mode, label, workErr)
				}
			}
		}
		currentValues, currentErr := testingUrlCensusBoundedValues(t, current, now)
		guardedValues, guardedErr := testingUrlCensusBoundedValues(t, guarded, now)
		if currentErr != nil || guardedErr != nil || len(currentValues) != 22 || !reflect.DeepEqual(currentValues, guardedValues) {
			t.Errorf("80k predicate changed census/diagnostic fields: current_error=%v guarded_error=%v", currentErr, guardedErr)
		}
		readCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		var fleet ProviderUrlProbeFleet
		err := server.HandleError(func() { fleet = GetProviderUrlProbeFleet(readCtx, now) })
		if err != nil || readCtx.Err() != nil {
			t.Errorf("actual model read exceeded or failed its ten-second owner: error=%v context=%v", err, readCtx.Err())
		} else if fleet.Eligible != 76000 || fleet.MatureEligible != 76000 || fleet.MatureQuotaComplete != 75920 ||
			fleet.MatureRunsNeeded != 80 || fleet.WarmingEligible != 0 || fleet.EligibilityAgeUnknown != 0 ||
			fleet.MatureDeficitDiagnostics.Selected != 80 || fleet.MatureDeficitDiagnostics.RunsNeeded != 80 {
			t.Errorf("80k bounded fixture changed exact quota and deficit selection: %+v", fleet)
		}
	})
}
