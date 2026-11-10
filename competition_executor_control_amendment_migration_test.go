// Migration-first compatibility keeps historical execution and source guards.
package server

import (
	"strings"
	"testing"
)

// The new overlay never disables an existing guard or rewrites its evidence.
func TestExecutorControlAmendmentMigrationPreservesFrozenAuthority(t *testing.T) {
	sql := CompetitionExecutorControlAmendmentMigrationSql()
	for _, forbidden := range []string{"DROP TRIGGER", "DISABLE TRIGGER", "UPDATE competition_round", "UPDATE competition_source_intake_activation", "UPDATE competition_job", "DELETE FROM", "ALTER TABLE competition_job", "CREATE OR REPLACE FUNCTION competition_round_immutable_guard"} {
		if strings.Contains(sql, forbidden) {
			t.Fatal("control correction rewrites frozen authority", forbidden)
		}
	}
	for _, required := range []string{"CREATE TABLE competition_executor_control_amendment", "competition_executor_control_amendment_append_only", "competition_executor_control_amendment_no_truncate", "FOR UPDATE", "competition_v2_scope_lock", "clock_timestamp()<lease_expires_at", "attempt_count<>0 OR started_at IS NOT NULL OR execution_deadline_at IS NOT NULL", "EXISTS(SELECT 1 FROM competition_executor_qualification_run", "stale coordinator cannot start a host-admission clock", "control_amendment_sha256", "previous_qualification_sha256", "previous_agent_sha256", "previous_builder_sha256"} {
		if !strings.Contains(sql, required) {
			t.Fatal("operational correction lost a custody guard", required)
		}
	}
	for _, required := range []string{"baseline.round_id IS NULL", "NEW.receipt_json->>'source_lock_sha256'", "NEW.receipt_json->>'providers_sha256'", "NEW.receipt_json->>'evaluator_image_digest'", "NEW.created_at<=q.deadline_at", "clock_timestamp()<=deadline_at", "archive_manifest_sha256"} {
		if !strings.Contains(sql, required) {
			t.Fatal("replacement host bypasses published no-op evidence requirements", required)
		}
	}
	count := 0
	for _, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == sql {
			count++
		}
	}
	if CompetitionExecutorControlAmendmentMigrationVersion() == 0 || count != 1 {
		t.Fatal("operator amendment must have one registered migration identity")
	}
	t.Logf("executor control amendment migration_version=%d", CompetitionExecutorControlAmendmentMigrationVersion())
}
