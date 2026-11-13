// Version 855 binds automatic closing reviews and the successor source to the
// elected winner.
package monitor

import "github.com/urnetwork/server/v2026"

// 855 replaces the 850 source authorization and 851 round insert bodies, so it
// republishes them with the unchanged 850 graph helpers and adds its own
// functions and insert triggers.
var competitionMigration855ArtifactQuery = func() string {
	sourceAuthorized, roundInsertGuard, winnerSourceAuthorized, winnerReviewGuard, winnerCheckpointGuard := server.CompetitionWinnerReviewBodiesSql()
	return "(" +
		snMainnetMigrationFunction("competition_v2_source_graph_valid(jsonb,boolean)", "boolean", "plpgsql", "i", false, competitionMigration850CompetitionV2SourceGraphValidBody, "commits", "staging_mode") + " AND " +
		snMainnetMigrationFunction("competition_v2_source_mode(jsonb)", "void", "plpgsql", "v", false, competitionMigration850CompetitionV2SourceModeBody, "intent") + " AND " +
		snMainnetMigrationFunction("competition_v2_source_authorized(jsonb,text,text,text)", "void", "plpgsql", "v", false, sourceAuthorized, "intent", "env", "competition", "season") + " AND " +
		snMainnetMigrationFunction("competition_v2_round_insert_guard()", "trigger", "plpgsql", "v", false, roundInsertGuard) + " AND " +
		snMainnetMigrationFunction("competition_v2_winner_source_authorized(jsonb,uuid)", "boolean", "plpgsql", "v", false, winnerSourceAuthorized, "intent", "predecessor_id") + " AND " +
		snMainnetMigrationFunction("competition_v2_winner_review_guard()", "trigger", "plpgsql", "v", false, winnerReviewGuard) + " AND " +
		snMainnetMigrationTrigger("competition_integrity_review", "competition_v2_winner_review_before_insert", "competition_v2_winner_review_guard", 7) + " AND " +
		snMainnetMigrationFunction("competition_v2_winner_checkpoint_guard()", "trigger", "plpgsql", "v", false, winnerCheckpointGuard) + " AND " +
		snMainnetMigrationTrigger("competition_supervisor_transition", "competition_v2_winner_checkpoint_before_insert", "competition_v2_winner_checkpoint_guard", 7) + ")"
}()
