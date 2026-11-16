// An explicit unchanged-patch retry is additive authority, not a relaxation of
// the published distributed infrastructure diagnosis or its original bytes.
package server

import "strings"

// Catalog custody owns the separately typed validators and every successor
// wrapper; unchanged distributed transition and deferred commit remain owned.
type CompetitionOperatorRequestedPatchRetrySqlBodies struct {
	Transition       string
	AuditValidation  string
	InsertValidation string
	Insert           string
	Audit            string
	Immutable        string
	Executor         string
	Intake           string
}

// Replacements are tied to exact reviewed predecessor text. A predecessor
// change must be audited instead of silently widening a patch retry gate.
func competitionPatchRetryReplace(body, previous, next string) string {
	if strings.Count(body, previous) != 1 {
		panic("operator requested patch retry predecessor changed")
	}
	return strings.Replace(body, previous, next, 1)
}

// The complete old authority is retained in each wrapper. Only the new exact
// authorization kind reaches the independently validated row-returning branch.
func CompetitionOperatorRequestedPatchRetryBodiesSql() CompetitionOperatorRequestedPatchRetrySqlBodies {
	transition, audit, _, immutable, executor, intake := CompetitionDistributedReevaluationBodiesSql()
	_, insert := CompetitionReevaluationPostCloseBodiesSql()
	bodies := CompetitionOperatorRequestedPatchRetrySqlBodies{}
	bodies.Transition = strings.NewReplacer(
		"competition-distributed-job-reevaluation-authorization/v1", "competition-operator-requested-patch-retry-authorization/v1",
		"operator_authorized_distributed_infrastructure_v1", "operator_requested_patch_retry",
		"urnetwork.competition_distributed_recovery_proof", "urnetwork.competition_patch_retry_proof",
	).Replace(transition)
	bodies.Transition = competitionPatchRetryReplace(bodies.Transition, "  OR previous_job.executor_host_id IS NULL", `
  OR previous_job.eval_error_json->>'kind' IS DISTINCT FROM 'infrastructure'
  OR previous_job.eval_error_json->>'code' IS DISTINCT FROM 'executor_process_failed'
  OR previous_job.eval_error_json->'retriable' IS DISTINCT FROM 'false'::jsonb
  OR audit.authorization_json->'distributed'->>'reason' IS DISTINCT FROM 'operator_requested_patch_retry'
  OR audit.authorization_json->'distributed'->'patch_retry'->>'patch_sha256' IS DISTINCT FROM previous_job.patch_sha256
  OR audit.receipt_json->'distributed'->'patch_retry' IS DISTINCT FROM audit.authorization_json->'distributed'->'patch_retry'
  OR previous_job.executor_host_id IS NULL`)
	bodies.AuditValidation = strings.NewReplacer(
		"competition-distributed-job-reevaluation-authorization/v1", "competition-operator-requested-patch-retry-authorization/v1",
		"operator_authorized_distributed_infrastructure_v1", "operator_requested_patch_retry",
		"urnetwork.competition_distributed_recovery_proof", "urnetwork.competition_patch_retry_proof",
		"IS DISTINCT FROM 'evaluation_time_budget_exhausted'", "IS DISTINCT FROM 'executor_process_failed'",
	).Replace(audit)
	bodies.AuditValidation = competitionPatchRetryReplace(bodies.AuditValidation,
		"  OR (NEW.original_terminal_json->>'completed_at')::timestamp<(NEW.original_terminal_json->>'execution_deadline_at')::timestamp",
		`  OR (NEW.original_terminal_json->>'completed_at')::timestamp<(NEW.original_terminal_json->>'started_at')::timestamp
  OR (NEW.original_terminal_json->>'completed_at')::timestamp>=(NEW.original_terminal_json->>'execution_deadline_at')::timestamp`)
	bodies.AuditValidation = competitionPatchRetryReplace(bodies.AuditValidation,
		"  OR COALESCE(authority->>'reason','') NOT IN ('retention_budget_exhausted','closed_legacy_prelaunch')",
		`  OR authority->>'reason' IS DISTINCT FROM 'operator_requested_patch_retry'
  OR jsonb_typeof(authority->'patch_retry') IS DISTINCT FROM 'object'
  OR (authority->'patch_retry') - ARRAY['schema','patch_sha256','applicability_evidence_sha256']<>'{}'::jsonb
  OR authority->'patch_retry'->>'schema' IS DISTINCT FROM '1'
  OR authority->'patch_retry'->>'patch_sha256' IS DISTINCT FROM NEW.original_terminal_json->>'patch_sha256'
  OR NEW.receipt_json->>'patch_sha256' IS DISTINCT FROM NEW.original_terminal_json->>'patch_sha256'
  OR COALESCE(authority->'patch_retry'->>'patch_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(authority->'patch_retry'->>'applicability_evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR NEW.receipt_json->'distributed'->'patch_retry' IS DISTINCT FROM authority->'patch_retry'
  OR NEW.receipt_json->'distributed'->>'reason' IS DISTINCT FROM 'operator_requested_patch_retry'
  OR COALESCE(NEW.authorization_json->>'evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR NEW.authorization_json->>'expected_attempt_count' IS DISTINCT FROM NEW.original_terminal_json->>'attempt_count'
  OR NEW.authorization_json->>'incident_attempt' IS DISTINCT FROM NEW.original_terminal_json->>'attempt_count'
  OR NEW.original_terminal_json->>'completed_at' IS NULL
  OR NEW.authorization_json->>'job_id' IS DISTINCT FROM NEW.job_id::text
  OR NEW.authorization_json->>'round_id' IS DISTINCT FROM NEW.round_id::text
  OR NEW.authorization_json->>'environment' IS DISTINCT FROM NEW.environment
  OR NEW.authorization_json->>'competition_id' IS DISTINCT FROM NEW.competition_id
  OR NEW.authorization_json->>'season_id' IS DISTINCT FROM NEW.season_id
  OR NEW.receipt_json->'previous_failure'->>'kind' IS DISTINCT FROM 'infrastructure'
  OR NEW.receipt_json->'previous_failure'->>'code' IS DISTINCT FROM 'executor_process_failed'
  OR NOT EXISTS(SELECT 1 FROM competition_job_event e WHERE e.event_id::text=NEW.authorization_json->>'incident_event_id'
   AND e.job_id=NEW.job_id AND e.event_type='failed' AND e.payload_sha256=NEW.authorization_json->>'incident_event_sha256'
   AND e.event_at=(NEW.original_terminal_json->>'completed_at')::timestamp
   AND e.payload_json->>'attempt'=NEW.original_terminal_json->>'attempt_count')`)
	bodies.AuditValidation = competitionPatchRetryReplace(bodies.AuditValidation,
		`  OR (authority->>'reason'='retention_budget_exhausted' AND (native->>'state' IS DISTINCT FROM 'succeeded' OR native->>'exit_code' IS DISTINCT FROM '0' OR COALESCE(native->>'error_code','')<>''))
  OR (authority->>'reason'='closed_legacy_prelaunch' AND (native->>'state' IS DISTINCT FROM 'failed' OR native->>'exit_code' IS DISTINCT FROM '-1' OR native->>'error_code' IS DISTINCT FROM 'legacy_prelaunch_infrastructure_failure'))`,
		`  OR native->>'state' IS DISTINCT FROM 'failed' OR native->>'exit_code' IS DISTINCT FROM '1'
  OR native->>'error_code' IS DISTINCT FROM 'evaluator_process_failed'`)
	bodies.AuditValidation = strings.NewReplacer("NEW.", "next_audit.", "RETURN NEW;", "RETURN next_audit;").Replace(bodies.AuditValidation)
	bodies.InsertValidation = competitionPatchRetryReplace(insert,
		"j.eval_error_json->>'code' IN ('artifact_archive_failed','evaluation_time_budget_exhausted')", "j.eval_error_json->>'code'='executor_process_failed'")
	bodies.InsertValidation = competitionPatchRetryReplace(bodies.InsertValidation, "\n\tBEGIN", `
	BEGIN
 IF NEW.authorization_json->>'schema' IS DISTINCT FROM '2'
  OR NEW.authorization_json->>'kind' IS DISTINCT FROM 'competition-operator-requested-patch-retry-authorization/v1'
  OR NEW.authorization_json->>'incident_class' IS DISTINCT FROM 'operator_requested_patch_retry'
  OR NEW.authorization_json->'distributed'->>'reason' IS DISTINCT FROM 'operator_requested_patch_retry' THEN
  RAISE EXCEPTION 'patch retry insert requires separate exact operator authority';
 END IF;`)
	bodies.InsertValidation = strings.NewReplacer("NEW.", "next_audit.", "RETURN NEW;", "RETURN next_audit;").Replace(bodies.InsertValidation)
	bodies.Insert = competitionPatchRetryReplace(insert, "\n\tBEGIN", `
	BEGIN
 IF NEW.authorization_json->>'kind'='competition-operator-requested-patch-retry-authorization/v1' THEN
  RETURN competition_patch_retry_insert_validate(NEW);
 END IF;`)
	bodies.Audit = competitionPatchRetryReplace(audit, "\nBEGIN", `
BEGIN
 IF NEW.authorization_json->>'kind'='competition-operator-requested-patch-retry-authorization/v1' THEN
  RETURN competition_patch_retry_audit_validate(NEW);
 END IF;`)
	bodies.Immutable = competitionPatchRetryReplace(immutable, "\n\tBEGIN", `
	BEGIN
 IF competition_patch_retry_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;`)
	bodies.Executor = competitionPatchRetryReplace(executor, "\nBEGIN", `
BEGIN
 IF TG_OP='UPDATE' AND competition_patch_retry_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;`)
	bodies.Intake = competitionPatchRetryReplace(intake, "\nBEGIN", `
BEGIN
 IF TG_OP='UPDATE' AND competition_patch_retry_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;`)
	return bodies
}

