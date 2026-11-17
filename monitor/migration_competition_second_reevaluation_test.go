// Catalog ownership follows only this reviewed append and retains all old
// source, admission, first-receipt and independent closure predicates.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// An unknown retirement must fail rather than becoming an implicit waiver.
func migrationSecondRecoveryTestOwner(t testing.TB) migrationArtifact {
	t.Helper()
	version := server.CompetitionSecondRecoveryMigrationVersion()
	previous := server.CompetitionOperatorRequestedPatchRetryMigrationVersion()
	current := migrationCurrentSuffixTestContract(t, version).artifact
	if version <= previous || migrationCurrentSuffixTestContract(t, previous).artifact.removedVersion != version ||
		current.name != "exact-job second recovery with immutable prior receipt" || current.removedVersion != server.CompetitionOfficialReviewEpochTwoMigrationVersion() {
		t.Fatal("second recovery catalog owner has an unreviewed boundary")
	}
	return current
}

// Every new object and the untouched first audit remain under exact custody.
func TestMigrationsSecondRecoveryRetainsClosedCatalogCustody(t *testing.T) {
	owner := migrationSecondRecoveryTestOwner(t)
	query := migrationCurrentSuffixTestContract(t, owner.requiredVersion).query
	for _, name := range []string{"competition_second_recovery_job_allowed", "competition_job_second_reevaluation",
		"competition_second_recovery_transition_allows", "competition_second_recovery_insert_validate", "competition_second_recovery_audit_validate",
		"competition_second_recovery_commit_guard", "second_recovery_receipt_json", "competition_job_second_reevaluation_append_only",
		"competition_job_reevaluation_append_only", "competition_job_reevaluation_atomic", "competition_distributed_recovery_commit_guard",
		"competition_patch_retry_audit_validate", "competition_job_immutable_guard", "competition_executor_job_guard", "competition_v2_intake_execution_guard",
		"competition_round_baseline_insert_guard"} {
		if !strings.Contains(query, name) {
			t.Fatal("second recovery catalog omitted inherited or new authority", name)
		}
	}
}

// Both normal migration boundaries are healthy; every injected fault is
// isolated and rolled back, never applied to a shared or production schema.
func TestMigrationsSecondRecoveryPgDetectsScopeCacheAuditAndInheritedDrift(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionSecondRecoveryMigrationVersion()
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			if _, drift := migrationPingDatabaseCheck(tb, tb.Context()); drift != "" {
				tb.Fatal("healthy second recovery boundary reports drift", head, drift)
			}
		}
		for _, fault := range []string{
			`CREATE OR REPLACE FUNCTION competition_second_recovery_job_allowed(target uuid) RETURNS boolean LANGUAGE sql IMMUTABLE AS 'SELECT true'`,
			`CREATE OR REPLACE FUNCTION competition_second_recovery_audit_validate(next_audit competition_job_second_reevaluation) RETURNS competition_job_second_reevaluation LANGUAGE plpgsql AS 'BEGIN RETURN next_audit; END'`,
			`ALTER TABLE competition_job DROP CONSTRAINT competition_job_cycle_shape`,
			`ALTER TABLE competition_job_second_reevaluation DISABLE TRIGGER competition_job_second_reevaluation_atomic`,
			`ALTER TABLE competition_job_reevaluation DISABLE TRIGGER competition_job_reevaluation_append_only`,
			`CREATE OR REPLACE FUNCTION competition_distributed_recovery_transition_allows(previous_job competition_job,next_job competition_job) RETURNS boolean LANGUAGE plpgsql AS 'BEGIN RETURN true; END'`,
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
				if !strings.Contains(alert.Markdown(), "exact-job second recovery with immutable prior receipt") {
					tb.Fatal("second recovery drift lost its exact current owner")
				}
			})
		}
	})
}
