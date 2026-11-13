// Current catalog custody must preserve all old guards while detecting loss
// of any new distributed closure, expiry or atomic publication predicate.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Only these two reviewed predecessors transfer to this exact distributed
// successor. An unexpected later retirement remains an oracle failure.
func migrationDistributedRecoveryTestOwner(t testing.TB, priorVersion, head int) migrationArtifact {
	t.Helper()
	version := server.CompetitionDistributedReevaluationMigrationVersion()
	legacy, claim := server.CompetitionReevaluationPostCloseMigrationVersion(), server.CompetitionExecutorAdmissionClaimMigrationVersion()
	if priorVersion != legacy && priorVersion != claim {
		t.Fatal("distributed owner oracle received an unreviewed predecessor", priorVersion)
	}
	for _, previous := range []int{legacy, claim} {
		if version <= previous || migrationCurrentSuffixTestContract(t, previous).artifact.removedVersion != version {
			t.Fatal("distributed owner oracle lost its exact predecessor transfer", previous)
		}
	}
	current := migrationCurrentSuffixTestContract(t, version).artifact
	if current.name != "separately authorized distributed infrastructure recovery" || current.removedVersion != 0 {
		t.Fatal("distributed owner oracle has an unreviewed successor")
	}
	if head >= version {
		return current
	}
	return migrationCurrentSuffixTestContract(t, priorVersion).artifact
}

// Both historical owners stay required up to the actual append, including at
// the immediately preceding head; only that append transfers their custody.
func TestMigrationsDistributedRecoveryOwnerOracleClosedBoundaries(t *testing.T) {
	version := server.CompetitionDistributedReevaluationMigrationVersion()
	for _, previous := range []int{server.CompetitionReevaluationPostCloseMigrationVersion(), server.CompetitionExecutorAdmissionClaimMigrationVersion()} {
		for _, head := range []int{previous, version - 1, version, version + 1, server.MigrationCount()} {
			want := previous
			if head >= version {
				want = version
			}
			if owner := migrationDistributedRecoveryTestOwner(t, previous, head); owner.requiredVersion != want {
				t.Fatal("explicit distributed owner boundary moved", previous, head, owner.requiredVersion, want)
			}
		}
	}
}

// Both independently owned predecessor artifacts end exactly at this successor;
// unrelated source/baseline/qualification checks remain carried by its query.
func TestMigrationsDistributedRecoveryRetainsExactCustody(t *testing.T) {
	version := server.CompetitionDistributedReevaluationMigrationVersion()
	current := migrationCurrentSuffixTestContract(t, version)
	for _, priorVersion := range []int{server.CompetitionReevaluationPostCloseMigrationVersion(), server.CompetitionExecutorAdmissionClaimMigrationVersion()} {
		previous := migrationCurrentSuffixTestContract(t, priorVersion)
		if previous.artifact.removedVersion != version {
			t.Fatal("distributed recovery did not inherit its exact predecessor boundary")
		}
	}
	for _, required := range []string{"competition_job_reevaluation_insert_guard", "competition_job_immutable_guard", "competition_executor_job_guard", "competition_v2_intake_execution_guard", "competition_round_baseline_insert_guard", "competition_distributed_recovery_audit_guard", "competition_distributed_recovery_commit_guard", "competition_job_reevaluation_atomic"} {
		if !strings.Contains(current.query, required) {
			t.Fatal("new custody omitted inherited/new authority", required)
		}
	}
	if current.artifact.removedVersion != 0 {
		t.Fatal("new current distributed recovery custody was unexpectedly retired")
	}
}

// The monitor observes both sides of the real migration boundary, then faults
// exact new/old function bodies only inside rolled-back disposable transactions.
func TestMigrationsDistributedRecoveryPgDetectsLostClosureAndOldGuards(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionDistributedReevaluationMigrationVersion()
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			if _, drift := migrationPingDatabaseCheck(tb, tb.Context()); drift != "" {
				tb.Fatal("healthy distributed recovery boundary reports drift", head, drift)
			}
		}
		_, _, oldImmutable, _ := server.CompetitionJobReevaluationBodiesSql()
		for _, fault := range []string{
			"CREATE OR REPLACE FUNCTION competition_distributed_recovery_transition_allows(previous_job competition_job,next_job competition_job) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN RETURN true; END $$",
			"CREATE OR REPLACE FUNCTION competition_job_immutable_guard() RETURNS trigger LANGUAGE plpgsql AS $synthetic_old$" + oldImmutable + "$synthetic_old$",
			"ALTER TABLE competition_job_reevaluation DISABLE TRIGGER competition_job_distributed_recovery_atomic",
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
				if !strings.Contains(alert.Markdown(), "separately authorized distributed infrastructure recovery") {
					tb.Fatal("new recovery authority drift lost its exact catalog owner")
				}
			})
		}
	})
}
