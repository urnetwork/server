// Independent stop-proof custody is additive: dropping its guard or changing
// its predicate must not hide behind the unchanged historical recovery audit.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The new artifact does not retire a predecessor or replace an old function.
func TestMigrationsCompetitionOperatorInterruptionRetainsIndependentCustody(t *testing.T) {
	version := server.CompetitionOperatorInterruptionMigrationVersion()
	if version <= server.CompetitionReevaluationPostCloseMigrationVersion() || competitionReevaluationPostCloseMigrationContract.artifact.removedVersion != 0 {
		t.Fatal("additive operator proof retired historical recovery custody")
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	for _, name := range []string{"competition_job_operator_interruption_valid", "competition_job_operator_interruption_guard", "competition_job_operator_interruption"} {
		if !strings.Contains(contract.query, name) {
			t.Fatalf("operator proof artifact omitted %s", name)
		}
	}
}

// Real catalog faults roll back independently; the new owning version must
// page on each fault and leave the old exact post-close artifact intact.
func TestMigrationsCompetitionOperatorInterruptionNativeFaultCustody(t *testing.T) {
	version := server.CompetitionOperatorInterruptionMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
	faults := []struct{ name, sql string }{
		{name: "predicate-bypass", sql: `CREATE OR REPLACE FUNCTION competition_job_operator_interruption_valid(target_job uuid,target_round uuid,terminal jsonb,request jsonb) RETURNS boolean LANGUAGE plpgsql STABLE AS 'BEGIN RETURN true; END'`},
		{name: "privileged-predicate", sql: `ALTER FUNCTION competition_job_operator_interruption_valid(uuid,uuid,jsonb,jsonb) SECURITY DEFINER`},
		{name: "trigger-bypass", sql: `ALTER TABLE competition_job_reevaluation DISABLE TRIGGER competition_job_operator_interruption`},
		{name: "missing-trigger", sql: `DROP TRIGGER competition_job_operator_interruption ON competition_job_reevaluation`},
	}
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatal("operator proof baseline catalog drift", drift)
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				alert := requireAlertClass(t, alerts, "migration-schema-drift")
				if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, owner) {
					t.Fatalf("%s lost operator proof ownership: %s", fault.name, alert.Observed)
				}
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatal("operator proof fault rollback left catalog drift", drift)
		}
	})
}
