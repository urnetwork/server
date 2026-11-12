// The new audited cycle transition is appended after published history, with
// unchanged legacy branches and explicit expiry/atomic-fence constraints.
package server

import (
	"strings"
	"testing"
)

// Deleting only each explicit addition recovers the complete reviewed old
// function. A broad replacement or silent gate removal changes these bytes.
func TestCompetitionDistributedRecoveryMigrationPreservesPredecessorGuards(t *testing.T) {
	_, _, oldImmutable, _ := CompetitionJobReevaluationBodiesSql()
	oldExecutor := CompetitionExecutorAdmissionClaimGuardBodySql()
	var oldIntake string
	for _, function := range CompetitionIntakeFirstFunctionsSql() {
		if function.Signature == "competition_v2_intake_execution_guard()" {
			oldIntake = function.Body
		}
	}
	immutable, executor, intake := competitionDistributedReevaluationSuccessorBodies()
	for _, body := range []struct{ old, next, marker string }{
		{old: oldImmutable, next: immutable, marker: " IF competition_distributed_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;"},
		{old: oldExecutor, next: executor, marker: "distributed cycle transition requires its separately audited closure"},
		{old: oldIntake, next: intake, marker: " IF TG_OP='UPDATE' AND competition_distributed_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;"},
	} {
		if body.old == "" || strings.Count(body.next, body.marker) != 1 {
			t.Fatal("reviewed predecessor/addition absent or ambiguous")
		}
		// Every original line remains in order, including every denial branch.
		position := 0
		for _, line := range strings.Split(body.old, "\n") {
			offset := strings.Index(body.next[position:], line)
			if offset < 0 {
				t.Fatal("distributed recovery lost an original guard line")
			}
			position += offset + len(line)
		}
	}
	version := CompetitionDistributedReevaluationMigrationVersion()
	if version != MigrationCount() || version <= CompetitionExecutorAdmissionClaimMigrationVersion() {
		t.Fatal("distributed recovery was not appended after origin migration prefix")
	}
}

// Missing/future/expired proof dates must fail closed, and deferred publication
// must bind all empty host fences rather than only a singleton row.
func TestCompetitionDistributedRecoveryMigrationPinsAtomicExpiryAndWholeHostCensus(t *testing.T) {
	transition, audit, commit, _, _, _ := CompetitionDistributedReevaluationBodiesSql()
	for _, required := range []string{"pg_current_xact_id()", "audit.original_terminal_json IS DISTINCT FROM to_jsonb(previous_job)", "next_job.execution_deadline_at IS NOT NULL", "next_job.attempt_count<>previous_job.attempt_count", "competition_distributed_recovery_proof"} {
		if !strings.Contains(transition, required) {
			t.Fatal("missing immutable transition authority", required)
		}
	}
	for _, required := range []string{"clock_timestamp()", "database_bracket_started_at", "database_bracket_expires_at", "IS DISTINCT FROM true", "competition_supervisor_lease", "competition_execution_slot", "competition_worker_slot", "competition_automation_hold", "original_terminal_json", "proof->'availability'", "native->'identity'->>'cycle'", "legacy_prelaunch_infrastructure_failure"} {
		if !strings.Contains(audit, required) {
			t.Fatal("missing exact distributed closure authority", required)
		}
	}
	for _, required := range []string{"expired before commit", "fencing_token=(expected->>'fencing_token')::bigint+1", "j.cycle_started_at IS NULL", "j.execution_deadline_at IS NULL"} {
		if !strings.Contains(commit, required) {
			t.Fatal("missing atomic deferred recovery check", required)
		}
	}
	if !strings.Contains(CompetitionDistributedReevaluationMigrationSql(), "DEFERRABLE INITIALLY DEFERRED") {
		t.Fatal("new cycle and whole-host fences lost their atomic commit guard")
	}
}