// Migration-first wrappers preserve all old query shapes and guard branches.
// Ddl alone grants no retry, clears no hold and changes no job or source row.
func CompetitionOperatorRequestedPatchRetryMigrationSql() string {
	bodies := CompetitionOperatorRequestedPatchRetryBodiesSql()
	create := func(name, arguments, result, body string) string {
		return "CREATE OR REPLACE FUNCTION " + name + "(" + arguments + ") RETURNS " + result + " LANGUAGE plpgsql AS $patch_retry$" + body + "$patch_retry$;\n"
	}
	return "SET LOCAL lock_timeout='2s';\n" +
		create("competition_patch_retry_transition_allows", "previous_job competition_job,next_job competition_job", "boolean", bodies.Transition) +
		create("competition_patch_retry_insert_validate", "next_audit competition_job_reevaluation", "competition_job_reevaluation", bodies.InsertValidation) +
		create("competition_patch_retry_audit_validate", "next_audit competition_job_reevaluation", "competition_job_reevaluation", bodies.AuditValidation) +
		create("competition_job_reevaluation_insert_guard", "", "trigger", bodies.Insert) +
		create("competition_distributed_recovery_audit_guard", "", "trigger", bodies.Audit) +
		create("competition_job_immutable_guard", "", "trigger", bodies.Immutable) +
		create("competition_executor_job_guard", "", "trigger", bodies.Executor) +
		create("competition_v2_intake_execution_guard", "", "trigger", bodies.Intake)
}

// The registered append position owns readiness, not a frozen version number.
func CompetitionOperatorRequestedPatchRetryMigrationVersion() int {
	return sqlMigrationVersion(CompetitionOperatorRequestedPatchRetryMigrationSql())
}
