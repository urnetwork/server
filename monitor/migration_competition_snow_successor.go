// Snow replaces only its provider/review/identity bodies. The full retained
// mono-root source guard and older unaffected custody remain mandatory.
package monitor

import (
	"strings"

	"github.com/urnetwork/server"
)

// Current complete guards, including the unchanged repository authorization.
// The old v844 identity body retires alone; all its other predicates stay live.
var competitionMigration893ArtifactQuery = func() string {
	sourceGraphValid, _ := server.CompetitionSourceRetirementBodiesSql()
	_, _, _, _, winnerCheckpointGuard := server.CompetitionWinnerReviewBodiesSql()
	provider, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, identityGuard, findings := server.CompetitionSnowWinnerReviewBodiesSql()
	providerQuery := snMainnetMigrationFunction("competition_v2_winner_review_provider(text,text,text)", "boolean", "sql", "i", false, provider, "policy_name", "model_name", "reasoning_name")
	providerQuery = strings.Replace(providerQuery, "p.proparallel='u'", "p.proparallel='s'", 1)
	findingsQuery := snMainnetMigrationFunction("competition_v2_snow_review_findings_valid(jsonb)", "boolean", "plpgsql", "i", false, findings, "request")
	findingsQuery = strings.Replace(findingsQuery, "p.proparallel='u'", "p.proparallel='s'", 1)
	return "(" + providerQuery + " AND " + findingsQuery + " AND " +
		competitionV2SourceGuardsArtifactQuery(sourceGraphValid, server.CompetitionRepositoryPatchSourceBodySql(),
			roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard) + " AND " +
		snMainnetMigrationFunction("competition_v2_review_identity_guard()", "trigger", "plpgsql", "v", false, identityGuard) + " AND " +
		snMainnetMigrationTrigger("competition_integrity_review", "competition_v2_review_identity", "competition_v2_review_identity_guard", 7) + ")"
}()
