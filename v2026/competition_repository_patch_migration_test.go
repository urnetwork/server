// Migration shape checks guard the exact frozen-policy admission boundary.
package server

import (
	"strings"
	"testing"
)

// Replacing source authority must retain all unrelated genuine-review guards.
func TestCompetitionRepositoryPatchMigrationPreservesSourceAuthority(t *testing.T) {
	sql := competitionRepositoryPatchMigration()
	for _, required := range []string{
		"CREATE OR REPLACE FUNCTION competition_v2_source_authorized",
		"competition_v2_winner_source_authorized(intent,predecessor_id)",
		"evaluated_round.policy_json->'patch_policy'->>'path_base'",
		"<>'mono_root_v1' AND ((intent->'commits')-'server') IS DISTINCT FROM ((previous->'commits')-'server')",
		"v2 promotion requires genuine exact-source approval ready by the deadline",
		"intent->>'threshold_evidence_sha256' IS DISTINCT FROM review.review_json->'request'->>'score_sha256'",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("source authorization lost guard %q", required)
		}
	}
	if strings.Contains(sql, "CREATE OR REPLACE FUNCTION competition_v2_winner_review_guard") || strings.Contains(sql, "UPDATE competition_") || strings.Contains(sql, "DELETE FROM competition_") {
		t.Fatal("repository migration rewrites review authority or historical evidence")
	}
	if sql != competitionRepositoryPatchMigration() {
		t.Fatal("migration is not deterministic")
	}
}
