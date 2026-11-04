// The numeric migration head cannot substitute for host/fence/deadline schema
// custody. All faults below use one disposable transaction and roll back.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Both published prefixes must be healthy before inspecting individual faults.
func TestMigrationsCompetitionDistributedPublishedPrefixesAndGuardFaults(t *testing.T) {
	version := server.CompetitionDistributedEvaluationMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
	faults := []struct{ name, sql string }{
		{name: "host fence guard removed", sql: `ALTER TABLE competition_execution_slot DISABLE TRIGGER competition_executor_slot_authority`},
		{name: "legacy exclusion guard removed", sql: `ALTER TABLE competition_worker_slot DISABLE TRIGGER competition_executor_legacy_slot_exclusion`},
		{name: "retained outcome guard removed", sql: `ALTER TABLE competition_job DISABLE TRIGGER competition_executor_job_authority`},
		{name: "qualification clock made mutable", sql: `ALTER TABLE competition_executor_qualification_run DISABLE TRIGGER competition_executor_qualification_run_append_only`},
		{name: "equivalence evidence made mutable", sql: `ALTER TABLE competition_executor_equivalence DISABLE TRIGGER competition_executor_equivalence_append_only`},
		{name: "job deadline column dropped", sql: `ALTER TABLE competition_job DROP COLUMN execution_deadline_at CASCADE`},
		{name: "baseline source guard weakened", sql: `CREATE OR REPLACE FUNCTION competition_round_baseline_insert_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
	}
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			row, drift := migrationPingDatabaseCheck(tb, tb.Context())
			want := "f"
			if head == version {
				want = "t"
			}
			if drift != "" || row[contract.artifact.rowColumn] != want {
				tb.Fatalf("distributed prefix %d=%q want=%q drift=%s", head, row[contract.artifact.rowColumn], want, drift)
			}
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					tb.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					tb.Fatalf("inspect %s: %v", fault.name, err)
				}
				alert := requireAlertClass(t, alerts, "migration-schema-drift")
				if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, owner) {
					tb.Fatalf("%s lost distributed migration custody", fault.name)
				}
			})
		}
	})
}
