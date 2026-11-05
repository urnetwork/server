// New guard tests use only synthetic scope/source objects in the existing
// networkless PostgreSQL harness. No installed or live database is consulted.
package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Published legacy bodies remain exact nested blocks; unrelated source and
// provider functions are never recreated by this candidate migration.
func TestCompetitionIntakeFirstMigrationRetainsExactLegacyBranches(t *testing.T) {
	sql := CompetitionIntakeFirstMigrationSql()
	round, reserved, baseline := CompetitionIntakeFirstLegacyBodiesSql()
	for _, body := range []string{round, reserved, baseline, CompetitionIntakeFirstLegacyCoverageRoundBodySql()} {
		if strings.Count(sql, body+";\nEND ") != 1 {
			t.Fatal("intake-first changed or omitted an exact published legacy branch")
		}
	}
	for _, forbidden := range []string{"CREATE OR REPLACE FUNCTION competition_v2_source_authorized", "CREATE OR REPLACE FUNCTION competition_v2_winner_source_authorized", "CREATE OR REPLACE FUNCTION competition_v2_source_pointer_guard", "UPDATE competition_source_pointer", "maximum_page_coverage_loss", "minimum_page_coverage_ratio", "minimum_byte_coverage_ratio"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("intake-first rewrote independent authority or interpreted opaque numerical rules: %s", forbidden)
		}
	}
	for _, required := range []string{"grant_canonical", "round_policy_canonical", "api_readiness_canonical", "request_canonical", "competition_source_handoff_capability", "competition_v2_winner_source_authorized(product,predecessor)", "competition_finalization_touch_scope", "competition_v2_intake_grading_ready", "j.attempt_count=0 AND j.started_at IS NULL", "transaction_timestamp()", "interval '7 days'"} {
		if !strings.Contains(sql, required) {
			t.Fatalf("intake-first omitted mandatory authority %s", required)
		}
	}
	functions := CompetitionIntakeFirstFunctionsSql()
	if len(functions) != 16 {
		t.Fatal("intake-first exact function census changed")
	}
	seen := map[string]bool{}
	for _, function := range functions {
		if function.Body == "" || function.Result == "" || seen[function.Signature] {
			t.Fatal("intake-first exported incomplete or duplicate catalog custody")
		}
		seen[function.Signature] = true
	}
}

// The fixture installs the real current source-mode, ten-repository, Snow
// winner, trusted-instrument and lease helpers; no helper fabricates approval.
func TestCompetitionIntakeFirstActualSqlSeparatesAdmissionAndExecution(t *testing.T) {
	mode := strings.Split(competitionAutomationStagingSourceGraphMigration, "$mode$")[1]
	graph, _ := CompetitionSourceRetirementBodiesSql()
	_, _, winner, _, _, _, _, _ := CompetitionSnowTwoMemberWinnerReviewBodiesSql()
	scope, _, _, _, _, _, _ := CompetitionFinalizationHoldBodiesSql()
	lease := strings.Split(competitionAutomationGuardMigration, "$lease$")[1]
	helpers := fmt.Sprintf(`
CREATE FUNCTION competition_v2_scope_lock(env text,competition text,season text) RETURNS void LANGUAGE sql AS $$ SELECT pg_advisory_xact_lock(hashtextextended('competition-automation:'||env||':'||competition||':'||season,0)) $$;
CREATE FUNCTION competition_v2_require_source_lease(env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS $fixture_lease$%s$fixture_lease$;
CREATE FUNCTION competition_finalization_touch_scope(env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS $fixture_scope$%s$fixture_scope$;
CREATE FUNCTION competition_v2_source_graph_valid(commits jsonb,staging_mode boolean) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $fixture_graph$%s$fixture_graph$;
CREATE FUNCTION competition_v2_source_mode(intent jsonb) RETURNS void LANGUAGE plpgsql AS $fixture_mode$%s$fixture_mode$;
CREATE FUNCTION competition_v2_winner_source_authorized(intent jsonb,predecessor_id uuid) RETURNS boolean LANGUAGE plpgsql AS $fixture_winner$%s$fixture_winner$;
`, lease, scope, graph, mode, winner)
	// Run directly in TestEnv's isolated database. This test must not require
	// host Docker privileges or silently skip behind a native-helper flag.
	coveragePolicy := CompetitionWorkCoverageBodiesSql()["policy"]
	setup := competitionIntakeFirstSqlFixture + helpers + competitionTrustedInstrumentMigration() +
		"CREATE FUNCTION competition_work_coverage_policy_valid(policy jsonb) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $fixture_coverage$" + coveragePolicy + "$fixture_coverage$;\n" + CompetitionIntakeFirstMigrationSql()
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		Db(tb.Context(), func(conn PgConn) {
			RaisePgResult(conn.Exec(tb.Context(), setup+competitionIntakeFirstSqlAssertions, pgx.QueryExecModeSimpleProtocol))
		}, OptReadWrite(), OptNoRetry())
	})
}

