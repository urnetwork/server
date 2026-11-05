// Migration-first compatibility and prospective authority must remain visible
// in the immutable SQL, independently of runtime configuration or test hosts.
package server

import (
	"strings"
	"testing"
)

// Future host slots do not reinterpret or relax the deployed singleton schema.
func TestCompetitionDistributedMigrationIsProspectiveAndPreservesSingleton(t *testing.T) {
	for _, forbidden := range []string{
		"DROP TABLE", "DROP TRIGGER", "DROP CONSTRAINT", "DELETE FROM competition_job", "UPDATE competition_round SET",
		"ALTER TABLE competition_worker_slot", "CREATE OR REPLACE FUNCTION competition_recovery_slot_guard",
		"CREATE OR REPLACE FUNCTION competition_job_immutable_guard", "ALTER ROLE",
	} {
		if strings.Contains(competitionDistributedEvaluationMigrationSql, forbidden) {
			t.Fatal("distributed migration changes legacy execution custody", forbidden)
		}
	}
	for _, required := range []string{
		"CREATE TABLE competition_execution_slot", "CREATE TABLE competition_executor_equivalence", "CREATE TABLE competition_executor_qualification_run",
		"ADD COLUMN execution_deadline_at timestamp NULL", "ADD COLUMN executor_fencing_token bigint NULL",
		"NEW.epoch_number<2", "'from_epoch')::integer IS DISTINCT FROM 2", "'minimum-work/1'", "IS DISTINCT FROM 10800",
		"'operational-admission/v1'", "IS DISTINCT FROM 0.05::numeric", "IS DISTINCT FROM 1.96::numeric",
		"competition_executor_legacy_slot_exclusion", "deadline_at=started_at+interval '3 hours'",
	} {
		if !strings.Contains(competitionDistributedEvaluationMigrationSql, required) {
			t.Fatal("distributed migration lost a prospective safety boundary", required)
		}
	}
	registered := 0
	for _, migration := range migrations {
		if sqlMigration, ok := migration.(*SqlMigration); ok && sqlMigration.sql == competitionDistributedEvaluationMigrationSql {
			registered++
		}
	}
	if registered != 1 || CompetitionDistributedEvaluationMigrationVersion() == 0 {
		t.Fatal("distributed migration must appear exactly once in the append-only catalog")
	}
}

// An unpublished executor append must follow, not displace, the complete
// published statistics/finalization prefix from the current control release.
func TestCompetitionDistributedMigrationFollowsPublishedFinalizationPrefix(t *testing.T) {
	statistics := TransferContractUnresolvedPredicateStatisticsVersion()
	finalization := CompetitionFinalizationHoldMigrationVersion()
	distributed := CompetitionDistributedEvaluationMigrationVersion()
	if statistics != 926 || finalization != 927 || distributed != finalization+1 || distributed != len(migrations) {
		t.Fatalf("executor append reordered published authority: statistics=%d finalization=%d distributed=%d head=%d", statistics, finalization, distributed, len(migrations))
	}
	for _, published := range []struct {
		version int
		sql     string
	}{
		{version: statistics, sql: TransferContractUnresolvedPredicateStatisticsSql},
		{version: finalization, sql: competitionFinalizationHoldMigrationSql},
	} {
		migration, ok := migrations[published.version-1].(*SqlMigration)
		if !ok || migration.sql != published.sql {
			t.Fatal("published migration no longer owns its original position", published.version)
		}
	}
}

// The narrow publication-only alternative cannot erase the running release's
// real baseline-source, cancellation or legacy-ranking protections.
func TestCompetitionDistributedBaselineGuardKeepsLegacyChecks(t *testing.T) {
	_, _, _, _, baseline := CompetitionDistributedEvaluationBodiesSql()
	for _, required := range []string{
		"competition round baseline cannot be attached after finalization or cancellation",
		"competition round baseline cannot replace legacy ranking policy",
		"state = 'running' AND attempt_count >= NEW.source_attempt",
		"competition round baseline source does not belong to its round attempt",
		"epoch_round.epoch_number>=2", "j.state='queued' AND j.attempt_count=0 AND j.started_at IS NULL",
		"'round_baseline_preparation_reserved'", "'urnetwork.competition_executor_baseline_manifest'",
		"'urnetwork.competition_executor_baseline_owner'", "'urnetwork.competition_executor_baseline_context'",
		"clock_timestamp()<s.lease_expires_at", "competition_supervisor_lease",
	} {
		if !strings.Contains(baseline, required) {
			t.Fatal("baseline successor lost legacy custody or its narrow future alternative", required)
		}
	}
}

// Every mutation checks explicit host/slot/fence and retained artifact custody;
// repeatable-read legacy snapshots are invalidated by an actual shared write.
func TestCompetitionDistributedGuardsFenceOwnershipAndDeadline(t *testing.T) {
	authority, job, slot, equivalence, _ := CompetitionDistributedEvaluationBodiesSql()
	for _, pair := range []struct{ body, required string }{
		{body: authority, required: "urnetwork.competition_executor_fence"},
		{body: job, required: "OLD.execution_deadline_at IS DISTINCT FROM NEW.execution_deadline_at"},
		{body: job, required: "distributed reconciliation cannot create another execution attempt"},
		{body: job, required: "urnetwork.competition_executor_artifacts_verified"},
		{body: job, required: "clock_timestamp()>=NEW.execution_deadline_at"},
		{body: slot, required: "NEW.fencing_token<OLD.fencing_token"},
		{body: slot, required: "UPDATE competition_worker_slot SET heartbeat_at=clock_timestamp()"},
		{body: slot, required: "job.executor_fencing_token IS DISTINCT FROM NEW.fencing_token"},
		{body: equivalence, required: "qualification_job_id"},
		{body: equivalence, required: "qualification_deadline_at"},
		{body: equivalence, required: "NEW.created_at<=q.deadline_at"},
	} {
		if !strings.Contains(pair.body, pair.required) {
			t.Fatal("executor SQL lost an independent authority boundary", pair.required)
		}
	}
}
