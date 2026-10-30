// Additive provider guards retain original review authority and the scheduled
// opening boundary. Optional SQL verification uses only an isolated fixture.
package server

import (
	"strings"
	"testing"
)

// No historical function text, source graph or trigger is rewritten in place.
func TestCompetitionWinnerProviderMigrationPreservesAuthority(t *testing.T) {
	historical := competitionWinnerReviewMigration()
	sql := competitionWinnerReviewProviderMigration()
	for _, required := range []string{
		"('elected_winner_sol61max_v1','gpt-6.1-sol','max')",
		"('elected_winner_gpt54mini_xhigh_v1','gpt-5.4-mini','xhigh')",
		"COALESCE((policy_name,model_name,reasoning_name)",
		"CREATE OR REPLACE FUNCTION competition_v2_winner_source_authorized",
		"CREATE OR REPLACE FUNCTION competition_v2_winner_review_guard",
		"checkpoint IS DISTINCT FROM actual_checkpoint",
		"review.created_at>=(checkpoint->>'started_at')::timestamp",
		"NEW.created_at<(checkpoint->>'deadline')::timestamp",
		"round and activated source closing policy disagree",
		"NEW.opens_at IS DISTINCT FROM (intent->>'opens_at')::timestamp",
		"NEW.closes_at-NEW.opens_at<>make_interval(secs=>window_seconds)",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("provider migration lost %q", required)
		}
	}
	if strings.Contains(sql, "CREATE TRIGGER") || strings.Contains(sql, "FUNCTION competition_v2_source_authorized(") || strings.Contains(sql, "FUNCTION competition_v2_source_graph_valid(") {
		t.Fatal("provider migration replaced unrelated source guards or existing triggers")
	}
	if historical != competitionWinnerReviewMigration() || strings.Contains(historical, "gpt-5.4-mini") {
		t.Fatal("historical winner-review migration was relabeled")
	}
}

// Both original and new policies pass the complete source/winner checks in
// one disposable database; cross-policy and missing provenance fail closed.
func TestCompetitionWinnerProviderDatabaseGuards(t *testing.T) {
	miniAssertions := strings.NewReplacer("elected_winner_sol61max_v1", "elected_winner_gpt54mini_xhigh_v1", "gpt-6.1-sol", "gpt-5.4-mini").Replace(winnerReviewSqlAssertions)
	assertions := "BEGIN;\n" + winnerReviewSqlAssertions + "\nROLLBACK;\n" + winnerProviderSqlAssertions + miniAssertions
	testCompetitionWinnerReviewDatabaseGuards(t, competitionWinnerReviewMigration()+competitionWinnerReviewProviderMigration(), assertions)
}

const winnerProviderSqlAssertions = `
DO $test_provider$
BEGIN
 IF NOT competition_v2_winner_review_provider('elected_winner_sol61max_v1','gpt-6.1-sol','max')
  OR NOT competition_v2_winner_review_provider('elected_winner_gpt54mini_xhigh_v1','gpt-5.4-mini','xhigh')
  OR competition_v2_winner_review_provider('elected_winner_sol61max_v1','gpt-5.4-mini','xhigh')
  OR competition_v2_winner_review_provider('elected_winner_gpt54mini_xhigh_v1','gpt-6.1-sol','max')
  OR competition_v2_winner_review_provider('elected_winner_gpt54mini_xhigh_v1','gpt-5.4-mini','max')
  OR competition_v2_winner_review_provider('elected_winner_gpt54mini_xhigh_v1','gpt-5.4','xhigh')
  OR competition_v2_winner_review_provider(NULL,'gpt-5.4-mini','xhigh')
  OR competition_v2_winner_review_provider('unknown','gpt-5.4-mini','xhigh') THEN
  RAISE EXCEPTION 'review provider provenance was missing or cross-policy';
 END IF;
END $test_provider$;
UPDATE competition_round SET policy_json=jsonb_set(policy_json,'{automation_policy,winner_review_policy}',to_jsonb('elected_winner_gpt54mini_xhigh_v1'::text));
UPDATE competition_integrity_review SET review_json=jsonb_set(jsonb_set(review_json,'{request,reviewer_model}',to_jsonb('gpt-5.4-mini'::text)),'{request,reviewer_reasoning}',to_jsonb('xhigh'::text));
`
