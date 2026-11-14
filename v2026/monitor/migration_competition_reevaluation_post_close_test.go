// Post-close admission replaces one predicate, not the rest of the recovery
// schema. Independent catalog faults prove the successor retains that custody.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A successor must retire exactly its predecessor and carry every unchanged
// function, immutable journal and generation guard into its new artifact.
func TestMigrationsCompetitionPostCloseRecoveryRetainsInheritedCustody(t *testing.T) {
	version := server.CompetitionReevaluationPostCloseMigrationVersion()
	if version <= server.CompetitionFinalizationHoldMigrationVersion() ||
		competitionJobReevaluationMigrationContract.artifact.removedVersion != version {
		t.Fatal("post-close recovery did not append after or exactly retire its predecessor")
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	for _, required := range []string{"competition_job_reevaluation_post_close_allows", "competition_job_reevaluation_insert_guard",
		"competition_job_reevaluation_commit_guard", "competition_job_immutable_guard", "competition_recovery_slot_guard",
		"competition_job_reevaluation_append_only", "competition_job_reevaluation_atomic", "competition_recovery_slot_authority"} {
		if !strings.Contains(contract.query, required) {
			t.Fatalf("successor lost inherited %s custody", required)
		}
	}
}

// Actual catalog reads must detect a changed window, old admission body and
// unrelated inherited guard failures, rather than merely a new schema head.
func TestMigrationsCompetitionPostCloseRecoveryNativeFaultCustody(t *testing.T) {
	version := server.CompetitionReevaluationPostCloseMigrationVersion()
	distributed := server.CompetitionDistributedReevaluationMigrationVersion()
	oldInsert, _, _, _ := server.CompetitionJobReevaluationBodiesSql()
	faults := []struct{ name, sql string }{
		{name: "window-bypass", sql: `CREATE OR REPLACE FUNCTION competition_job_reevaluation_post_close_allows(request jsonb,closes timestamp,granted timestamp) RETURNS boolean LANGUAGE plpgsql STABLE AS 'BEGIN RETURN true; END'`},
		{name: "restore-open-only-guard", sql: "CREATE OR REPLACE FUNCTION competition_job_reevaluation_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $old_guard$" + oldInsert + "$old_guard$"},
		{name: "privileged-window", sql: `ALTER FUNCTION competition_job_reevaluation_post_close_allows(jsonb,timestamp,timestamp) SECURITY DEFINER`},
		{name: "lost-immutable-audit", sql: `ALTER TABLE competition_job_reevaluation DISABLE TRIGGER competition_job_reevaluation_append_only`},
		{name: "lost-atomic-commit", sql: `ALTER TABLE competition_job_reevaluation DISABLE TRIGGER competition_job_reevaluation_atomic`},
		{name: "lost-slot-authority", sql: `ALTER TABLE competition_worker_slot DISABLE TRIGGER competition_recovery_slot_authority`},
	}
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		heads := []int{distributed - 1, distributed}
		if head := server.MigrationCount(); head > distributed {
			heads = append(heads, head)
		}
		for _, head := range heads {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			contract := migrationDistributedRecoveryTestOwner(t, version, head)
			owner := fmt.Sprintf("%s@v%d", contract.name, contract.requiredVersion)
			var actual int
			server.Db(t.Context(), func(conn server.PgConn) {
				server.Raise(conn.QueryRow(t.Context(), `SELECT COALESCE(MAX(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&actual))
			})
			if actual != head {
				t.Fatalf("post-close monitor fixture schema=%d want=%d", actual, head)
			}
			t.Logf("post_close_fixture_schema_head=%d", actual)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatal("post-close migration schema drift", drift)
			}
			for _, fault := range faults {
				snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
					if _, err := tx.Exec(ctx, fault.sql); err != nil {
						t.Fatalf("apply %s: %v", fault.name, err)
					}
					source := &snMainnetMigrationTxSource{tx: tx}
					alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
					if err != nil {
						t.Fatalf("inspect %s: %v", fault.name, err)
					}
					alert := requireAlertClass(t, alerts, "migration-schema-drift")
					if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, owner) {
						t.Fatalf("%s lost successor ownership: %s", fault.name, alert.Observed)
					}
				})
			}
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatal("rolled-back recovery fault left schema drift", drift)
			}
		}
	})
}
