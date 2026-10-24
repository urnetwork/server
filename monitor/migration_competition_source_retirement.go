package monitor

import "github.com/urnetwork/server"

// v858 and v859 replace functions that v855 pinned by exact body, so each
// republishes v855's guards with its own bodies and retires its predecessor.
//
// v858 retired operator-proxy from production's source requirement: the source
// graph and source authorization count the same ten git refs in both modes
// (server.competitionSourceRetirementMigration).
var competitionMigration858ArtifactQuery = func() string {
	sourceGraphValid, sourceAuthorized := server.CompetitionSourceRetirementBodiesSql()
	_, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard := server.CompetitionWinnerReviewBodiesSql()
	return competitionV2SourceGuardsArtifactQuery(sourceGraphValid, sourceAuthorized, roundInsertGuard,
		winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard)
}()

// v859 binds a scheduled round's opening to the time its intent announced
// (server.competitionScheduledOpeningMigration); it re-creates the winner
// guards from v855's text unchanged.
var competitionMigration859ArtifactQuery = func() string {
	sourceGraphValid, sourceAuthorized := server.CompetitionSourceRetirementBodiesSql()
	_, _, winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard := server.CompetitionWinnerReviewBodiesSql()
	return competitionV2SourceGuardsArtifactQuery(sourceGraphValid, sourceAuthorized, server.CompetitionScheduledOpeningRoundGuardBodySql(),
		winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard)
}()

// The v855 guard set with the given bodies (competitionMigration855ArtifactQuery
// keeps its published text byte for byte).
func competitionV2SourceGuardsArtifactQuery(sourceGraphValid, sourceAuthorized, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard string) string {
	return "(" +
		snMainnetMigrationFunction("competition_v2_source_graph_valid(jsonb,boolean)", "boolean", "plpgsql", "i", false, sourceGraphValid, "commits", "staging_mode") + " AND " +
		snMainnetMigrationFunction("competition_v2_source_mode(jsonb)", "void", "plpgsql", "v", false, competitionMigration850CompetitionV2SourceModeBody, "intent") + " AND " +
		snMainnetMigrationFunction("competition_v2_source_authorized(jsonb,text,text,text)", "void", "plpgsql", "v", false, sourceAuthorized, "intent", "env", "competition", "season") + " AND " +
		snMainnetMigrationFunction("competition_v2_round_insert_guard()", "trigger", "plpgsql", "v", false, roundInsertGuard) + " AND " +
		snMainnetMigrationFunction("competition_v2_winner_source_authorized(jsonb,uuid)", "boolean", "plpgsql", "v", false, winnerSourceAuthorized, "intent", "predecessor_id") + " AND " +
		snMainnetMigrationFunction("competition_v2_winner_review_guard()", "trigger", "plpgsql", "v", false, winnerReviewGuard) + " AND " +
		snMainnetMigrationTrigger("competition_integrity_review", "competition_v2_winner_review_before_insert", "competition_v2_winner_review_guard", 7) + " AND " +
		snMainnetMigrationFunction("competition_v2_winner_checkpoint_guard()", "trigger", "plpgsql", "v", false, winnerCheckpointGuard) + " AND " +
		snMainnetMigrationTrigger("competition_supervisor_transition", "competition_v2_winner_checkpoint_before_insert", "competition_v2_winner_checkpoint_guard", 7) + ")"
}
