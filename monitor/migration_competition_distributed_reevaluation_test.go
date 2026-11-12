// Current catalog custody must preserve all old guards while detecting loss
// of any new distributed closure, expiry or atomic publication predicate.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

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
