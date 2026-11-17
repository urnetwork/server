// The appended amendment preserves historical bodies and uses actual immutable
// evidence plus the existing checkpoint, not a provider or deadline blanket waiver.
package server

import (
	"strings"
	"testing"
)

// Rendering the real migration also checks every exact predecessor anchor.
func TestCompetitionEpochTwoOfficialMigrationRetainsNarrowAuthority(t *testing.T) {
	sql := competitionOfficialReviewEpochTwoMigration()
	for _, required := range []string{
		"r.epoch_number IN (1,2) AND NOT r.staging", "CASE WHEN r.epoch_number=1 THEN competition_official_review_findings_valid(request)",
		"a.import_json->'review'=request", "competition_official_review_import_sha256", "competition_review_handoff_clock",
		"r.epoch_number=2 OR (request->'official_review_override'->>'completed_at')::timestamp>=(s.seal_json->>'publication_at')::timestamp",
		"native_qualification_waived' IS DISTINCT FROM 'false'::jsonb", "reviewer_model' IS DISTINCT FROM 'gpt-6-astra'",
		"reviewer_reasoning' IS DISTINCT FROM 'max'", "jsonb_array_length(request->'findings')<>6", "supported_failure",
		"competition_v2_production_benefit_review_valid", "entry", "checkpoint IS DISTINCT FROM actual_checkpoint",
		"imported_at>=(checkpoint->>'started_at')::timestamp AND imported_at<(checkpoint->>'deadline')::timestamp",
		"(e->>'completed_at')::timestamp<=(request->'official_review_override'->>'completed_at')::timestamp",
		"b.created_at<=(request->'official_review_override'->>'completed_at')::timestamp", "expected_decision_sha256",
		"competition_official_review_findings_valid(review.review_json->'request') AND competition_official_review_request_authorized",
	} {
		if !strings.Contains(sql, required) {
			t.Fatal("missing exact authority boundary", required)
		}
	}
	for _, forbidden := range []string{"UPDATE competition_", "DELETE FROM competition_", "ALTER TABLE", "DROP ", "CREATE OR REPLACE FUNCTION competition_review_handoff", "CREATE OR REPLACE FUNCTION competition_v2_source_authorized", "CREATE OR REPLACE FUNCTION competition_official_review_findings_valid", "CREATE OR REPLACE FUNCTION competition_v2_winner_checkpoint_guard", "CREATE OR REPLACE FUNCTION competition_v2_score_eligible"} {
		if strings.Contains(sql, forbidden) {
			t.Fatal("reviewer amendment changed unrelated or historical authority", forbidden)
		}
	}
	version := CompetitionOfficialReviewEpochTwoMigrationVersion()
	if version <= CompetitionSecondRecoveryMigrationVersion() || version != len(migrations) {
		t.Fatal("reviewer amendment must follow every currently published migration", version)
	}
	if CompetitionOfficialReviewBodiesSql().FindingsValid == "" || strings.Contains(competitionOfficialReviewMigration(), "official_two") {
		t.Fatal("frozen epoch-one migration changed")
	}
}
