// A separately approved distributed recovery is additive before deployment.
// No DDL grants a retry, changes a hold, or rewrites published migration bytes.
package server

import "strings"

const competitionDistributedReevaluationAuthoritySql = `
SET LOCAL lock_timeout='2s';

CREATE FUNCTION competition_distributed_recovery_transition_allows(previous_job competition_job,next_job competition_job)
RETURNS boolean LANGUAGE plpgsql AS $distributed_recovery_transition$
DECLARE audit competition_job_reevaluation%ROWTYPE;
BEGIN
 IF previous_job.evaluation_cycle<>0 OR next_job.evaluation_cycle<>1 OR previous_job.state<>'failed' OR next_job.state<>'queued' THEN RETURN false; END IF;
 SELECT * INTO audit FROM competition_job_reevaluation WHERE job_id=previous_job.job_id;
 IF NOT FOUND OR audit.authorization_json->>'schema' IS DISTINCT FROM '2'
  OR audit.authorization_json->>'kind' IS DISTINCT FROM 'competition-distributed-job-reevaluation-authorization/v1'
  OR audit.authorization_json->>'incident_class' IS DISTINCT FROM 'operator_authorized_distributed_infrastructure_v1'
  OR audit.transaction_id IS DISTINCT FROM pg_current_xact_id()
  OR audit.operator_id IS DISTINCT FROM current_setting('urnetwork.competition_reevaluation_operator',true)
  OR audit.request_sha256 IS DISTINCT FROM current_setting('urnetwork.competition_reevaluation_request',true)
  OR audit.original_terminal_json IS DISTINCT FROM to_jsonb(previous_job)
  OR audit.receipt_json->'distributed'->>'native_proof_sha256' IS DISTINCT FROM current_setting('urnetwork.competition_distributed_recovery_proof',true)
  OR previous_job.executor_host_id IS NULL OR previous_job.execution_deadline_at IS NULL
  OR previous_job.cycle_started_at IS NOT NULL OR previous_job.cycle_attempt_offset<>0
  OR next_job.attempt_count<>previous_job.attempt_count OR next_job.cycle_attempt_offset<>previous_job.attempt_count
  OR next_job.cycle_started_at IS NOT NULL OR next_job.completed_at IS NOT NULL OR next_job.score_json IS NOT NULL
  OR next_job.eval_error_json IS NOT NULL OR next_job.artifact_manifest_json IS NOT NULL OR next_job.artifact_manifest_sha256 IS NOT NULL
  OR next_job.lease_owner IS NOT NULL OR next_job.lease_expires_at IS NOT NULL
  OR next_job.executor_host_id IS NOT NULL OR next_job.executor_slot_id IS NOT NULL OR next_job.executor_fencing_token IS NOT NULL
  OR next_job.execution_deadline_at IS NOT NULL OR next_job.available_at IS DISTINCT FROM audit.authorized_at
  OR (to_jsonb(next_job)-ARRAY['evaluation_cycle','cycle_attempt_offset','cycle_started_at','state','completed_at',
    'score_json','eval_error_json','artifact_manifest_json','artifact_manifest_sha256','available_at',
    'executor_host_id','executor_slot_id','executor_fencing_token','execution_deadline_at'])
   IS DISTINCT FROM
   (to_jsonb(previous_job)-ARRAY['evaluation_cycle','cycle_attempt_offset','cycle_started_at','state','completed_at',
    'score_json','eval_error_json','artifact_manifest_json','artifact_manifest_sha256','available_at',
    'executor_host_id','executor_slot_id','executor_fencing_token','execution_deadline_at']) THEN RETURN false; END IF;
 RETURN true;
END $distributed_recovery_transition$;

CREATE FUNCTION competition_distributed_recovery_audit_guard() RETURNS trigger LANGUAGE plpgsql AS $distributed_recovery_audit$
DECLARE epoch_round competition_round%ROWTYPE; policy jsonb; authority jsonb; proof jsonb; native jsonb; expected jsonb; host jsonb; idle_count integer;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 policy:=epoch_round.policy_json->'distributed_evaluation_policy';
 IF policy IS NULL OR policy='null'::jsonb THEN
  IF NEW.authorization_json ? 'distributed' OR NEW.authorization_json->>'schema'='2' THEN
   RAISE EXCEPTION 'distributed recovery cannot replace singleton authority';
  END IF;
  RETURN NEW;
 END IF;
 PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
 authority:=NEW.authorization_json->'distributed'; proof:=NEW.receipt_json->'distributed_proof'; native:=proof->'original_status';
 IF NEW.authorization_json->>'schema' IS DISTINCT FROM '2'
  OR NEW.authorization_json->>'kind' IS DISTINCT FROM 'competition-distributed-job-reevaluation-authorization/v1'
  OR NEW.authorization_json->>'incident_class' IS DISTINCT FROM 'operator_authorized_distributed_infrastructure_v1'
  OR NEW.authorization_json->'single_use' IS DISTINCT FROM 'true'::jsonb
  OR NEW.authorization_json->'expected_cycle' IS DISTINCT FROM '0'::jsonb
  OR NEW.receipt_json->>'schema' IS DISTINCT FROM '2' OR NEW.receipt_json->>'cycle' IS DISTINCT FROM '1'
  OR NEW.receipt_json->>'incident_class' IS DISTINCT FROM NEW.authorization_json->>'incident_class'
  OR NEW.receipt_json->>'authorization_sha256' IS DISTINCT FROM NEW.authorization_sha256
  OR NEW.authorization_json->'expected_terminal_snapshot' IS DISTINCT FROM NEW.original_terminal_json
  OR NEW.receipt_json->>'request_sha256' IS DISTINCT FROM NEW.request_sha256
  OR NEW.receipt_json->>'runtime_qualification_sha256' IS DISTINCT FROM NEW.authorization_json->>'runtime_qualification_sha256'
  OR NEW.receipt_json->>'fixed_source_commit' IS DISTINCT FROM NEW.authorization_json->>'fixed_source_commit'
  OR NEW.receipt_json->>'fixed_worker_sha256' IS DISTINCT FROM NEW.authorization_json->>'fixed_worker_sha256'
  OR NEW.receipt_json->>'fixed_worker_image_digest' IS DISTINCT FROM NEW.authorization_json->>'fixed_worker_image_digest'
  OR NEW.receipt_json->>'api_image_digest' IS DISTINCT FROM NEW.authorization_json->>'api_image_digest'
  OR NEW.receipt_json->>'round_policy_sha256' IS DISTINCT FROM NEW.authorization_json->>'round_policy_sha256'
  OR NEW.receipt_json->>'baseline_sha256' IS DISTINCT FROM NEW.authorization_json->>'baseline_sha256'
  OR ((NEW.authorization_json->>'authorized_at')::timestamptz AT TIME ZONE 'UTC'<=NEW.authorized_at) IS DISTINCT FROM true
  OR ((NEW.authorization_json->>'expires_at')::timestamptz AT TIME ZONE 'UTC'>NEW.authorized_at) IS DISTINCT FROM true
  OR NEW.receipt_json->>'previous_terminal_sha256' IS DISTINCT FROM NEW.authorization_json->>'expected_terminal_sha256'
  OR NEW.receipt_json->'previous_failure'->>'kind' IS DISTINCT FROM NEW.original_terminal_json->'eval_error_json'->>'kind'
  OR NEW.receipt_json->'previous_failure'->>'code' IS DISTINCT FROM NEW.original_terminal_json->'eval_error_json'->>'code'
  OR NEW.original_terminal_json->'eval_error_json'->>'kind' IS DISTINCT FROM 'infrastructure'
  OR NEW.original_terminal_json->'eval_error_json'->>'code' IS DISTINCT FROM 'evaluation_time_budget_exhausted'
  OR NEW.original_terminal_json->'eval_error_json'->'retriable' IS DISTINCT FROM 'false'::jsonb
  OR NEW.receipt_json->'previous_failure'->'retriable' IS DISTINCT FROM 'false'::jsonb
  OR NEW.receipt_json->>'execution_timeout_seconds' IS DISTINCT FROM '10800'
  OR authority->>'schema' IS DISTINCT FROM '1' OR proof->>'schema' IS DISTINCT FROM '1'
  OR proof->>'authorization_sha256' IS DISTINCT FROM NEW.authorization_sha256
  OR proof->>'original_terminal_sha256' IS DISTINCT FROM NEW.authorization_json->>'expected_terminal_sha256'
  OR ((proof->>'database_bracket_started_at')::timestamptz<=clock_timestamp()) IS DISTINCT FROM true
  OR ((proof->>'database_bracket_expires_at')::timestamptz>clock_timestamp()) IS DISTINCT FROM true
  OR COALESCE(NEW.receipt_json->'distributed'->>'native_proof_sha256','') !~ '^[0-9a-f]{64}$'
  OR NEW.receipt_json->'distributed'->>'native_proof_sha256' IS DISTINCT FROM current_setting('urnetwork.competition_distributed_recovery_proof',true)
  OR authority->>'original_host_id' IS DISTINCT FROM NEW.original_terminal_json->>'executor_host_id'
  OR authority->'original_slot_id' IS DISTINCT FROM NEW.original_terminal_json->'executor_slot_id'
  OR authority->'original_fencing_token' IS DISTINCT FROM NEW.original_terminal_json->'executor_fencing_token'
  OR (authority->>'original_started_at')::timestamptz AT TIME ZONE 'UTC' IS DISTINCT FROM (NEW.original_terminal_json->>'started_at')::timestamp
  OR (authority->>'original_deadline_at')::timestamptz AT TIME ZONE 'UTC' IS DISTINCT FROM (NEW.original_terminal_json->>'execution_deadline_at')::timestamp
  OR (NEW.original_terminal_json->>'execution_deadline_at')::timestamp IS DISTINCT FROM (NEW.original_terminal_json->>'started_at')::timestamp+interval '3 hours'
  OR (NEW.original_terminal_json->>'completed_at')::timestamp<(NEW.original_terminal_json->>'execution_deadline_at')::timestamp
  OR authority->>'source_lock_sha256' IS DISTINCT FROM epoch_round.policy_json->'automation_policy'->>'source_lock_sha256'
  OR authority->>'evaluator_image_digest' IS DISTINCT FROM epoch_round.policy_json->>'evaluator_image_digest'
  OR authority->>'policy_approval_sha256' IS DISTINCT FROM policy->>'approval_sha256'
  OR COALESCE(authority->>'closure_evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(authority->>'reason','') NOT IN ('retention_budget_exhausted','closed_legacy_prelaunch')
  OR jsonb_typeof(authority->'idle_slots') IS DISTINCT FROM 'array'
  OR proof->'idle_slots' IS DISTINCT FROM authority->'idle_slots'
  OR jsonb_typeof(proof->'availability') IS DISTINCT FROM 'array'
  OR NOT EXISTS(SELECT 1 FROM competition_round_baseline b WHERE b.round_id=NEW.round_id AND b.baseline_sha256=NEW.authorization_json->>'baseline_sha256')
  OR NOT EXISTS(SELECT 1 FROM competition_supervisor_lease l WHERE l.environment=NEW.environment AND l.competition_id=NEW.competition_id AND l.season_id=NEW.season_id
   AND l.owner_id=authority->>'supervisor_owner_id' AND l.fencing_token::text=authority->>'supervisor_fencing_token'
   AND l.owner_id=current_setting('urnetwork.competition_v2_owner',true) AND l.fencing_token::text=current_setting('urnetwork.competition_v2_fence',true)
   AND clock_timestamp()<l.lease_expires_at)
  OR (SELECT active FROM competition_automation_hold h WHERE h.environment=NEW.environment AND h.competition_id=NEW.competition_id AND h.season_id=NEW.season_id AND h.kind='evaluation' ORDER BY revision DESC LIMIT 1) IS DISTINCT FROM true
  OR EXISTS(SELECT 1 FROM competition_job WHERE state='running')
  OR EXISTS(SELECT 1 FROM competition_worker_slot WHERE worker_id IS NOT NULL OR job_id IS NOT NULL OR lease_expires_at IS NOT NULL)
  OR EXISTS(SELECT 1 FROM competition_execution_slot WHERE worker_id IS NOT NULL OR job_id IS NOT NULL OR lease_expires_at IS NOT NULL) THEN
  RAISE EXCEPTION 'distributed recovery requires exact held idle fenced closure authority';
 END IF;
 idle_count:=jsonb_array_length(authority->'idle_slots');
 IF idle_count<2 OR idle_count<>jsonb_array_length(policy->'hosts') OR idle_count<>jsonb_array_length(proof->'availability')
  OR (SELECT count(DISTINCT e->>'host_id') FROM jsonb_array_elements(authority->'idle_slots') e)<>idle_count
  OR (SELECT count(DISTINCT e->>'slot_id') FROM jsonb_array_elements(authority->'idle_slots') e)<>idle_count THEN
  RAISE EXCEPTION 'distributed recovery requires every distinct frozen host';
 END IF;
 FOR host IN SELECT * FROM jsonb_array_elements(policy->'hosts') LOOP
  SELECT e INTO expected FROM jsonb_array_elements(authority->'idle_slots') e WHERE e->>'host_id'=host->>'host_id' AND e->'slot_id'=host->'slot_id';
  IF NOT FOUND OR NOT EXISTS(SELECT 1 FROM competition_execution_slot s WHERE s.host_id=host->>'host_id' AND s.slot_id=(host->>'slot_id')::integer
   AND s.fencing_token=(expected->>'fencing_token')::bigint AND s.worker_id IS NULL AND s.job_id IS NULL AND s.lease_expires_at IS NULL)
   OR NOT EXISTS(SELECT 1 FROM jsonb_array_elements(proof->'availability') e WHERE e->>'host_id'=host->>'host_id' AND e->>'schema'='1'
    AND e->>'state'='available' AND e->>'exit_code'='0' AND COALESCE(e->>'error_code','')='' AND COALESCE(e->>'run_id','')=''
    AND COALESCE(e->>'fencing_token','0')='0' AND COALESCE(e->>'signature','')<>'') THEN
   RAISE EXCEPTION 'distributed recovery cannot omit or replace a host closure';
  END IF;
 END LOOP;
 IF native->>'host_id' IS DISTINCT FROM authority->>'original_host_id'
  OR native->>'run_id' IS DISTINCT FROM authority->>'original_run_id'
  OR native->>'manifest_sha256' IS DISTINCT FROM authority->>'original_manifest_sha256'
  OR native->>'request_sha256' IS DISTINCT FROM authority->>'original_request_sha256'
  OR native->'fencing_token' IS DISTINCT FROM authority->'native_fencing_token'
  OR native->'identity'->>'job_id' IS DISTINCT FROM NEW.job_id::text
  OR native->'identity'->>'round_id' IS DISTINCT FROM NEW.round_id::text
  OR native->'identity'->>'run_id' IS DISTINCT FROM authority->>'original_run_id'
  OR native->'identity'->>'cycle' IS DISTINCT FROM '0'
  OR native->'identity'->'attempt' IS DISTINCT FROM NEW.original_terminal_json->'attempt_count'
  OR COALESCE(native->>'signature','')=''
  OR (authority->>'reason'='retention_budget_exhausted' AND (native->>'state' IS DISTINCT FROM 'succeeded' OR native->>'exit_code' IS DISTINCT FROM '0' OR COALESCE(native->>'error_code','')<>''))
  OR (authority->>'reason'='closed_legacy_prelaunch' AND (native->>'state' IS DISTINCT FROM 'failed' OR native->>'exit_code' IS DISTINCT FROM '-1' OR native->>'error_code' IS DISTINCT FROM 'legacy_prelaunch_infrastructure_failure')) THEN
  RAISE EXCEPTION 'distributed recovery requires the exact closed original native run';
 END IF;
 RETURN NEW;
END $distributed_recovery_audit$;
CREATE TRIGGER competition_job_distributed_recovery_audit BEFORE INSERT ON competition_job_reevaluation
 FOR EACH ROW EXECUTE FUNCTION competition_distributed_recovery_audit_guard();

CREATE FUNCTION competition_distributed_recovery_commit_guard() RETURNS trigger LANGUAGE plpgsql AS $distributed_recovery_commit$
DECLARE expected jsonb;
BEGIN
 IF NEW.authorization_json->>'schema' IS DISTINCT FROM '2' THEN RETURN NEW; END IF;
 IF ((NEW.receipt_json->'distributed_proof'->>'database_bracket_expires_at')::timestamptz>clock_timestamp()) IS DISTINCT FROM true THEN
  RAISE EXCEPTION 'distributed recovery native observation expired before commit';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM competition_job j WHERE j.job_id=NEW.job_id AND j.state='queued' AND j.evaluation_cycle=1
  AND j.executor_host_id IS NULL AND j.executor_slot_id IS NULL AND j.executor_fencing_token IS NULL AND j.execution_deadline_at IS NULL
  AND j.started_at=(NEW.original_terminal_json->>'started_at')::timestamp AND j.cycle_started_at IS NULL) THEN
  RAISE EXCEPTION 'distributed recovery must commit only a clockless queued cycle';
 END IF;
 FOR expected IN SELECT * FROM jsonb_array_elements(NEW.authorization_json->'distributed'->'idle_slots') LOOP
  IF NOT EXISTS(SELECT 1 FROM competition_execution_slot s WHERE s.host_id=expected->>'host_id' AND s.slot_id=(expected->>'slot_id')::integer
   AND s.fencing_token=(expected->>'fencing_token')::bigint+1 AND s.worker_id IS NULL AND s.job_id IS NULL AND s.lease_expires_at IS NULL) THEN
   RAISE EXCEPTION 'distributed recovery must advance every idle database host fence exactly once';
  END IF;
 END LOOP;
 RETURN NEW;
END $distributed_recovery_commit$;
CREATE CONSTRAINT TRIGGER competition_job_distributed_recovery_atomic AFTER INSERT ON competition_job_reevaluation
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION competition_distributed_recovery_commit_guard();
`

