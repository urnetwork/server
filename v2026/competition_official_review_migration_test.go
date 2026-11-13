package server

import (
	"strings"
	"testing"
)

// Render the real appended DDL; frozen migration generators remain unchanged.
func TestCompetitionOfficialReviewMigrationKeepsIndependentAuthority(t *testing.T) {
	sql := competitionOfficialReviewMigration()
	for _, required := range []string{"CREATE TABLE competition_official_review_authorization", "r.epoch_number=1 AND NOT r.staging", "competition_official_review_import_sha256", "a.import_json->'review'=request", "checkpoint IS DISTINCT FROM actual_checkpoint", "competition_v2_snow_review_findings_valid_for_policy", "competition_v2_winner_review_provider", "reviewer_model'='gpt-6-astra'", "unaccepted_calibration_acknowledged", "competition_v2_production_benefit_review_valid", "competition_append_only_guard"} {
		if !strings.Contains(sql, required) {
			t.Fatal("missing authority boundary", required)
		}
	}
	for _, forbidden := range []string{"UPDATE competition_", "DELETE FROM competition_", "DROP TABLE", "ALTER TABLE", "CREATE OR REPLACE FUNCTION competition_review_handoff_observed", "CREATE OR REPLACE FUNCTION competition_v2_source_authorized"} {
		if strings.Contains(sql, forbidden) {
			t.Fatal("amendment changed historical or unrelated authority", forbidden)
		}
	}
	count := 0
	for index, migration := range migrations {
		if item, ok := migration.(*SqlMigration); ok && item.sql == sql {
			count++
			t.Logf("official review migration version: %d", index+1)
		}
	}
	if count != 1 {
		t.Fatal("official review migration must be appended exactly once", count)
	}
}
