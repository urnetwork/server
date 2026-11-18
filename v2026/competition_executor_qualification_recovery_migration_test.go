// The replacement extends only qualification clocks, never scoring authority.
package server

import (
	"strings"
	"testing"
)

func TestExecutorQualificationRecoveryMigrationPreservesOriginalAuthority(t *testing.T) {
	sql := CompetitionExecutorQualificationRecoveryMigrationSql()
	for _, forbidden := range []string{"DROP TRIGGER", "DISABLE TRIGGER", "UPDATE competition_round", "UPDATE competition_job", "UPDATE competition_executor_qualification_run", "DELETE FROM", "ALTER TABLE competition_job"} {
		if strings.Contains(sql, forbidden) {
			t.Fatal("replacement rewrites original authority", forbidden)
		}
	}
	for _, required := range []string{"PRIMARY KEY(round_id,host_id)", "qualification_recovery_append_only", "qualification_recovery_no_truncate", "authorization_canonical", "encode(sha256(", "statement_timestamp() AT TIME ZONE 'UTC'", "interval '3 hours'", "expired original immutable clock", "current_setting('urnetwork.competition_executor_qualification_recovery'", "clock_timestamp()<lease_expires_at", "competition_executor_control_host", "original_score_sha256", "evidence_sha256", "NOT EXISTS(SELECT 1 FROM competition_executor_qualification_recovery", "NEW.created_at<=q.deadline_at", "clock_timestamp()<=deadline_at", "recovery_sha256", "archive_manifest_sha256", "providers_sha256", "config_local_sha256", "vault_local_sha256"} {
		if !strings.Contains(sql, required) {
			t.Fatal("replacement lost a frozen or recovery predicate", required)
		}
	}
	if CompetitionExecutorQualificationRecoveryMigrationVersion() == 0 {
		t.Fatal("replacement migration is not registered")
	}
	t.Logf("qualification recovery migration_version=%d", CompetitionExecutorQualificationRecoveryMigrationVersion())
}
