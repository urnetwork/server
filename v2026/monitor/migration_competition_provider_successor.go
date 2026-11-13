// Published provider and repository successors retain the complete source
// and winner guard set while retiring the exact predecessor body contracts.
package monitor

import (
	"strings"

	"github.com/urnetwork/server/v2026"
)

// The provider helper is immutable and parallel-safe; every other function
// retains the exact full-body and metadata comparison of its predecessor.
func competitionProviderSuccessorArtifactQuery(sourceAuthorized string) string {
	sourceGraphValid, _ := server.CompetitionSourceRetirementBodiesSql()
	_, _, _, _, winnerCheckpointGuard := server.CompetitionWinnerReviewBodiesSql()
	provider, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard := server.CompetitionWinnerReviewProviderBodiesSql()
	providerQuery := snMainnetMigrationFunction("competition_v2_winner_review_provider(text,text,text)", "boolean", "sql", "i", false, provider, "policy_name", "model_name", "reasoning_name")
	providerQuery = strings.Replace(providerQuery, "p.proparallel='u'", "p.proparallel='s'", 1)
	return "(" + providerQuery + " AND " + competitionV2SourceGuardsArtifactQuery(sourceGraphValid, sourceAuthorized,
		roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard) + ")"
}

// At 891 only the provider, winner review and scheduled round bodies change.
var competitionMigration891ArtifactQuery = func() string {
	_, sourceAuthorized := server.CompetitionSourceRetirementBodiesSql()
	return competitionProviderSuccessorArtifactQuery(sourceAuthorized)
}()

// At 892 the source guard additionally admits only explicitly reviewed
// repository patches; all the provider and unchanged guards stay required.
var competitionMigration892ArtifactQuery = competitionProviderSuccessorArtifactQuery(server.CompetitionRepositoryPatchSourceBodySql())