// Each successor embeds the complete reviewed predecessor, adding only the
// same exact same-transaction distributed transition. Legacy branches survive.
func competitionDistributedReevaluationSuccessorBodies() (immutable, executor, intake string) {
	_, _, immutable, _ = CompetitionJobReevaluationBodiesSql()
	executor = CompetitionExecutorAdmissionClaimGuardBodySql()
	for _, function := range CompetitionIntakeFirstFunctionsSql() {
		if function.Signature == "competition_v2_intake_execution_guard()" {
			intake = function.Body
		}
	}
	insert := func(body, marker, addition string) string {
		if strings.Count(body, marker) != 1 {
			panic("distributed recovery predecessor authority changed")
		}
		return strings.Replace(body, marker, marker+addition, 1)
	}
	immutable = insert(immutable, "\n\tBEGIN", `
 IF competition_distributed_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;`)
	executor = insert(executor, " policy:=epoch_round.policy_json->'distributed_evaluation_policy';", `
 IF TG_OP='UPDATE' AND OLD.evaluation_cycle=0 AND NEW.evaluation_cycle=1 AND policy IS NOT NULL AND policy<>'null'::jsonb THEN
  IF NOT competition_distributed_recovery_transition_allows(OLD,NEW) THEN
   RAISE EXCEPTION 'distributed cycle transition requires its separately audited closure';
  END IF;
  RETURN NEW;
 END IF;`)
	intake = insert(intake, "\nBEGIN", `
 IF TG_OP='UPDATE' AND competition_distributed_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;`)
	return immutable, executor, intake
}

