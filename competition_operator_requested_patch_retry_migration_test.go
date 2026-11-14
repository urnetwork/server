// New authority is appended without changing published infrastructure bytes;
// predecessor wrappers keep every legacy denial and own only an exact new path.
package server

import (
	"strings"
	"testing"
)

// Removing only the explicitly reviewed dispatch restores each entire prior
// body byte for byte, including its unchanged old infrastructure branch.
func TestCompetitionOperatorPatchRetryMigrationPreservesCompletePredecessors(t *testing.T) {
	bodies := CompetitionOperatorRequestedPatchRetryBodiesSql()
	_, oldAudit, _, oldImmutable, oldExecutor, oldIntake := CompetitionDistributedReevaluationBodiesSql()
	_, oldInsert := CompetitionReevaluationPostCloseBodiesSql()
	for _, wrapper := range []struct{ previous, next, addition string }{
		{previous: oldInsert, next: bodies.Insert, addition: "\n IF NEW.authorization_json->>'kind'='competition-operator-requested-patch-retry-authorization/v1' THEN\n  RETURN competition_patch_retry_insert_validate(NEW);\n END IF;"},
		{previous: oldAudit, next: bodies.Audit, addition: "\n IF NEW.authorization_json->>'kind'='competition-operator-requested-patch-retry-authorization/v1' THEN\n  RETURN competition_patch_retry_audit_validate(NEW);\n END IF;"},
		{previous: oldImmutable, next: bodies.Immutable, addition: "\n IF competition_patch_retry_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;"},
		{previous: oldExecutor, next: bodies.Executor, addition: "\n IF TG_OP='UPDATE' AND competition_patch_retry_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;"},
		{previous: oldIntake, next: bodies.Intake, addition: "\n IF TG_OP='UPDATE' AND competition_patch_retry_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;"},
	} {
		if strings.Count(wrapper.next, wrapper.addition) != 1 || strings.Replace(wrapper.next, wrapper.addition, "", 1) != wrapper.previous {
			t.Fatal("patch retry did not preserve its complete predecessor verbatim")
		}
	}
	if strings.Contains(CompetitionDistributedReevaluationMigrationSql(), "operator_requested_patch_retry") {
		t.Fatal("new patch retry altered published infrastructure migration bytes")
	}
	version := CompetitionOperatorRequestedPatchRetryMigrationVersion()
	if GiftSchemaVersion() != CompetitionDistributedReevaluationMigrationVersion()+1 || GiftRefundSchemaVersion() != GiftSchemaVersion()+1 ||
		version != GiftRefundSchemaVersion()+1 || version != MigrationCount() {
		t.Fatal("patch retry was not appended after the complete published prefix")
	}
}

// The separate validators retain current scope/transaction, unchanged input,
// actual terminal event, explicit applicability evidence and native closure.
func TestCompetitionOperatorPatchRetryMigrationPinsExactAuthorityAndNativeBarrier(t *testing.T) {
	bodies := CompetitionOperatorRequestedPatchRetryBodiesSql()
	for _, required := range []string{"operator_requested_patch_retry", "competition_patch_retry_proof", "executor_process_failed", "audit.original_terminal_json IS DISTINCT FROM to_jsonb(previous_job)", "next_job.attempt_count<>previous_job.attempt_count", "next_job.execution_deadline_at IS NOT NULL"} {
		if !strings.Contains(bodies.Transition, required) {
			t.Fatal("missing exact patch transition guard", required)
		}
	}
	for _, required := range []string{"competition-operator-requested-patch-retry-authorization/v1", "applicability_evidence_sha256", "competition_job_event", "incident_event_sha256", "expected_attempt_count", "'patch_sha256'", "IS DISTINCT FROM 'executor_process_failed'", "IS DISTINCT FROM 'evaluator_process_failed'", "database_bracket_expires_at", "competition_supervisor_lease", "competition_execution_slot", "competition_worker_slot", "competition_automation_hold", "proof->'availability'"} {
		if !strings.Contains(bodies.AuditValidation, required) {
			t.Fatal("missing exact patch audit guard", required)
		}
	}
	if strings.Contains(bodies.AuditValidation, "retention_budget_exhausted") || strings.Contains(bodies.AuditValidation, "closed_legacy_prelaunch") {
		t.Fatal("patch retry can borrow an infrastructure diagnosis")
	}
	if !strings.Contains(bodies.InsertValidation, "j.eval_error_json->>'code'='executor_process_failed'") ||
		!strings.Contains(bodies.InsertValidation, "competition_job_reevaluation_post_close_allows") ||
		!strings.Contains(bodies.InsertValidation, "to_jsonb(j)=next_audit.original_terminal_json") {
		t.Fatal("new insert validator lost exact original/window authority")
	}
}
