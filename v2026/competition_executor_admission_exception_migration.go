// A separate operator decision admits one retained failed host screen without
// relabeling its receipt or weakening any candidate-scoring gate.
package server

import "strings"

// Reuse the published whole-host predicate; both original and replacement
// receipts must bind the selected immutable clock, never the original alone.
func CompetitionExecutorAdmissionExceptionMigrationSql() string {
	parts := strings.Split(CompetitionExecutorControlAmendmentMigrationSql(), "$executor_control_hosts_ready$")
	if len(parts) != 3 {
		panic("published whole-host readiness changed")
	}
	body := parts[1]
	for _, change := range []struct{ before, after string }{
		{before: "FROM competition_executor_equivalence e JOIN competition_executor_qualification_run q ON q.round_id=e.round_id AND q.host_id=e.host_id", after: "FROM competition_executor_admitted_evidence e CROSS JOIN LATERAL competition_executor_qualification_clock(e.round_id,e.host_id) q"},
		{before: "e.receipt_json->'passed'='true'::jsonb", after: "(e.receipt_json->'passed'='true'::jsonb OR EXISTS(SELECT 1 FROM competition_executor_admission_exception x WHERE x.round_id=e.round_id AND x.host_id=e.host_id AND x.receipt_sha256=e.receipt_sha256))"},
	} {
		if strings.Count(body, change.before) != 1 {
			panic("published host admission predicate changed")
		}
		body = strings.Replace(body, change.before, change.after, 1)
	}
	return competitionExecutorAdmissionExceptionSql + "\nCREATE OR REPLACE FUNCTION competition_executor_control_hosts_ready(target uuid) RETURNS boolean LANGUAGE plpgsql AS $admission_exception_hosts$" + body + "$admission_exception_hosts$;\n"
}

