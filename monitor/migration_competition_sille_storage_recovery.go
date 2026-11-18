package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

var competitionSilleStorageRecoveryMigrationContract = func() snMainnetMigrationContract {
	query := competitionOfficialReviewEpochTwoAstra62MigrationContract.query
	old := snMainnetMigrationLiteral(server.CompetitionOperatorRequestedPatchRetryBodiesSql().Audit)
	next := snMainnetMigrationLiteral(server.CompetitionSilleStorageRecoveryAuditBodySql())
	if strings.Count(query, old) != 1 {
		panic("Sille storage recovery catalog predecessor absent or ambiguous")
	}
	return migrationCurrentSuffix(server.CompetitionSilleStorageRecoveryMigrationVersion(),
		"exact Sille storage interruption recovery with retained catalog custody", strings.Replace(query, old, next, 1))
}()
