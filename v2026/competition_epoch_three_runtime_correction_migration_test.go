// The correction migration replaces identity checks, not grading predicates.
package server

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Preserve all existing consumer checks except the exact identity predicates.
func TestEpochThreeRuntimeCorrectionSqlPreservesReadinessRequirements(t *testing.T) {
	sql := competitionEpochThreeRuntimeCorrectionSql()
	for _, required := range []string{
		"competition_epoch_three_runtime_receipt_valid(intent,NEW.role,NEW.receipt_json)",
		"competition_epoch_three_runtime_receipt_valid(intent,r.role,r.receipt_json)",
		"role_name='worker' AND c IS NOT NULL",
		"receipt->>'runtime_image_digest'=c->>'worker_image_digest'",
		"r.receipt_json=convert_from(intake_grant.api_readiness_canonical,'UTF8')::jsonb",
		"competition_v2_intake_require_handoff(intent)",
		"jsonb_array_length(control->'replicates')=9",
		"competition_executor_qualification_run WHERE round_id=NEW.round_id",
		"encode(sha256(correction_canonical),'hex')=correction_sha256",
		"clock_timestamp()<l.lease_expires_at",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("missing protected predicate %q", required)
		}
	}
	if strings.Contains(sql, "CREATE OR REPLACE FUNCTION competition_v2_intake_grading_ready") {
		t.Fatal("grading quorum must not change for an identity correction")
	}
}

// Exercises the journal on isolated PostgreSQL, including rejected writes
// that would otherwise replace an already running submission clock.
func TestEpochThreeRuntimeCorrectionJournalRejectsStartedRound(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		Db(tb.Context(), func(conn PgConn) {
			RaisePgResult(conn.Exec(tb.Context(), epochThreeRuntimeCorrectionFixture+
				competitionEpochThreeRuntimeCorrectionSql()+epochThreeRuntimeCorrectionReadinessTrigger+
				epochThreeRuntimeCorrectionAssertions, pgx.QueryExecModeSimpleProtocol))
		}, OptReadWrite(), OptNoRetry())
	})
}

const epochThreeRuntimeCorrectionFixture = `
CREATE TABLE competition_round(round_id uuid PRIMARY KEY, epoch_number int, staging bool, canceled bool,
 finalized_at timestamp, automation_sealed_at timestamp, closes_at timestamp, policy_json jsonb, providers_sha256 text);
CREATE TABLE competition_source_intake_activation(environment text, competition_id text, season_id text,
 generation bigint, target_round_id uuid, target_epoch int, round_policy_json jsonb, grant_json jsonb, api_readiness_canonical bytea);
CREATE TABLE competition_source_activation(environment text, competition_id text, season_id text,
 generation bigint, target_epoch int, abandoned_at timestamp, activated_at timestamp, intent_json jsonb, intent_sha256 text);
CREATE TABLE competition_supervisor_lease(environment text, competition_id text, season_id text, owner_id text,
 fencing_token bigint, lease_expires_at timestamp);
CREATE TABLE competition_source_readiness(environment text, competition_id text, season_id text, generation bigint, role text, receipt_json jsonb, created_at timestamp DEFAULT transaction_timestamp());
CREATE TABLE competition_source_pointer(environment text, competition_id text, season_id text, generation bigint);
CREATE TABLE competition_automation_hold(environment text, competition_id text, season_id text, kind text, active bool, revision integer);
CREATE TABLE competition_job(round_id uuid, attempt_count int, started_at timestamp, execution_deadline_at timestamp);
CREATE TABLE competition_round_baseline(round_id uuid);
CREATE TABLE competition_worker_slot(slot_id int, worker_id text, lease_expires_at timestamp);
CREATE TABLE competition_executor_qualification_run(round_id uuid);
CREATE TABLE competition_executor_equivalence(round_id uuid);
CREATE TABLE competition_execution_slot(round_id uuid, job_id uuid);
CREATE FUNCTION competition_append_only_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'append only'; END $$;
CREATE FUNCTION competition_v2_scope_lock(env text, competition text, season text) RETURNS void LANGUAGE sql AS
 $$ SELECT pg_advisory_xact_lock(hashtextextended(env||':'||competition||':'||season,0)) $$;
CREATE FUNCTION competition_v2_intake_require_handoff(intent jsonb) RETURNS void LANGUAGE plpgsql AS
 $$ BEGIN IF intent->>'target_round_id' IS DISTINCT FROM '01a1f4d0-47c9-ac2d-ce45-55ef159a9b41' THEN RAISE EXCEPTION 'wrong handoff'; END IF; END $$;
`

