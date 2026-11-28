package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

// Take over the complete previous custody; only the epoch-two findings
// function has a different published body.
var competitionOfficialReviewEpochTwoAstra62MigrationContract = func() snMainnetMigrationContract {
	query := competitionOfficialReviewEpochTwoMigrationContract.query
	old := snMainnetMigrationLiteral(server.CompetitionOfficialReviewEpochTwoBodiesSql().FindingsValid)
	next := snMainnetMigrationLiteral(server.CompetitionOfficialReviewEpochTwoAstra62FindingsBodySql())
	if strings.Count(query, old) != 1 {
		panic("epoch-two Astra 6.2 catalog predecessor absent or ambiguous")
	}
	return migrationCurrentSuffix(server.CompetitionOfficialReviewEpochTwoAstra62MigrationVersion(),
		"epoch-two Astra 6.2 reviewer provenance with retained complete custody", strings.Replace(query, old, next, 1))
}()
