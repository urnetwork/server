// Additive before deployment: older recovery classes and every published guard
// stay unchanged. A new independent trigger owns only the operator-stop proof.
package server

import "strings"

const competitionOperatorInterruptionMigrationSql = `
CREATE FUNCTION competition_job_operator_interruption_valid(target_job uuid,target_round uuid,terminal jsonb,request jsonb)
RETURNS boolean LANGUAGE plpgsql STABLE AS $operator_interruption$
DECLARE proof jsonb; claimed competition_job_event%ROWTYPE; resumed competition_job_event%ROWTYPE;
 ended competition_job_event%ROWTYPE; following bigint; started timestamp; completed timestamp; stopped timestamp;
 budget integer; interrupted_attempt integer; terminal_attempt integer;
BEGIN
 proof:=request->'operator_interruption_evidence';
 IF jsonb_typeof(proof) IS DISTINCT FROM 'object' OR
  (SELECT count(*) FROM jsonb_object_keys(proof))<>6 OR
  proof-ARRAY['schema','action','stopped_at','claim_event_id','claim_event_sha256','evidence_sha256']<>'{}'::jsonb OR
  proof->'schema' IS DISTINCT FROM '1'::jsonb OR proof->>'action' IS DISTINCT FROM 'stop_worker' OR
  jsonb_typeof(proof->'stopped_at') IS DISTINCT FROM 'string' OR
  COALESCE(proof->>'stopped_at' !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]{1,6})?(Z|[+-][0-9]{2}:[0-9]{2})$',true) OR
  jsonb_typeof(proof->'claim_event_id') IS DISTINCT FROM 'number' OR
  COALESCE(proof->>'claim_event_id' !~ '^[1-9][0-9]{0,18}$',true) OR
  COALESCE(proof->>'claim_event_sha256' !~ '^[0-9a-f]{64}$',true) OR
  COALESCE(proof->>'evidence_sha256' !~ '^[0-9a-f]{64}$',true) OR
  request->>'incident_class' IS DISTINCT FROM 'operator_authorized_interrupted_budget_v1' OR
  jsonb_typeof(request->'authorized_at') IS DISTINCT FROM 'string' OR
  COALESCE(request->>'authorized_at' !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$',true) OR
  jsonb_typeof(request->'incident_event_id') IS DISTINCT FROM 'number' OR
  COALESCE(request->>'incident_event_id' !~ '^[1-9][0-9]{0,18}$',true) OR
  request->>'job_id' IS DISTINCT FROM target_job::text OR request->>'round_id' IS DISTINCT FROM target_round::text OR
  request->'expected_cycle' IS DISTINCT FROM '0'::jsonb OR request->'single_use' IS DISTINCT FROM 'true'::jsonb OR
  request->'expected_terminal_snapshot' IS DISTINCT FROM terminal OR
  terminal->>'job_id' IS DISTINCT FROM target_job::text OR terminal->>'round_id' IS DISTINCT FROM target_round::text OR
  terminal->>'state' IS DISTINCT FROM 'failed' OR terminal->'evaluation_cycle' IS DISTINCT FROM '0'::jsonb OR
  terminal->'score_json' IS DISTINCT FROM 'null'::jsonb OR
  terminal->'lease_owner' IS DISTINCT FROM 'null'::jsonb OR terminal->'lease_expires_at' IS DISTINCT FROM 'null'::jsonb OR
  terminal->'eval_error_json'->>'kind' IS DISTINCT FROM 'infrastructure' OR
  terminal->'eval_error_json'->>'code' IS DISTINCT FROM 'evaluation_time_budget_exhausted' OR
  terminal->'eval_error_json'->'retriable' IS DISTINCT FROM 'false'::jsonb OR
  COALESCE(request->>'failure_manifest_sha256','')<>'' OR COALESCE(request->>'rejected_artifact_path','')<>'' OR
  request ? 'backing_service_evidence' THEN RETURN false; END IF;
 started:=(terminal->>'started_at')::timestamp;
 completed:=(terminal->>'completed_at')::timestamp;
 stopped:=(proof->>'stopped_at')::timestamptz AT TIME ZONE 'UTC';
 interrupted_attempt:=(request->>'incident_attempt')::integer;
 terminal_attempt:=(request->>'expected_attempt_count')::integer;
 SELECT (policy_json->'evaluation_policy'->>'score_timeout_seconds')::integer INTO budget
  FROM competition_round WHERE round_id=target_round;
 IF started IS NULL OR completed IS NULL OR stopped IS NULL OR NOT isfinite(started) OR NOT isfinite(completed) OR NOT isfinite(stopped) OR budget IS DISTINCT FROM 10800 OR
  interrupted_attempt IS NULL OR interrupted_attempt<1 OR terminal_attempt IS NULL OR
  terminal_attempt<=interrupted_attempt OR terminal_attempt-interrupted_attempt<>1 OR
  terminal->'attempt_count' IS DISTINCT FROM request->'expected_attempt_count' OR
  NOT COALESCE(stopped<started+make_interval(secs=>budget) AND started+make_interval(secs=>budget)<=completed
   AND stopped<((request->>'authorized_at')::timestamptz AT TIME ZONE 'UTC'),false) THEN RETURN false; END IF;
 SELECT * INTO claimed FROM competition_job_event
  WHERE job_id=target_job AND event_id=(proof->>'claim_event_id')::bigint AND event_type='claimed';
 IF NOT FOUND OR claimed.payload_sha256 IS DISTINCT FROM proof->>'claim_event_sha256' OR
  claimed.payload_json->'attempt' IS DISTINCT FROM request->'incident_attempt' OR
  claimed.event_at<started OR stopped<claimed.event_at THEN RETURN false; END IF;
 SELECT * INTO resumed FROM competition_job_event
  WHERE job_id=target_job AND event_id>claimed.event_id ORDER BY event_id LIMIT 1;
 IF NOT FOUND OR resumed.event_type<>'claimed' OR resumed.payload_json->'attempt' IS DISTINCT FROM request->'expected_attempt_count' OR
  resumed.event_at<started+make_interval(secs=>budget) OR resumed.event_at>completed THEN RETURN false; END IF;
 SELECT * INTO ended FROM competition_job_event
  WHERE job_id=target_job AND event_id=(request->>'incident_event_id')::bigint;
 IF NOT FOUND OR ended.event_type<>'failed' OR ended.event_at IS DISTINCT FROM completed OR
  ended.payload_sha256 IS DISTINCT FROM request->>'incident_event_sha256' OR
  ended.payload_json->'attempt' IS DISTINCT FROM request->'expected_attempt_count' OR
  ended.payload_json ? 'artifact_archive_failure' OR
  (ended.payload_json ? 'error_code' AND ended.payload_json->>'error_code' IS DISTINCT FROM 'evaluation_time_budget_exhausted')
  THEN RETURN false; END IF;
 SELECT event_id INTO following FROM competition_job_event
  WHERE job_id=target_job AND event_id>resumed.event_id ORDER BY event_id LIMIT 1;
 RETURN COALESCE(claimed.event_id<resumed.event_id AND resumed.event_id<ended.event_id AND following=ended.event_id,false);
EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range OR invalid_datetime_format OR datetime_field_overflow OR invalid_parameter_value THEN
 RETURN false;
END $operator_interruption$;

CREATE FUNCTION competition_job_operator_interruption_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF NEW.authorization_json->>'incident_class' IS DISTINCT FROM 'operator_authorized_interrupted_budget_v1' THEN
  IF NEW.authorization_json ? 'operator_interruption_evidence' THEN
   RAISE EXCEPTION 'operator-stop evidence cannot authorize another incident class';
  END IF;
  RETURN NEW;
 END IF;
 IF NEW.receipt_json->>'incident_class' IS DISTINCT FROM NEW.authorization_json->>'incident_class' OR
  NEW.receipt_json->'previous_failure'->>'kind' IS DISTINCT FROM NEW.original_terminal_json->'eval_error_json'->>'kind' OR
  NEW.receipt_json->'previous_failure'->>'code' IS DISTINCT FROM NEW.original_terminal_json->'eval_error_json'->>'code' OR
  NEW.receipt_json->'previous_failure'->'retriable' IS DISTINCT FROM 'false'::jsonb OR
  NEW.receipt_json->'execution_timeout_seconds' IS DISTINCT FROM '10800'::jsonb OR
  NOT competition_job_operator_interruption_valid(NEW.job_id,NEW.round_id,NEW.original_terminal_json,NEW.authorization_json) THEN
  RAISE EXCEPTION 'operator interruption requires exact stopped-claim and expired-budget evidence';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_job_operator_interruption BEFORE INSERT ON competition_job_reevaluation
 FOR EACH ROW EXECUTE FUNCTION competition_job_operator_interruption_guard();
`

var competitionOperatorInterruptionMigration = newSqlMigration(competitionOperatorInterruptionMigrationSql)

// The append-only list owns the version, including after unrelated merges.
func CompetitionOperatorInterruptionMigrationVersion() int {
	return migrationVersion(competitionOperatorInterruptionMigration)
}

// Independent catalog custody includes both the exact predicate and trigger.
func CompetitionOperatorInterruptionBodiesSql() (valid, guard string) {
	return strings.Split(competitionOperatorInterruptionMigrationSql, "$operator_interruption$")[1],
		strings.Split(competitionOperatorInterruptionMigrationSql, "$guard$")[1]
}
