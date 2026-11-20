// Corrected claim custody must detect the original failure and preserve all
// inherited schema checks on both sides of the published migration boundary.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsExecutorAdmissionClaimRetainsExactCustody(t *testing.T) {
	version := server.CompetitionExecutorAdmissionClaimMigrationVersion()
	previous := migrationCurrentSuffixTestContract(t, server.CompetitionExecutorAdmissionExceptionMigrationVersion())
	current := migrationCurrentSuffixTestContract(t, version)
	if previous.artifact.removedVersion != version || current.artifact.removedVersion != server.CompetitionDistributedReevaluationMigrationVersion() {
		t.Fatal("admitted claim custody does not transfer at its actual migration")
	}
	_, old, _, _, _ := server.CompetitionDistributedEvaluationBodiesSql()
	expected := strings.Replace(previous.query, snMainnetMigrationLiteral(old), snMainnetMigrationLiteral(server.CompetitionExecutorAdmissionClaimGuardBodySql()), 1)
	if current.query != expected || strings.Count(previous.query, snMainnetMigrationLiteral(old)) != 1 {
		t.Fatal("claim successor lost or changed unrelated admission custody")
	}
}

func TestMigrationsExecutorAdmissionClaimPgDetectsOldPredicateAndLostFences(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionExecutorAdmissionClaimMigrationVersion()
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			if _, drift := migrationPingDatabaseCheck(tb, tb.Context()); drift != "" {
				tb.Fatalf("healthy claim boundary %d reports drift: %s", head, drift)
			}
		}
		_, previous, _, _, _ := server.CompetitionDistributedEvaluationBodiesSql()
		for _, fault := range []string{
			"CREATE OR REPLACE FUNCTION competition_executor_job_guard() RETURNS trigger LANGUAGE plpgsql AS $synthetic_original$" + previous + "$synthetic_original$",
			"CREATE OR REPLACE FUNCTION competition_executor_job_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$",
			"ALTER FUNCTION competition_append_only_guard() RENAME TO synthetic_missing_append_only_guard",
		} {
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault); err != nil {
					tb.Fatal(err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					tb.Fatal(err)
				}
				alert := requireAlertClass(tb, alerts, "migration-schema-drift")
				if !strings.Contains(alert.Markdown(), fmt.Sprintf("executor claims honor admitted evidence with retained admission custody@v%d", version)) {
					tb.Fatal("claim or inherited custody fault lost its current owner")
				}
			})
		}
	})
}
