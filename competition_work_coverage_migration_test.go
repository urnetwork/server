// These guards add provenance custody without selecting the unresolved numeric
// tolerance or rewriting a historical scorer's eligibility interpretation.
package server

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Every body has a single exact monitor owner; this migration creates only new
// functions and triggers and never updates previously stored evidence.
func TestCompetitionWorkCoverageMigrationIsAdditive(t *testing.T) {
	for _, forbidden := range []string{"CREATE OR REPLACE", "UPDATE competition_", "DELETE FROM", "ALTER TABLE", "maximum_page_coverage_loss", "maximum_byte_coverage_loss"} {
		if strings.Contains(competitionWorkCoverageMigration, forbidden) {
			t.Errorf("coverage migration mutates history or selects provisional policy: %s", forbidden)
		}
	}
	bodies := CompetitionWorkCoverageBodiesSql()
	if len(bodies) != 9 || strings.Count(competitionWorkCoverageMigration, "CREATE TRIGGER ") != 6 {
		t.Fatal("coverage function/trigger census differs from custody contract")
	}
	for name, body := range bodies {
		if strings.TrimSpace(body) == "" {
			t.Fatalf("empty coverage %s body", name)
		}
	}
}

// The real published v1/v2 eligibility functions, not approval stubs, are used
// beneath the new wrapper. Tests execute only in the existing private fixture.
func TestCompetitionWorkCoverageDatabaseRejectsDowngradeAndPolicySubstitution(t *testing.T) {
	production := "CREATE FUNCTION competition_v2_score_eligible(score jsonb, baseline_raw double precision, margin double precision) RETURNS boolean LANGUAGE sql IMMUTABLE AS $eligible$" + strings.Split(competitionAutomationMigration, "$eligible$")[1] + "$eligible$;"
	staging := "CREATE FUNCTION competition_v2_score_eligible_mode(score jsonb, baseline_raw double precision, margin double precision, staging_mode boolean) RETURNS boolean LANGUAGE sql IMMUTABLE AS $eligible$" + strings.Split(competitionAutomationStagingMigration, "$eligible$")[1] + "$eligible$;"
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		Db(tb.Context(), func(conn PgConn) {
			RaisePgResult(conn.Exec(tb.Context(), competitionWorkCoverageSqlFixture+production+staging+competitionWorkCoverageMigration+competitionWorkCoverageSqlAssertions, pgx.QueryExecModeSimpleProtocol))
		}, OptReadWrite(), OptNoRetry())
	})
}

// Minimal tables isolate this additive guard's independent contribution. The
// established source/review/lease guards retain their own composed fixtures.
const competitionWorkCoverageSqlFixture = `
CREATE TABLE competition_round(round_id uuid,competition_id text,staging boolean,policy_json jsonb);
CREATE TABLE competition_job(job_id uuid,round_id uuid,state text,score_json jsonb);
CREATE TABLE competition_round_baseline(round_id uuid,baseline_json bytea);
CREATE TABLE competition_evaluation_seal(round_id uuid,seal_json jsonb);
CREATE TABLE competition_integrity_review(review_id uuid,round_id uuid,job_id uuid);
CREATE TABLE competition_executor_equivalence(round_id uuid,receipt_json jsonb);
CREATE TABLE competition_source_activation(environment text,competition_id text,season_id text,generation bigint,target_epoch integer,intent_json jsonb,activated_at timestamp);
CREATE TABLE competition_source_pointer(environment text,competition_id text,season_id text,generation bigint);
INSERT INTO competition_round VALUES('00000000-0000-0000-0000-000000000001','synthetic-coverage',false,'{"evaluation_policy":{"takeover_margin":0.05}}');
CREATE FUNCTION coverage_test_reject(statement text) RETURNS void LANGUAGE plpgsql AS $reject$
BEGIN
 BEGIN EXECUTE statement; EXCEPTION WHEN raise_exception THEN RETURN; END;
 RAISE EXCEPTION 'coverage guard unexpectedly accepted: %',statement;
END $reject$;
`

