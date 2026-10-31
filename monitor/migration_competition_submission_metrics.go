// The operator's completed-score walk follows its exact same-round index.
package monitor

import "github.com/urnetwork/server"

// The partial index covers scored success and failure, never unfinished jobs.
// Complete definition and validity/readiness remain independent requirements.
var competitionSubmissionMetricsMigrationContract = migrationCurrentSuffix(server.CompetitionSubmissionMetricsMigrationVersion(), "competition completed-score metrics index",
	migrationFp2Index("competition_job", "competition_job_completed_score_metrics_idx", "round_id, job_id",
		"(((state)::text = ANY ((ARRAY['succeeded'::character varying, 'failed'::character varying])::text[])) AND (score_json IS NOT NULL))", false))
