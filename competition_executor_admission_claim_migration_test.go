// The runtime trigger must compose approved evidence without losing fences.
package server

import (
	"strings"
	"testing"
)

func TestExecutorAdmissionClaimMigrationChangesOnlyAdmissionPredicate(t *testing.T) {
	_, original, _, _, _ := CompetitionDistributedEvaluationBodiesSql()
	corrected := CompetitionExecutorAdmissionClaimGuardBodySql()
	const marker = "  RAISE EXCEPTION 'distributed work requires its shared baseline and every qualified no-op executor';"
	start := strings.Index(original, " IF NEW.state='running' AND (NOT EXISTS(SELECT 1 FROM competition_round_baseline")
	end := strings.Index(original, marker)
	if start < 0 || end <= start {
		t.Fatal("published admission boundary absent")
	}
	expected := original[:start] + " IF NEW.state='running' AND NOT competition_executor_control_hosts_ready(NEW.round_id) THEN\n" + original[end:]
	if corrected != expected {
		t.Fatal("claim correction changed more than the obsolete whole-host predicate")
	}
	sql := CompetitionExecutorAdmissionClaimMigrationSql()
	for _, forbidden := range []string{"DROP ", "DISABLE TRIGGER", "UPDATE competition_", "DELETE FROM", "ALTER TABLE", "INSERT INTO"} {
		if strings.Contains(sql, forbidden) {
			t.Fatal("claim correction mutates frozen evidence or disables custody", forbidden)
		}
	}
	if CompetitionExecutorAdmissionClaimMigrationVersion() == 0 {
		t.Fatal("claim correction is not registered")
	}
	t.Logf("admitted executor claim migration_version=%d", CompetitionExecutorAdmissionClaimMigrationVersion())
}