const competitionWorkCoverageSqlAssertions = `
DO $coverage_test$
DECLARE policy jsonb; evaluation jsonb; score jsonb; historical jsonb; altered jsonb; failing jsonb; baseline jsonb; configured jsonb; intent jsonb; entry jsonb; receipt jsonb; metrics jsonb;
 legacy_round uuid:='00000000-0000-0000-0000-000000000001'; coverage_round uuid:='00000000-0000-0000-0000-000000000002';
BEGIN
 -- Synthetic opaque policy values exercise full-object equality without
 -- prescribing the not-yet-approved production tolerance field names/values.
 policy:='{"kind":"seeded-crawl-coverage/v1","synthetic_threshold":0.037,"synthetic_rule":{"basis":"control"}}';
 evaluation:=jsonb_build_object('takeover_margin',0.05,'g2_volume_policy','minimum-work/1','work_coverage_policy',policy);
 historical:='{"score_schema":1,"raw_score":90,"placeable":true,"takeover_eligible":true,"significance":{"statistically_significant":true,"recommended_next_epoch_takeover_margin_supported":true,"replicate_count":9,"takeover_margin_percent":5},"gates":{"G1_success":{"passed":true},"G2_volume":{"passed":true},"G3_path_integrity":{"passed":true},"G4_matchmaking":{"passed":true},"G5_stability":{"passed":true},"G6_resources":{"passed":true}}}';
 IF NOT competition_v2_score_eligible_policy(historical,100,0.05,false,'{}')
  OR NOT competition_v2_score_eligible_policy(historical||'{"diagnostics":{"g2_volume_policy":"minimum-work/1","scorer_version":"sim-latency-score/2"}}',100,0.05,false,'{"g2_volume_policy":"minimum-work/1"}') THEN
  RAISE EXCEPTION 'historical scorer compatibility lost';
 END IF;
 INSERT INTO competition_job VALUES('00000000-0000-0000-0000-000000000011',legacy_round,'succeeded',historical);
 INSERT INTO competition_executor_equivalence VALUES(legacy_round,'{"synthetic_legacy_receipt":true}');
 INSERT INTO competition_round_baseline VALUES(legacy_round,convert_to('{"scorer_version":"sim-latency-score/1","replicates":[{"raw_score":100}]}','UTF8'));
 INSERT INTO competition_source_activation VALUES('synthetic','synthetic-coverage','synthetic',1,1,'{"target_epoch":1}',now());
 INSERT INTO competition_source_pointer VALUES('synthetic','synthetic-coverage','synthetic',1);
 configured:=jsonb_build_object('target_epoch',2,'work_coverage_policy',policy);
 PERFORM set_config('urnetwork.competition_v2_instrument_approval_json',configured::text,true);
 intent:=jsonb_build_object('target_epoch',2,'g2_volume_policy','minimum-work/1','work_coverage_policy',policy,'trusted_instrument',jsonb_build_object('approval',configured,'build_receipt',jsonb_build_object('work_coverage_policy',policy)));
 FOR altered IN SELECT intent-'work_coverage_policy'
  UNION ALL SELECT jsonb_set(intent,'{work_coverage_policy}','null')
  UNION ALL SELECT intent-'trusted_instrument'
  UNION ALL SELECT jsonb_set(intent,'{work_coverage_policy,synthetic_threshold}','0.04')
  UNION ALL SELECT jsonb_set(intent,'{trusted_instrument,build_receipt,work_coverage_policy,synthetic_rule,basis}','"candidate"')
  UNION ALL SELECT jsonb_set(intent,'{trusted_instrument,approval,work_coverage_policy,synthetic_threshold}','0.04') LOOP
  PERFORM coverage_test_reject(format('INSERT INTO competition_source_activation VALUES(%L,%L,%L,2,2,%L::jsonb,NULL)','synthetic','synthetic-coverage','synthetic',altered));
 END LOOP;
 INSERT INTO competition_source_activation VALUES('synthetic','synthetic-coverage','synthetic',2,2,intent,now());
 UPDATE competition_source_pointer SET generation=2;
 FOR altered IN SELECT evaluation-'work_coverage_policy'
  UNION ALL SELECT jsonb_set(evaluation,'{work_coverage_policy,synthetic_threshold}','0.04')
  UNION ALL SELECT evaluation-'g2_volume_policy' LOOP
  PERFORM coverage_test_reject(format('INSERT INTO competition_round VALUES(%L,%L,false,%L::jsonb)',coverage_round,'synthetic-coverage',jsonb_build_object('scorer_version','sim-latency-score/3','evaluation_policy',altered,'automation_policy',jsonb_build_object('enabled',true,'environment','synthetic','season_id','synthetic','source_activation_generation',2))));
 END LOOP;
 entry:=jsonb_build_object('scorer_version','sim-latency-score/3','evaluation_policy',evaluation,'automation_policy',jsonb_build_object('enabled',true,'environment','synthetic','season_id','synthetic','source_activation_generation',2));
 FOR altered IN SELECT entry-'scorer_version' UNION ALL SELECT jsonb_set(entry,'{scorer_version}','"sim-latency-score/2"') LOOP
  PERFORM coverage_test_reject(format('INSERT INTO competition_round VALUES(%L,%L,false,%L::jsonb)',coverage_round,'synthetic-coverage',altered));
 END LOOP;
 INSERT INTO competition_round VALUES(coverage_round,'synthetic-coverage',false,entry);
 PERFORM coverage_test_reject(format('UPDATE competition_round SET policy_json=jsonb_set(policy_json,''{scorer_version}'',''"sim-latency-score/2"'') WHERE round_id=%L',coverage_round));
 SELECT jsonb_agg(jsonb_build_object('metric',name,'baseline_values','[1,1,1,1,1,1,1,1,1]'::jsonb,'candidate_values','[1,1,1,1,1,1,1,1,1]'::jsonb,'baseline_sample_variance',0,'candidate_sample_variance',0,'difference_ci_low',0,'difference_ci_high',0)) INTO metrics
  FROM unnest(ARRAY['raw_score','request_count','received_bytes','ttfb_p50_ms','ttfb_p95_ms','throughput_p50_bytes_per_s','throughput_p95_bytes_per_s','page_coverage','byte_coverage']) name;
 receipt:=jsonb_build_object('work_coverage_policy',policy,'metrics',metrics);
 FOR altered IN SELECT receipt-'work_coverage_policy'
  UNION ALL SELECT jsonb_set(receipt,'{work_coverage_policy,synthetic_threshold}','0.04')
  UNION ALL SELECT jsonb_set(receipt,'{metrics}',metrics-8-7)
  UNION ALL SELECT jsonb_set(receipt,'{metrics,8,metric}','"page_coverage"')
  UNION ALL SELECT jsonb_set(receipt,'{metrics,8,baseline_values}','[1,1,1,1,1,1,1,1]')
  UNION ALL SELECT jsonb_set(receipt,'{metrics,7,candidate_values,0}','"1"')
  UNION ALL SELECT receipt#-'{metrics,8,baseline_sample_variance}' LOOP
  PERFORM coverage_test_reject(format('INSERT INTO competition_executor_equivalence VALUES(%L,%L::jsonb)',coverage_round,altered));
 END LOOP;
 INSERT INTO competition_executor_equivalence VALUES(coverage_round,receipt);
 PERFORM coverage_test_reject(format('UPDATE competition_round SET policy_json=policy_json #- ''{evaluation_policy,work_coverage_policy}'' WHERE round_id=%L',coverage_round));
 baseline:=jsonb_build_object('scorer_version','sim-latency-score/3','g2_volume_policy','minimum-work/1','work_coverage_policy',policy,'replicates',jsonb_build_array(jsonb_build_object('raw_score',100,'work_coverage',jsonb_build_object('synthetic_summary',true))));
 FOR altered IN SELECT baseline-'work_coverage_policy'
  UNION ALL SELECT jsonb_set(baseline,'{scorer_version}','"sim-latency-score/2"')
  UNION ALL SELECT jsonb_set(baseline,'{work_coverage_policy,synthetic_threshold}','0.04')
  UNION ALL SELECT jsonb_set(baseline,'{replicates}','[]')
  UNION ALL SELECT baseline#-'{replicates,0,work_coverage}' LOOP
  PERFORM coverage_test_reject(format('INSERT INTO competition_round_baseline VALUES(%L,convert_to(%L,''UTF8''))',coverage_round,altered));
 END LOOP;
 INSERT INTO competition_round_baseline VALUES(coverage_round,convert_to(baseline::text,'UTF8'));
 score:=historical||jsonb_build_object('diagnostics',jsonb_build_object('scorer_version','sim-latency-score/3','g2_volume_policy','minimum-work/1','work_coverage_policy',policy));
 score:=jsonb_set(score,'{gates,G7_work_coverage}',jsonb_build_object('passed',true,'details',jsonb_build_object('policy',policy)));
 IF NOT competition_v2_score_eligible_policy(score,100,0.05,false,evaluation)
  OR competition_work_coverage_score_valid(score,'{}')
  OR NOT competition_v2_score_eligible_policy(jsonb_set(score,'{significance,takeover_margin_percent}','0'),100,0,true,evaluation) THEN
  RAISE EXCEPTION 'positive coverage or exact-version control failed';
 END IF;
 INSERT INTO competition_job VALUES('00000000-0000-0000-0000-000000000012',coverage_round,'succeeded',score);
 FOR altered IN SELECT score#-'{gates,G7_work_coverage}'
  UNION ALL SELECT score#-'{diagnostics,work_coverage_policy}'
  UNION ALL SELECT jsonb_set(score,'{diagnostics,scorer_version}','"sim-latency-score/2"')
  UNION ALL SELECT jsonb_set(score,'{diagnostics,work_coverage_policy,synthetic_threshold}','0.04')
  UNION ALL SELECT jsonb_set(score,'{gates,G7_work_coverage,details,policy,synthetic_rule,basis}','"candidate"')
  UNION ALL SELECT score#-'{gates,G7_work_coverage,details,policy}'
  UNION ALL SELECT jsonb_set(score,'{gates,G7_work_coverage,passed}','null')
  UNION ALL SELECT jsonb_set(score,'{gates,G7_work_coverage,passed}','"true"')
  UNION ALL SELECT jsonb_set(score,'{gates,G7_work_coverage,passed}','false')
  UNION ALL SELECT historical LOOP
  IF competition_work_coverage_score_valid(altered,evaluation) OR competition_v2_score_eligible_policy(altered,100,0.05,false,evaluation)
   OR competition_v2_score_eligible_policy(jsonb_set(altered,'{significance,takeover_margin_percent}','0'),100,0,true,evaluation) THEN
   RAISE EXCEPTION 'downgraded/substituted score passed helper';
  END IF;
  PERFORM coverage_test_reject(format('INSERT INTO competition_job VALUES(NULL,%L,''succeeded'',%L::jsonb)',coverage_round,altered));
  PERFORM coverage_test_reject(format('UPDATE competition_job SET score_json=%L::jsonb WHERE round_id=%L',altered,coverage_round));
  entry:=jsonb_build_object('state','succeeded','eligible',true,'score',altered);
  PERFORM coverage_test_reject(format('INSERT INTO competition_evaluation_seal VALUES(%L,%L::jsonb)',coverage_round,jsonb_build_object('entries',jsonb_build_array(entry))));
 END LOOP;
 PERFORM coverage_test_reject(format('INSERT INTO competition_job VALUES(NULL,%L,''succeeded'',NULL)',coverage_round));
 failing:=jsonb_set(score,'{gates,G7_work_coverage,passed}','false')||'{"placeable":false,"takeover_eligible":false}';
 IF NOT competition_work_coverage_score_valid(failing,evaluation) OR competition_v2_score_eligible_policy(failing,100,0.05,false,evaluation) THEN
  RAISE EXCEPTION 'honest failed G7 must be recordable and ineligible';
 END IF;
 INSERT INTO competition_job VALUES(NULL,coverage_round,'succeeded',failing);
 INSERT INTO competition_evaluation_seal VALUES(coverage_round,jsonb_build_object('entries',jsonb_build_array(jsonb_build_object('state','succeeded','eligible',false,'score',failing))));
 PERFORM coverage_test_reject(format('INSERT INTO competition_evaluation_seal VALUES(%L,%L::jsonb)',coverage_round,jsonb_build_object('entries',jsonb_build_array(jsonb_build_object('state','succeeded','eligible',true,'score',failing)))));
 INSERT INTO competition_evaluation_seal VALUES(coverage_round,jsonb_build_object('entries',jsonb_build_array(jsonb_build_object('state','succeeded','eligible',true,'score',score))));
 -- The first transition may promote a genuinely evaluated historical score;
 -- a later source cannot promote a failed/missing G7 from a coverage round.
 INSERT INTO competition_integrity_review VALUES('00000000-0000-0000-0000-000000000021',coverage_round,'00000000-0000-0000-0000-000000000012');
 intent:=intent-'trusted_instrument'||jsonb_build_object('target_epoch',3,'review_id','00000000-0000-0000-0000-000000000021');
 INSERT INTO competition_source_activation VALUES('synthetic','synthetic-coverage','synthetic',3,3,intent,NULL);
 UPDATE competition_job SET score_json=failing WHERE job_id='00000000-0000-0000-0000-000000000012';
 PERFORM coverage_test_reject(format('UPDATE competition_source_activation SET activated_at=now() WHERE generation=3'));
 PERFORM coverage_test_reject(format('INSERT INTO competition_source_activation VALUES(%L,%L,%L,4,3,%L::jsonb,NULL)','synthetic','synthetic-coverage','synthetic',intent-'work_coverage_policy'));
 PERFORM set_config('urnetwork.competition_v2_instrument_approval_json','',true);
 PERFORM coverage_test_reject(format('INSERT INTO competition_source_activation VALUES(%L,%L,%L,4,3,%L::jsonb,NULL)','synthetic','synthetic-coverage','synthetic',intent-'work_coverage_policy'));
END $coverage_test$;
`