const competitionIntakeFirstSqlFixture = `
CREATE TABLE competition_round(round_id uuid PRIMARY KEY,competition_id text,staging boolean NOT NULL DEFAULT false,epoch_number integer,policy_json jsonb,providers_sha256 text,
 opens_at timestamp,closes_at timestamp,reveal_at timestamp,canceled boolean NOT NULL DEFAULT false,finalized_at timestamp,winner_job_id uuid,admission_closed_at timestamp,automation_sealed_at timestamp,automation_publication_at timestamp);
CREATE TABLE competition_source_activation(environment text,competition_id text,season_id text,generation bigint,target_epoch integer,intent_json jsonb,activated_at timestamp,abandoned_at timestamp,PRIMARY KEY(environment,competition_id,season_id,generation));
CREATE TABLE competition_source_pointer(environment text,competition_id text,season_id text,generation bigint,PRIMARY KEY(environment,competition_id,season_id));
CREATE TABLE competition_source_readiness(environment text,competition_id text,season_id text,generation bigint,role text,receipt_json jsonb,created_at timestamp,PRIMARY KEY(environment,competition_id,season_id,generation,role));
CREATE TABLE competition_integrity_review(review_id uuid,round_id uuid,job_id uuid,request_sha256 text,review_json jsonb,review_cursor bigserial,created_at timestamp);
CREATE TABLE competition_reward_decision(round_id uuid,revision integer,decision_json jsonb);
CREATE TABLE competition_supervisor_transition(environment text,competition_id text,season_id text,target_epoch integer,revision integer,state text,transition_json jsonb,updated_at timestamp);
CREATE TABLE competition_supervisor_lease(environment text,competition_id text,season_id text,owner_id text,fencing_token bigint,lease_expires_at timestamp);
CREATE TABLE competition_finalization_scope_fence(environment text,competition_id text,season_id text,generation bigint,PRIMARY KEY(environment,competition_id,season_id));
CREATE TABLE competition_automation_hold(environment text,competition_id text,season_id text,kind text,revision integer,active boolean);
CREATE TABLE competition_automation_incident(round_id uuid,invalidates_evaluation boolean);
CREATE TABLE competition_evaluation_seal(round_id uuid,seal_json jsonb);
CREATE TABLE competition_job(job_id uuid PRIMARY KEY,round_id uuid,state text,attempt_count integer DEFAULT 0,started_at timestamp,evaluation_cycle integer DEFAULT 0,cycle_started_at timestamp,score_json jsonb);
CREATE TABLE competition_worker_slot(slot_id integer PRIMARY KEY,job_id uuid,worker_id text,lease_expires_at timestamp);
CREATE TABLE competition_job_event(job_id uuid,actor_id text,event_type text,payload_json jsonb);
CREATE TABLE competition_round_baseline(round_id uuid PRIMARY KEY,baseline_json bytea,baseline_sha256 text,source_job_id uuid,source_attempt integer,source_artifact_manifest_sha256 text,
 base_sha text,providers_sha256 text,evaluator_image_digest text,scorer_version text,hardware_id text,host_qualification_sha256 text,created_at timestamp);
CREATE TABLE competition_executor_qualification_run(round_id uuid,host_id text,job_id uuid,started_at timestamp,deadline_at timestamp);
CREATE TABLE competition_executor_equivalence(round_id uuid,host_id text,receipt_json jsonb,baseline_sha256 text,created_at timestamp);
CREATE FUNCTION competition_append_only_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'competition append-only record changed'; END $$;
CREATE FUNCTION synthetic_expect_intake_failure(statement text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 BEGIN EXECUTE statement; EXCEPTION WHEN OTHERS THEN RETURN; END;
 RAISE EXCEPTION 'synthetic forbidden intake statement succeeded: %',statement;
END $$;
CREATE TEMP TABLE synthetic_intake_fixture(intent jsonb,policy jsonb,api jsonb,grant_value jsonb,request jsonb,handoff jsonb);
`