const competitionExecutorAdmissionExceptionSql = `
SET LOCAL lock_timeout='5s';
CREATE TABLE competition_executor_admission_exception (
 round_id uuid NOT NULL REFERENCES competition_round(round_id),host_id varchar(128) NOT NULL,
 approval_sha256 char(64) NOT NULL CHECK(approval_sha256 ~ '^[0-9a-f]{64}$'),
 approval_json jsonb NOT NULL,approval_canonical bytea NOT NULL CHECK(octet_length(approval_canonical) BETWEEN 1 AND 32768),
 receipt_json jsonb NOT NULL,receipt_canonical bytea NOT NULL CHECK(octet_length(receipt_canonical) BETWEEN 1 AND 1048576),
 receipt_sha256 char(64) NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
 baseline_sha256 char(64) NOT NULL,created_at timestamp NOT NULL,
 approved_at timestamp NOT NULL DEFAULT (statement_timestamp() AT TIME ZONE 'UTC'),
 PRIMARY KEY(round_id,host_id),
 CHECK(convert_from(approval_canonical,'UTF8')::jsonb=approval_json),
 CHECK(encode(sha256(approval_canonical),'hex')=approval_sha256),
 CHECK(convert_from(receipt_canonical,'UTF8')::jsonb=receipt_json),
 CHECK(encode(sha256(receipt_canonical),'hex')=receipt_sha256)
);
CREATE TRIGGER competition_executor_admission_exception_append_only BEFORE UPDATE OR DELETE ON competition_executor_admission_exception FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_executor_admission_exception_no_truncate BEFORE TRUNCATE ON competition_executor_admission_exception FOR EACH STATEMENT EXECUTE FUNCTION competition_append_only_guard();
CREATE FUNCTION competition_executor_admission_exception_guard() RETURNS trigger LANGUAGE plpgsql AS $admission_exception$
DECLARE r competition_round%ROWTYPE; host jsonb; value jsonb; receipt jsonb; baseline text; q record;
BEGIN
 SELECT * INTO r FROM competition_round WHERE round_id=NEW.round_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'admission exception requires an existing round'; END IF;
 PERFORM competition_v2_scope_lock(r.policy_json->'automation_policy'->>'environment',r.competition_id,r.policy_json->'automation_policy'->>'season_id');
 SELECT * INTO r FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT * INTO q FROM competition_executor_qualification_clock(NEW.round_id,NEW.host_id);
 host:=competition_executor_control_host(NEW.round_id,NEW.host_id);
 SELECT baseline_sha256 INTO baseline FROM competition_round_baseline WHERE round_id=NEW.round_id;
 value:=NEW.approval_json;receipt:=NEW.receipt_json;
 IF host IS NULL OR baseline IS NULL OR q.job_id IS NULL OR r.epoch_number<2 OR r.staging OR r.canceled OR r.finalized_at IS NOT NULL OR r.automation_sealed_at IS NOT NULL
  OR EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND (attempt_count<>0 OR started_at IS NOT NULL OR execution_deadline_at IS NOT NULL))
  OR EXISTS(SELECT 1 FROM competition_execution_slot WHERE round_id=NEW.round_id AND job_id IS NOT NULL)
  OR EXISTS(SELECT 1 FROM competition_executor_equivalence WHERE round_id=NEW.round_id AND host_id=NEW.host_id)
  OR current_setting('urnetwork.competition_executor_admission_exception',true) IS DISTINCT FROM NEW.approval_sha256::text
  OR NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=r.policy_json->'automation_policy'->>'environment' AND competition_id=r.competition_id AND season_id=r.policy_json->'automation_policy'->>'season_id'
   AND owner_id=current_setting('urnetwork.competition_v2_owner',true) AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at)
 THEN RAISE EXCEPTION 'admission exception requires the idle live operator fence'; END IF;
 IF jsonb_typeof(value) IS DISTINCT FROM 'object' OR (SELECT count(*) FROM jsonb_object_keys(value))<>9
  OR value->>'schema' IS DISTINCT FROM '1' OR value->>'kind' IS DISTINCT FROM 'executor-retained-admission-exception/v1'
  OR value->>'round_id' IS DISTINCT FROM NEW.round_id::text OR value->>'host_id' IS DISTINCT FROM NEW.host_id
  OR value->>'job_id' IS DISTINCT FROM q.job_id::text OR value->>'baseline_sha256' IS DISTINCT FROM baseline
  OR value->>'receipt_document_sha256' IS DISTINCT FROM receipt->'retention'->>'sha256'
  OR value->>'excepted_metric' IS DISTINCT FROM 'throughput_p50_bytes_per_s' OR COALESCE(value->>'evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR NEW.baseline_sha256 IS DISTINCT FROM baseline OR receipt->>'round_id' IS DISTINCT FROM NEW.round_id::text OR receipt->>'host_id' IS DISTINCT FROM NEW.host_id
  OR receipt->>'schema' IS DISTINCT FROM '1' OR receipt->>'kind' IS DISTINCT FROM 'competition-executor-noop-admission/v1'
  OR receipt->>'baseline_sha256' IS DISTINCT FROM baseline OR receipt->>'source_lock_sha256' IS DISTINCT FROM r.policy_json->'automation_policy'->>'source_lock_sha256'
  OR receipt->>'evaluator_image_digest' IS DISTINCT FROM r.policy_json->>'evaluator_image_digest' OR receipt->>'providers_sha256' IS DISTINCT FROM r.providers_sha256
  OR receipt->>'qualification_sha256' IS DISTINCT FROM host->>'qualification_sha256' OR receipt->>'policy_approval_sha256' IS DISTINCT FROM r.policy_json->'distributed_evaluation_policy'->>'approval_sha256'
  OR receipt->>'config_local_sha256' IS DISTINCT FROM r.policy_json->'evaluation_policy'->>'config_local_sha256' OR receipt->>'vault_local_sha256' IS DISTINCT FROM r.policy_json->'evaluation_policy'->>'vault_local_sha256'
  OR COALESCE(receipt->>'control_amendment_sha256','') IS DISTINCT FROM COALESCE((SELECT amendment_sha256::text FROM competition_executor_control_amendment WHERE round_id=NEW.round_id),'')
  OR COALESCE(receipt->>'recovery_sha256','') IS DISTINCT FROM COALESCE((SELECT authorization_sha256::text FROM competition_executor_qualification_recovery WHERE round_id=NEW.round_id AND host_id=NEW.host_id),'')
  OR receipt->>'qualification_job_id' IS DISTINCT FROM q.job_id::text OR (receipt->>'qualification_started_at')::timestamp IS DISTINCT FROM q.started_at
  OR (receipt->>'qualification_deadline_at')::timestamp IS DISTINCT FROM q.deadline_at OR (receipt->>'created_at')::timestamp IS DISTINCT FROM NEW.created_at
  OR NEW.created_at<q.started_at OR NEW.created_at>q.deadline_at
  OR receipt->'passed' IS DISTINCT FROM 'false'::jsonb OR receipt->'noop' IS DISTINCT FROM 'true'::jsonb OR receipt->'replicates' IS DISTINCT FROM '9'::jsonb
  OR receipt->'statistical_equivalence_proven' IS DISTINCT FROM 'false'::jsonb
  OR receipt->>'screen_kind' IS DISTINCT FROM 'operational-admission/v1' OR (receipt->>'maximum_raw_score_relative_difference')::numeric IS DISTINCT FROM 0.05
  OR COALESCE((receipt->>'raw_score_relative_difference')::numeric,1) NOT BETWEEN 0 AND 0.05
  OR receipt->>'statistical_method' IS DISTINCT FROM 'two-sided-welch-t-run-level'
  OR (receipt->>'confidence_level')::numeric IS DISTINCT FROM 0.95
  OR receipt->'work_coverage_policy' IS DISTINCT FROM r.policy_json->'evaluation_policy'->'work_coverage_policy'
  OR COALESCE(receipt->>'archive_manifest_sha256','') !~ '^[0-9a-f]{64}$'
  OR receipt->'retention'->>'mode' IS DISTINCT FROM 'COMPLIANCE' OR COALESCE(receipt->'retention'->>'version_id','')=''
  OR COALESCE(receipt->'retention'->>'sha256','') !~ '^[0-9a-f]{64}$' OR COALESCE(receipt->'retention'->>'key','')=''
  OR COALESCE((receipt->'retention'->>'bytes')::bigint,0)<=0
  OR COALESCE((receipt->'retention'->>'retain_until')::timestamp,'-infinity'::timestamp)<COALESCE((SELECT max(artifact_retain_until) FROM competition_job WHERE round_id=NEW.round_id),NEW.created_at)
  OR jsonb_typeof(receipt->'metrics') IS DISTINCT FROM 'array' OR jsonb_array_length(receipt->'metrics')<>9
  OR (SELECT count(*) FROM jsonb_array_elements(receipt->'metrics') m WHERE m->'significant_shift'='true'::jsonb AND m->>'metric'='throughput_p50_bytes_per_s')<>1
  OR EXISTS(SELECT 1 FROM jsonb_array_elements(receipt->'metrics') m WHERE m->'significant_shift'='true'::jsonb AND m->>'metric'<>'throughput_p50_bytes_per_s')
 THEN RAISE EXCEPTION 'admission exception changed its exact failed retained sample or scope'; END IF;
 RETURN NEW;
END $admission_exception$;
CREATE TRIGGER competition_executor_admission_exception_identity BEFORE INSERT ON competition_executor_admission_exception FOR EACH ROW EXECUTE FUNCTION competition_executor_admission_exception_guard();
CREATE VIEW competition_executor_admitted_evidence AS
 SELECT round_id,host_id,receipt_json,receipt_sha256,baseline_sha256,created_at FROM competition_executor_equivalence
 UNION ALL SELECT round_id,host_id,receipt_json,receipt_sha256,baseline_sha256,created_at FROM competition_executor_admission_exception;
`
