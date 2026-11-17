// The distributed recovery successor inherits every source/admission and
// historical recovery catalog check, changing only three exact guard bodies.
package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

// An additive path does not retire unrelated old guards. The new artifact
// carries both complete predecessors, with their exact changed bodies pinned.
var competitionDistributedReevaluationMigrationContract = func() snMainnetMigrationContract {
	transition, audit, commit, immutable, executor, intake := server.CompetitionDistributedReevaluationBodiesSql()
	_, _, oldImmutable, _ := server.CompetitionJobReevaluationBodiesSql()
	oldExecutor := server.CompetitionExecutorAdmissionClaimGuardBodySql()
	oldIntake := ""
	for _, function := range server.CompetitionIntakeFirstFunctionsSql() {
		if function.Signature == "competition_v2_intake_execution_guard()" {
			oldIntake = function.Body
		}
	}
	replace := func(query, old, next string) string {
		old = snMainnetMigrationLiteral(old)
		if strings.Count(query, old) != 1 {
			panic("distributed recovery catalog predecessor changed")
		}
		return strings.Replace(query, old, snMainnetMigrationLiteral(next), 1)
	}
	legacy := replace(competitionReevaluationPostCloseMigrationContract.query, oldImmutable, immutable)
	distributed := replace(competitionExecutorAdmissionClaimMigrationContract.query, oldExecutor, executor)
	distributed = replace(distributed, oldIntake, intake)
	return migrationCurrentSuffix(server.CompetitionDistributedReevaluationMigrationVersion(), "separately authorized distributed infrastructure recovery", legacy, distributed,
		snMainnetMigrationFunction("competition_distributed_recovery_transition_allows(competition_job,competition_job)", "boolean", "plpgsql", "v", false, transition, "previous_job", "next_job"),
		snMainnetMigrationFunction("competition_distributed_recovery_audit_guard()", "trigger", "plpgsql", "v", false, audit),
		snMainnetMigrationFunction("competition_distributed_recovery_commit_guard()", "trigger", "plpgsql", "v", false, commit),
		snMainnetMigrationTrigger("competition_job_reevaluation", "competition_job_distributed_recovery_audit", "competition_distributed_recovery_audit_guard", 7),
		migrationCompetitionDeferredTrigger("competition_job_reevaluation", "competition_job_distributed_recovery_atomic", "competition_distributed_recovery_commit_guard", 5))
}()
