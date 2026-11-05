// A successor owns the changed admission predicate and every unchanged
// recovery guard; replacing one function never retires the remaining custody.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// Reuse the predecessor's complete artifact, replacing only its operative
// insert-function body and appending the close-bound helper's exact contract.
var competitionReevaluationPostCloseMigrationContract = func() snMainnetMigrationContract {
	oldInsert, _, _, _ := server.CompetitionJobReevaluationBodiesSql()
	window, insertGuard := server.CompetitionReevaluationPostCloseBodiesSql()
	oldQuery := snMainnetMigrationFunction("competition_job_reevaluation_insert_guard()", "trigger", "plpgsql", "v", false, oldInsert)
	newQuery := snMainnetMigrationFunction("competition_job_reevaluation_insert_guard()", "trigger", "plpgsql", "v", false, insertGuard)
	query := competitionJobReevaluationMigrationContract.query
	if strings.Count(query, oldQuery) != 1 {
		panic("post-close recovery catalog predecessor changed")
	}
	query = strings.Replace(query, oldQuery, newQuery, 1)
	return migrationCurrentSuffix(server.CompetitionReevaluationPostCloseMigrationVersion(), "finite held post-close competition recovery",
		query, snMainnetMigrationFunction("competition_job_reevaluation_post_close_allows(jsonb,timestamp without time zone,timestamp without time zone)",
			"boolean", "plpgsql", "s", false, window, "request", "closes", "granted"))
}()
