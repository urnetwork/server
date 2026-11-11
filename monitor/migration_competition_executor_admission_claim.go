// The claim correction carries all prior admission custody forward and replaces
// only the exact execution guard body changed by its migration.
package monitor

import (
	"github.com/urnetwork/server"
	"strings"
)

var competitionExecutorAdmissionClaimMigrationContract = func() snMainnetMigrationContract {
	_, previous, _, _, _ := server.CompetitionDistributedEvaluationBodiesSql()
	old := snMainnetMigrationLiteral(previous)
	inherited := competitionExecutorAdmissionExceptionMigrationContract.query
	if strings.Count(inherited, old) != 1 {
		panic("admitted claim predecessor custody is absent or ambiguous")
	}
	inherited = strings.Replace(inherited, old, snMainnetMigrationLiteral(server.CompetitionExecutorAdmissionClaimGuardBodySql()), 1)
	return migrationCurrentSuffix(server.CompetitionExecutorAdmissionClaimMigrationVersion(), "executor claims honor admitted evidence with retained admission custody", inherited)
}()