const competitionIntakeFirstSqlAssertions = `
CREATE TRIGGER competition_v2_round_source BEFORE INSERT ON competition_round FOR EACH ROW EXECUTE FUNCTION competition_v2_round_insert_guard();
CREATE TRIGGER competition_v2_round_reserved_identity BEFORE INSERT ON competition_round FOR EACH ROW EXECUTE FUNCTION competition_v2_round_reserved_identity_guard();
CREATE TRIGGER competition_round_baseline_source_guard BEFORE INSERT ON competition_round_baseline FOR EACH ROW EXECUTE FUNCTION competition_round_baseline_insert_guard();
CREATE TRIGGER competition_work_coverage_round BEFORE INSERT OR UPDATE ON competition_round FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_round_guard();
BEGIN;
DO $fixture$
DECLARE commits jsonb; refs jsonb; lock_digest text; intent jsonb; policy jsonb; api jsonb; grant_value jsonb; request jsonb; handoff jsonb; at_time timestamp;
BEGIN
 at_time:=timezone('UTC',clock_timestamp());
 SELECT jsonb_object_agg(name,repeat('a',40)) INTO commits FROM unnest(ARRAY['server','connect','sdk','proxy','glog','goidenticons','userwireguard','sn','warp','gvisor']) name;
 lock_digest:=competition_v2_instrument_lock_digest(commits);
 SELECT jsonb_object_agg(name,jsonb_build_object('branch','sim-latency','expected_commit',repeat('a',40),'commit',repeat('a',40))) INTO refs FROM jsonb_object_keys(commits) name;
 refs:=refs||jsonb_build_object('config',jsonb_build_object('branch','main','expected_commit',repeat('b',40),'commit',repeat('c',40)));
 intent:=jsonb_build_object('environment','synthetic','competition_id','synthetic-competition','season_id','synthetic-season','generation',2,'expected_generation',1,
  'source_generation',0,'target_epoch',2,'target_round_id','00000000-0000-0000-0000-000000000002','kind','reviewed_source_carry_forward','reason','no_reward_winner',
  'commits',commits,'source_lock_sha256',lock_digest,'base_source_lock_sha256',lock_digest,'threshold',0.161,'threshold_evidence_sha256',repeat('d',64),
  'source_decision_digest',repeat('e',64),'reward_decision_digest',repeat('f',64),'intent_sha256',repeat('1',64),'git_refs',refs,
  'manifest',encode(convert_to('synthetic immutable source manifest','UTF8'),'base64'),'manifest_sha256',encode(sha256(convert_to('synthetic immutable source manifest','UTF8')),'hex'),
  'manifest_commit',repeat('c',40),'config_expected_commit',repeat('b',40),'publication_journal_sha256',repeat('2',64),
  'evaluator_image_digest','sha256:'||repeat('3',64),'host_qualification_sha256',repeat('4',64),'winner_review_policy','elected_winner_snow_union_v2',
  'g2_volume_policy','minimum-work/1','simulator_sha256',repeat('a',64),'scorer_sha256',repeat('a',64),
  'work_coverage_policy',jsonb_build_object('kind','seeded-crawl-coverage/v1','synthetic_opaque_ratio',0.95),
  'prepared_at',at_time-interval '1 hour','opens_at','0001-01-01T00:00:00Z');
 INSERT INTO competition_source_activation VALUES('synthetic','synthetic-competition','synthetic-season',1,1,
  intent||jsonb_build_object('generation',1,'expected_generation',0,'target_epoch',1,'target_round_id','00000000-0000-0000-0000-000000000001','kind','baseline'),at_time-interval '1 day',NULL);
 INSERT INTO competition_source_activation VALUES('synthetic','synthetic-competition','synthetic-season',2,2,intent,NULL,NULL);
 INSERT INTO competition_supervisor_transition VALUES('synthetic','synthetic-competition','synthetic-season',2,1,'preparation_pending',jsonb_build_object('source_activation_generation',2),at_time);
 INSERT INTO competition_source_pointer VALUES('synthetic','synthetic-competition','synthetic-season',1);
 INSERT INTO competition_round(round_id,competition_id,epoch_number,policy_json,providers_sha256,opens_at,closes_at,reveal_at,automation_sealed_at)
  VALUES('00000000-0000-0000-0000-000000000001','synthetic-competition',1,'{"automation_policy":{"enabled":false}}',repeat('5',64),at_time-interval '8 days',at_time-interval '1 day',at_time-interval '1 day',at_time);
 UPDATE competition_round SET policy_json=jsonb_build_object('automation_policy',jsonb_build_object('enabled',true,'environment','synthetic','season_id','synthetic-season','winner_review_policy','elected_winner_snow_union_v2')) WHERE epoch_number=1;
 INSERT INTO competition_evaluation_seal VALUES('00000000-0000-0000-0000-000000000001',jsonb_build_object('source_choice_at',at_time-interval '2 hours','publication_at',at_time-interval '2 hours'));
 INSERT INTO competition_reward_decision VALUES('00000000-0000-0000-0000-000000000001',1,jsonb_build_object('decision_sha256',repeat('f',64),'selected_job_id',NULL));
 INSERT INTO competition_supervisor_lease VALUES('synthetic','synthetic-competition','synthetic-season','synthetic-owner',7,at_time+interval '1 hour');
 PERFORM set_config('urnetwork.competition_v2_owner','synthetic-owner',true); PERFORM set_config('urnetwork.competition_v2_fence','7',true);
 PERFORM set_config('urnetwork.competition_intake_first_policy','{"kind":"published-source-intake/v1","from_epoch":2,"approval_sha256":"'||repeat('6',64)||'"}',true);
 PERFORM set_config('urnetwork.competition_source_handoff_actor','synthetic-human',true); PERFORM set_config('urnetwork.competition_source_handoff_capability','source_handoff',true);
 PERFORM competition_v2_source_authorized(intent,'synthetic','synthetic-competition','synthetic-season');
 request:=jsonb_build_object('target_epoch',2,'expected_revision',0,'reward_decision_sha256',repeat('f',64),'product_source_lock_sha256',lock_digest,
  'disposition','approve_no_winner_carry','idempotency_key','synthetic-handoff','evidence_sha256',repeat('7',64));
 handoff:=jsonb_build_object('schema',1,'kind','source-operator-handoff/v1','environment','synthetic','competition_id','synthetic-competition','season_id','synthetic-season',
  'revision',1,'actor_id','synthetic-human','request',request,'request_sha256',encode(sha256(convert_to(request::text,'UTF8')),'hex'),'created_at',at_time);
 api:=jsonb_build_object('environment','synthetic','competition_id','synthetic-competition','season_id','synthetic-season','generation',2,
  'target_round_id','00000000-0000-0000-0000-000000000002','role','api','actor_id','synthetic-api','ready',true,
  'manifest_commit',intent->>'manifest_commit','manifest_sha256',intent->>'manifest_sha256','source_lock_sha256',lock_digest,
  'qualification_sha256',intent->>'host_qualification_sha256','runtime_image_digest','sha256:'||repeat('8',64),'recorded_at',at_time);
 INSERT INTO competition_source_readiness VALUES('synthetic','synthetic-competition','synthetic-season',2,'api',api,at_time);
 policy:=jsonb_build_object('schema',2,'competition_id','synthetic-competition','base_sha',repeat('a',40),'score_schema',1,'scorer_version','sim-latency-score/3',
  'evaluator_image_digest',intent->>'evaluator_image_digest','evaluator_command','/synthetic/evaluator','evaluator_command_sha256',repeat('9',64),
  'patch_policy',jsonb_build_object('max_patch_bytes',4096,'allowed_paths',jsonb_build_array('synthetic/*.go'),'forbidden_paths',jsonb_build_array('synthetic/protected/**')),
  'automation_policy',jsonb_build_object('enabled',true,'version',2,'environment','synthetic','season_id','synthetic-season','required_api_capability','competition.reward.v2.runtime-blob.v1',
   'source_activation_generation',2,'source_generation',0,'source_lock_sha256',lock_digest,'winner_review_policy','elected_winner_snow_union_v2'),
  'evaluation_policy',jsonb_build_object('replicates',9,'takeover_margin',0.161,'host_qualification_sha256',repeat('4',64),'hardware_id','synthetic-hardware',
   'g2_volume_policy','minimum-work/1','simulator_sha256',repeat('a',64),'scorer_sha256',repeat('a',64),'work_coverage_policy',intent->'work_coverage_policy'),
  'season_policy',jsonb_build_object('submission_window_seconds',604800,'epoch_count',6,'preparation_window_seconds',0));
 policy:=policy||jsonb_build_object('distributed_evaluation_policy',jsonb_build_object('approval_sha256',repeat('e',64),
  'hosts',jsonb_build_array(jsonb_build_object('host_id','synthetic-host-local','qualification_sha256',repeat('c',64)),jsonb_build_object('host_id','synthetic-host-remote','qualification_sha256',repeat('d',64)))));
 grant_value:=jsonb_build_object('schema',1,'kind','published-source-intake/v1','policy',current_setting('urnetwork.competition_intake_first_policy')::jsonb,
  'environment','synthetic','competition_id','synthetic-competition','season_id','synthetic-season','generation',2,'expected_generation',1,'target_epoch',2,
  'target_round_id','00000000-0000-0000-0000-000000000002','intent_sha256',repeat('1',64),'round_policy_sha256',encode(sha256(convert_to(policy::text,'UTF8')),'hex'),
  'api_readiness_sha256',encode(sha256(convert_to(api::text,'UTF8')),'hex'),'api_runtime_image_digest',api->>'runtime_image_digest',
  'created_at',at_time,'season_ends_at',at_time+interval '30 days','retain_until',at_time+interval '30 days');
 INSERT INTO synthetic_intake_fixture VALUES(intent,policy,api,grant_value,request,handoff);
END $fixture$;
CREATE FUNCTION synthetic_insert_intake(value jsonb, frozen jsonb) RETURNS void LANGUAGE sql AS $$
 INSERT INTO competition_source_intake_activation SELECT 'synthetic','synthetic-competition','synthetic-season',2,'00000000-0000-0000-0000-000000000002',2,
  value,encode(sha256(convert_to(value::text,'UTF8')),'hex'),frozen,convert_to(value::text,'UTF8'),convert_to(frozen::text,'UTF8'),convert_to(api::text,'UTF8'),(value->>'created_at')::timestamp FROM synthetic_intake_fixture;
$$;
CREATE FUNCTION synthetic_insert_policy_variant(frozen jsonb) RETURNS void LANGUAGE sql AS $$
 SELECT synthetic_insert_intake(grant_value||jsonb_build_object('round_policy_sha256',encode(sha256(convert_to(frozen::text,'UTF8')),'hex')),frozen) FROM synthetic_intake_fixture;
$$;
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_intake(grant_value,policy) FROM synthetic_intake_fixture');
SELECT set_config('urnetwork.competition_source_handoff_capability','source_promoter',true);
SELECT synthetic_expect_intake_failure('INSERT INTO competition_source_operator_handoff SELECT ''synthetic'',''synthetic-competition'',''synthetic-season'',2,1,''synthetic-handoff'',handoff,convert_to(request::text,''UTF8''),(handoff->>''created_at'')::timestamp FROM synthetic_intake_fixture');
SELECT set_config('urnetwork.competition_source_handoff_capability','source_handoff',true);
INSERT INTO competition_source_operator_handoff SELECT 'synthetic','synthetic-competition','synthetic-season',2,1,'synthetic-handoff',handoff,convert_to(request::text,'UTF8'),(handoff->>'created_at')::timestamp FROM synthetic_intake_fixture;
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_policy_variant(jsonb_set(policy,''{patch_policy}'',''{}''::jsonb)) FROM synthetic_intake_fixture');
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_policy_variant(policy-''evaluator_command_sha256'') FROM synthetic_intake_fixture');
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_policy_variant(jsonb_set(policy,''{scorer_version}'',''"sim-latency-score/2"''::jsonb)) FROM synthetic_intake_fixture');
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_policy_variant(jsonb_set(policy,''{evaluation_policy,work_coverage_policy,synthetic_opaque_ratio}'',''0.90''::jsonb)) FROM synthetic_intake_fixture');
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_intake(grant_value||''{"intent_sha256":"'||repeat('0',64)||'"}''::jsonb,policy) FROM synthetic_intake_fixture');
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_intake(grant_value||''{"expected_generation":9}''::jsonb,policy) FROM synthetic_intake_fixture');
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_intake(grant_value,policy||''{"base_sha":"'||repeat('0',40)||'"}''::jsonb) FROM synthetic_intake_fixture');
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_intake(grant_value||''{"api_readiness_sha256":"'||repeat('0',64)||'"}''::jsonb,policy) FROM synthetic_intake_fixture');
SELECT synthetic_insert_intake(grant_value,policy) FROM synthetic_intake_fixture;
INSERT INTO competition_source_intake_pointer VALUES('synthetic','synthetic-competition','synthetic-season',2);
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM competition_source_readiness WHERE generation=2 AND role='worker') OR (SELECT generation FROM competition_source_pointer)<>1
  OR (SELECT activated_at FROM competition_source_activation WHERE generation=2) IS NOT NULL THEN RAISE EXCEPTION 'intake manufactured worker or runtime activation'; END IF;
END $$;
CREATE FUNCTION synthetic_insert_intake_round(frozen jsonb, opened timestamp) RETURNS void LANGUAGE sql AS $$
 INSERT INTO competition_round(round_id,competition_id,epoch_number,policy_json,providers_sha256,opens_at,closes_at,reveal_at)
 VALUES('00000000-0000-0000-0000-000000000002','synthetic-competition',2,frozen,repeat('5',64),opened,opened+interval '7 days',opened+interval '7 days');
$$;
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_intake_round(policy,timezone(''UTC'',transaction_timestamp())-interval ''1 second'') FROM synthetic_intake_fixture');
SELECT synthetic_expect_intake_failure('SELECT synthetic_insert_intake_round(policy||''{"synthetic_extra_numeric_rule":1}''::jsonb,timezone(''UTC'',clock_timestamp())) FROM synthetic_intake_fixture');
SELECT synthetic_insert_intake_round(policy,timezone('UTC',clock_timestamp())) FROM synthetic_intake_fixture;
INSERT INTO competition_job(job_id,round_id,state) VALUES('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000002','queued');
SELECT synthetic_expect_intake_failure('UPDATE competition_job SET started_at=timezone(''UTC'',clock_timestamp()) WHERE job_id=''00000000-0000-0000-0000-000000000003''');
SELECT synthetic_expect_intake_failure('UPDATE competition_job SET attempt_count=1 WHERE job_id=''00000000-0000-0000-0000-000000000003''');
SELECT synthetic_expect_intake_failure('UPDATE competition_job SET state=''running'' WHERE job_id=''00000000-0000-0000-0000-000000000003''');
UPDATE competition_source_activation SET activated_at=timezone('UTC',clock_timestamp()) WHERE generation=2;
UPDATE competition_source_pointer SET generation=2;
DO $$ BEGIN IF competition_v2_intake_runtime_ready('00000000-0000-0000-0000-000000000002') THEN RAISE EXCEPTION 'activation timestamp replaced missing worker evidence'; END IF; END $$;
INSERT INTO competition_source_readiness SELECT environment,competition_id,season_id,generation,'worker',receipt_json||'{"role":"worker","actor_id":"synthetic-worker"}'::jsonb,created_at FROM competition_source_readiness WHERE generation=2 AND role='api';
DO $$ BEGIN
 IF NOT competition_v2_intake_runtime_ready('00000000-0000-0000-0000-000000000002') THEN RAISE EXCEPTION 'real matching runtime quorum refused'; END IF;
 IF competition_v2_intake_grading_ready('00000000-0000-0000-0000-000000000002') THEN RAISE EXCEPTION 'missing fresh same-round baseline authorized grading'; END IF;
END $$;
INSERT INTO competition_worker_slot VALUES(1,NULL,'synthetic-baseline-owner',timezone('UTC',clock_timestamp())+interval '1 hour');
INSERT INTO competition_job_event VALUES('00000000-0000-0000-0000-000000000003','synthetic-baseline-owner','round_baseline_preparation_reserved',
 jsonb_build_object('context_sha256',repeat('a',64),'round_id','00000000-0000-0000-0000-000000000002','job_id','00000000-0000-0000-0000-000000000003'));
SELECT set_config('urnetwork.competition_executor_baseline_owner','synthetic-baseline-owner',true),set_config('urnetwork.competition_executor_baseline_context',repeat('a',64),true),set_config('urnetwork.competition_executor_baseline_manifest',repeat('b',64),true);
DO $baseline$
DECLARE content bytea;
BEGIN
 content:=convert_to(jsonb_build_object('kind','sim-latency-score-baseline','round_id','00000000-0000-0000-0000-000000000002','score_schema',1,'scorer_version','sim-latency-score/3','takeover_margin',0.161,'g2_volume_policy','minimum-work/1',
  'work_coverage_policy',(SELECT intent->'work_coverage_policy' FROM synthetic_intake_fixture),
  'replicates',(SELECT jsonb_agg(jsonb_build_object('raw_score',80,'request_count',100,'received_bytes',1000,'work_coverage','{"synthetic_measured_work":true}'::jsonb)) FROM generate_series(1,9)))::text,'UTF8');
 INSERT INTO competition_round_baseline VALUES('00000000-0000-0000-0000-000000000002',content,encode(sha256(content),'hex'),'00000000-0000-0000-0000-000000000003',1,repeat('b',64),
  repeat('a',40),repeat('5',64),'sha256:'||repeat('3',64),'sim-latency-score/3','synthetic-hardware',repeat('4',64),timezone('UTC',clock_timestamp()));
 IF competition_v2_intake_grading_ready('00000000-0000-0000-0000-000000000002') THEN RAISE EXCEPTION 'missing configured host no-op receipts permitted grading'; END IF;
 IF EXISTS(SELECT 1 FROM competition_job WHERE state<>'queued' OR attempt_count<>0 OR started_at IS NOT NULL) THEN RAISE EXCEPTION 'baseline publication started a candidate'; END IF;
END $baseline$;
DO $hosts$
DECLARE host jsonb; control text; at_time timestamp; count integer:=0;
BEGIN
 SELECT baseline_sha256 INTO control FROM competition_round_baseline;
 at_time:=timezone('UTC',clock_timestamp());
 FOR host IN SELECT value FROM jsonb_array_elements((SELECT policy->'distributed_evaluation_policy'->'hosts' FROM synthetic_intake_fixture)) LOOP
  count:=count+1;
  INSERT INTO competition_executor_qualification_run VALUES('00000000-0000-0000-0000-000000000002',host->>'host_id','00000000-0000-0000-0000-000000000004',at_time,at_time+interval '3 hours');
  INSERT INTO competition_executor_equivalence VALUES('00000000-0000-0000-0000-000000000002',host->>'host_id',
   jsonb_build_object('round_id','00000000-0000-0000-0000-000000000002','host_id',host->>'host_id','baseline_sha256',control,'source_lock_sha256',(SELECT intent->>'source_lock_sha256' FROM synthetic_intake_fixture),
    'evaluator_image_digest','sha256:'||repeat('3',64),'providers_sha256',repeat('5',64),'qualification_sha256',host->>'qualification_sha256','policy_approval_sha256',repeat('e',64),
    'passed',true,'noop',true,'replicates',9,'qualification_job_id','00000000-0000-0000-0000-000000000004','qualification_started_at',at_time,'qualification_deadline_at',at_time+interval '3 hours','archive_manifest_sha256',repeat('f',64)),control,at_time);
  IF count=1 AND competition_v2_intake_grading_ready('00000000-0000-0000-0000-000000000002') THEN RAISE EXCEPTION 'partial host admission permitted grading'; END IF;
 END LOOP;
 IF NOT competition_v2_intake_grading_ready('00000000-0000-0000-0000-000000000002') THEN RAISE EXCEPTION 'genuine full quorum, R9 and every no-op host refused'; END IF;
END $hosts$;
UPDATE competition_job SET state='running',attempt_count=1,started_at=timezone('UTC',clock_timestamp()) WHERE job_id='00000000-0000-0000-0000-000000000003';
SELECT synthetic_expect_intake_failure('UPDATE competition_source_intake_activation SET target_epoch=3');
SELECT synthetic_expect_intake_failure('DELETE FROM competition_source_operator_handoff');
SELECT synthetic_expect_intake_failure('TRUNCATE competition_source_intake_activation CASCADE');
INSERT INTO competition_round(round_id,competition_id,epoch_number,policy_json,opens_at,closes_at,reveal_at) VALUES('00000000-0000-0000-0000-000000000099','synthetic-legacy',1,'{}',now(),now()+interval '7 days',now()+interval '7 days');
DO $$ BEGIN IF NOT competition_v2_intake_grading_ready('00000000-0000-0000-0000-000000000099') THEN RAISE EXCEPTION 'legacy no-grant path was changed'; END IF; END $$;
INSERT INTO competition_reward_decision VALUES('00000000-0000-0000-0000-000000000001',2,jsonb_build_object('decision_sha256',repeat('0',64),'selected_job_id','00000000-0000-0000-0000-000000000009'));
SELECT synthetic_expect_intake_failure('SELECT competition_v2_intake_runtime_ready(''00000000-0000-0000-0000-000000000002'')');
COMMIT;
`
