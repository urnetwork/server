// The operator-stop exception adds its own catalog custody without retiring
// the predecessor's immutable journal, approval-window or generation guards.
package monitor

import "github.com/urnetwork/server"

// Exact body, invoker rights and enabled trigger remain independently owned.
var competitionOperatorInterruptionMigrationContract = func() snMainnetMigrationContract {
	valid, guard := server.CompetitionOperatorInterruptionBodiesSql()
	return migrationCurrentSuffix(server.CompetitionOperatorInterruptionMigrationVersion(), "scoped stopped-execution recovery evidence",
		snMainnetMigrationFunction("competition_job_operator_interruption_valid(uuid,uuid,jsonb,jsonb)", "boolean", "plpgsql", "s", false, valid, "target_job", "target_round", "terminal", "request"),
		snMainnetMigrationFunction("competition_job_operator_interruption_guard()", "trigger", "plpgsql", "v", false, guard),
		snMainnetMigrationTrigger("competition_job_reevaluation", "competition_job_operator_interruption", "competition_job_operator_interruption_guard", 7))
}()