// Only prospective audited recovery gains a path; no old state or migration
// is rewritten and old running workers keep their cycle-zero authority.
func CompetitionDistributedReevaluationMigrationSql() string {
	immutable, executor, intake := competitionDistributedReevaluationSuccessorBodies()
	return competitionDistributedReevaluationAuthoritySql +
		"CREATE OR REPLACE FUNCTION competition_job_immutable_guard() RETURNS trigger LANGUAGE plpgsql AS $distributed_immutable$" + immutable + "$distributed_immutable$;\n" +
		"CREATE OR REPLACE FUNCTION competition_executor_job_guard() RETURNS trigger LANGUAGE plpgsql AS $distributed_executor$" + executor + "$distributed_executor$;\n" +
		"CREATE OR REPLACE FUNCTION competition_v2_intake_execution_guard() RETURNS trigger LANGUAGE plpgsql AS $distributed_intake$" + intake + "$distributed_intake$;\n"
}

// The actual append-only position, not an assumed ordinal, owns readiness.
func CompetitionDistributedReevaluationMigrationVersion() int {
	return sqlMigrationVersion(CompetitionDistributedReevaluationMigrationSql())
}

// Catalog qualification independently owns every new and replaced exact body.
func CompetitionDistributedReevaluationBodiesSql() (transition, audit, commit, immutable, executor, intake string) {
	transition = strings.Split(competitionDistributedReevaluationAuthoritySql, "$distributed_recovery_transition$")[1]
	audit = strings.Split(competitionDistributedReevaluationAuthoritySql, "$distributed_recovery_audit$")[1]
	commit = strings.Split(competitionDistributedReevaluationAuthoritySql, "$distributed_recovery_commit$")[1]
	immutable, executor, intake = competitionDistributedReevaluationSuccessorBodies()
	return
}
