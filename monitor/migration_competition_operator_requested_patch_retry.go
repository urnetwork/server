// The patch-retry successor carries complete distributed and historical
// custody while owning every new validator and exact changed wrapper body.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// The predecessor's infrastructure transition and deferred all-host commit
// guards remain unchanged and are still checked by the inherited query.
var competitionOperatorRequestedPatchRetryMigrationContract = func() snMainnetMigrationContract {
	bodies := server.CompetitionOperatorRequestedPatchRetryBodiesSql()
	_, oldAudit, _, oldImmutable, oldExecutor, oldIntake := server.CompetitionDistributedReevaluationBodiesSql()
	_, oldInsert := server.CompetitionReevaluationPostCloseBodiesSql()
	query := competitionDistributedReevaluationMigrationContract.query
	for _, replacement := range []struct{ previous, next string }{
		{previous: oldInsert, next: bodies.Insert},
		{previous: oldAudit, next: bodies.Audit},
		{previous: oldImmutable, next: bodies.Immutable},
		{previous: oldExecutor, next: bodies.Executor},
		{previous: oldIntake, next: bodies.Intake},
	} {
		previous := snMainnetMigrationLiteral(replacement.previous)
		if strings.Count(query, previous) != 1 {
			panic("patch retry catalog predecessor changed")
		}
		query = strings.Replace(query, previous, snMainnetMigrationLiteral(replacement.next), 1)
	}
	return migrationCurrentSuffix(server.CompetitionOperatorRequestedPatchRetryMigrationVersion(), "separately authorized unchanged-patch retry", query,
		snMainnetMigrationFunction("competition_patch_retry_transition_allows(competition_job,competition_job)", "boolean", "plpgsql", "v", false, bodies.Transition, "previous_job", "next_job"),
		snMainnetMigrationFunction("competition_patch_retry_insert_validate(competition_job_reevaluation)", "competition_job_reevaluation", "plpgsql", "v", false, bodies.InsertValidation, "next_audit"),
		snMainnetMigrationFunction("competition_patch_retry_audit_validate(competition_job_reevaluation)", "competition_job_reevaluation", "plpgsql", "v", false, bodies.AuditValidation, "next_audit"))
}()
