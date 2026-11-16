// The exact new capability inherits every prior guard and separately owns its
// validators. Native faults occur only in rolled-back disposable transactions.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A specific, reviewed successor is required; no generic retirement walker
// can silently accept future catalog custody or waive inherited predicates.
func migrationPatchRetryTestOwner(t testing.TB) migrationArtifact {
	t.Helper()
	version := server.CompetitionOperatorRequestedPatchRetryMigrationVersion()
	previous := server.CompetitionDistributedReevaluationMigrationVersion()
	current := migrationCurrentSuffixTestContract(t, version).artifact
	if version <= previous || migrationCurrentSuffixTestContract(t, previous).artifact.removedVersion != version ||
		current.name != "separately authorized unchanged-patch retry" || current.removedVersion != 0 {
		t.Fatal("patch retry catalog owner has an unreviewed boundary")
	}
	return current
}

// Exact old infrastructure transition/commit guards, old recovery journals and
// source/admission custody remain required at the new published head.
func TestMigrationsOperatorPatchRetryRetainsAllInheritedAndNewCustody(t *testing.T) {
	owner := migrationPatchRetryTestOwner(t)
	query := migrationCurrentSuffixTestContract(t, owner.requiredVersion).query
	for _, name := range []string{"competition_patch_retry_transition_allows", "competition_patch_retry_insert_validate", "competition_patch_retry_audit_validate",
		"competition_distributed_recovery_transition_allows", "competition_distributed_recovery_commit_guard", "competition_job_reevaluation_insert_guard",
		"competition_job_immutable_guard", "competition_executor_job_guard", "competition_v2_intake_execution_guard", "competition_job_reevaluation_atomic",
		"competition_job_reevaluation_append_only", "competition_round_baseline_insert_guard"} {
		if !strings.Contains(query, name) {
			t.Fatal("patch retry catalog custody omitted guard", name)
		}
	}
}

// Both actual schema boundaries are healthy. Changing a new or retained old
// guard produces a current-owner alert rather than a future/legacy waiver.
func TestMigrationsOperatorPatchRetryPgDetectsNewAndInheritedGuardDrift(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionOperatorRequestedPatchRetryMigrationVersion()
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			if _, drift := migrationPingDatabaseCheck(tb, tb.Context()); drift != "" {
				tb.Fatal("healthy patch retry migration boundary drifted", head, drift)
			}
		}
		for _, fault := range []string{
			`CREATE OR REPLACE FUNCTION competition_patch_retry_transition_allows(previous_job competition_job,next_job competition_job) RETURNS boolean LANGUAGE plpgsql AS 'BEGIN RETURN true; END'`,
			`CREATE OR REPLACE FUNCTION competition_patch_retry_audit_validate(next_audit competition_job_reevaluation) RETURNS competition_job_reevaluation LANGUAGE plpgsql AS 'BEGIN RETURN next_audit; END'`,
			`CREATE OR REPLACE FUNCTION competition_distributed_recovery_transition_allows(previous_job competition_job,next_job competition_job) RETURNS boolean LANGUAGE plpgsql AS 'BEGIN RETURN true; END'`,
			`ALTER TABLE competition_job_reevaluation DISABLE TRIGGER competition_job_distributed_recovery_atomic`,
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
				if !strings.Contains(alert.Markdown(), "separately authorized unchanged-patch retry") {
					tb.Fatal("guard drift lost its exact new catalog owner")
				}
			})
		}
	})
}