const epochThreeRuntimeCorrectionReadinessTrigger = `
CREATE TRIGGER test_runtime_receipt BEFORE INSERT ON competition_source_readiness
 FOR EACH ROW EXECUTE FUNCTION competition_v2_source_readiness_guard();
`

const epochThreeRuntimeCorrectionAssertions = `
DO $test$
DECLARE
 target uuid:='01a1f4d0-47c9-ac2d-ce45-55ef159a9b41';
 intent jsonb:=jsonb_build_object('target_round_id',target,'generation',3,'intent_sha256',repeat('a',64),
   'source_lock_sha256',repeat('b',64),'evaluator_image_digest','sha256:'||repeat('c',64),'host_qualification_sha256',repeat('d',64),
   'manifest_commit',repeat('1',40),'manifest_sha256',repeat('2',64));
 policy jsonb:=jsonb_build_object('base_sha',repeat('1',40),'evaluator_image_digest','sha256:'||repeat('c',64),
   'automation_policy',jsonb_build_object('source_lock_sha256',repeat('b',64)),
   'scorer_version','test','score_schema',1,'evaluation_policy',jsonb_build_object('host_qualification_sha256',repeat('d',64),
     'hardware_id','sille','replicates',9,'takeover_margin',0.2));
 value jsonb:=jsonb_build_object('schema',1,'kind','epoch-three-runtime-identity-correction/v1','round_id',target,
   'source_activation_generation',3,'source_intent_sha256',repeat('a',64),'source_lock_sha256',repeat('b',64),
   'evaluator_image_digest','sha256:'||repeat('c',64),'previous_qualification_sha256',repeat('d',64),
   'worker_qualification_sha256',repeat('e',64),'worker_image_digest','sha256:'||repeat('f',64),
   'host_containment_receipt_sha256',repeat('1',64));
 canonical bytea:=convert_to(value::text,'UTF8');
 digest text:=encode(sha256(canonical),'hex');
 envelope jsonb:=jsonb_build_object('environment','main','competition_id','sim','season_id','season','generation',3,'target_round_id',target,'recorded_at',transaction_timestamp(),
   'manifest_commit',repeat('1',40),'manifest_sha256',repeat('2',64),'source_lock_sha256',repeat('b',64),'actor_id','test','ready',true);
 old_receipt jsonb:=envelope||jsonb_build_object('qualification_sha256',repeat('d',64),'runtime_image_digest','sha256:'||repeat('1',64));
 new_receipt jsonb:=envelope||jsonb_build_object('qualification_sha256',repeat('e',64),'runtime_image_digest','sha256:'||repeat('f',64),'role','worker');
 control bytea:=convert_to(jsonb_build_object('round_id',target,'kind','sim-latency-score-baseline','score_schema',1,'scorer_version','test',
   'takeover_margin',0.2,'replicates',(SELECT jsonb_agg('{"raw_score":1,"request_count":1,"received_bytes":1,"work_coverage":{}}'::jsonb) FROM generate_series(1,9)))::text,'UTF8');
BEGIN
 INSERT INTO competition_round VALUES(target,3,false,false,NULL,NULL,clock_timestamp()+interval '1 hour',policy,NULL);
 INSERT INTO competition_source_intake_activation VALUES('main','sim','season',3,target,3,policy,jsonb_build_object('intent_sha256',repeat('a',64)),
   convert_to((old_receipt||'{"role":"api"}'::jsonb)::text,'UTF8'));
 INSERT INTO competition_source_activation VALUES('main','sim','season',3,3,NULL,NULL,intent,repeat('a',64));
 INSERT INTO competition_supervisor_lease VALUES('main','sim','season','owner',7,clock_timestamp()+interval '1 hour');
 PERFORM set_config('urnetwork.competition_v2_owner','owner',true);
 PERFORM set_config('urnetwork.competition_v2_fence','7',true);
 BEGIN
  INSERT INTO competition_epoch_three_runtime_correction VALUES(target,digest,canonical,value,transaction_timestamp());
  RAISE EXCEPTION 'missing approval accepted' USING ERRCODE='22000';
 EXCEPTION WHEN raise_exception THEN NULL;
 END;
 PERFORM set_config('urnetwork.competition_epoch_three_runtime_correction',digest,true);
 BEGIN
  INSERT INTO competition_job VALUES(target,1,clock_timestamp(),clock_timestamp()+interval '3 hours');
  INSERT INTO competition_epoch_three_runtime_correction VALUES(target,digest,canonical,value,transaction_timestamp());
  RAISE EXCEPTION 'started job was ignored' USING ERRCODE='22000';
 EXCEPTION WHEN raise_exception THEN NULL;
 END;
 INSERT INTO competition_epoch_three_runtime_correction VALUES(target,digest,canonical,value,transaction_timestamp());
 IF NOT competition_epoch_three_runtime_receipt_valid(intent,'api',old_receipt)
   OR competition_epoch_three_runtime_receipt_valid(intent,'api',new_receipt)
   OR competition_epoch_three_runtime_receipt_valid(intent,'worker',old_receipt)
   OR NOT competition_epoch_three_runtime_receipt_valid(intent,'worker',new_receipt)
   OR competition_epoch_three_runtime_receipt_valid(intent,'worker',new_receipt-'runtime_image_digest')
   OR NOT competition_epoch_three_runtime_receipt_valid(intent||'{"generation":2}'::jsonb,'worker',old_receipt) THEN
  RAISE EXCEPTION 'correction changed API or historical identity, or failed to bind the exact worker';
 END IF;
 INSERT INTO competition_source_readiness VALUES('main','sim','season',3,'api',old_receipt||'{"role":"api"}'::jsonb);
 INSERT INTO competition_source_readiness VALUES('main','sim','season',3,'worker',new_receipt);
 BEGIN
  INSERT INTO competition_source_readiness VALUES('main','sim','season',3,'worker',old_receipt||'{"role":"worker"}'::jsonb);
  RAISE EXCEPTION 'obsolete worker passed the readiness trigger' USING ERRCODE='22000';
 EXCEPTION WHEN raise_exception THEN NULL;
 END;
 IF NOT competition_v2_intake_baseline_valid(target,control,encode(sha256(control),'hex'),repeat('1',40),NULL,
   'sha256:'||repeat('c',64),'test','sille',repeat('e',64))
 OR competition_v2_intake_baseline_valid(target,control,encode(sha256(control),'hex'),repeat('1',40),NULL,
   'sha256:'||repeat('c',64),'test','sille',repeat('d',64)) THEN
  RAISE EXCEPTION 'baseline guard did not bind the actual producer fingerprint';
 END IF;
 IF competition_v2_intake_runtime_ready(target) THEN
  RAISE EXCEPTION 'correction activated the source';
 END IF;
 UPDATE competition_source_activation SET activated_at=transaction_timestamp();
 INSERT INTO competition_source_pointer VALUES('main','sim','season',3);
 IF NOT competition_v2_intake_runtime_ready(target) THEN
  RAISE EXCEPTION 'exact API and corrected worker receipts did not satisfy runtime readiness';
 END IF;
 INSERT INTO competition_automation_hold VALUES('main','sim','season','evaluation',true,1);
 IF competition_v2_intake_runtime_ready(target) THEN
  RAISE EXCEPTION 'correction ignored an evaluation hold';
 END IF;
END $test$;
`
