// Adds a prospectively approved reviewer without relabeling historical
// policies, reviews, source intents or their immutable digests.
package server

import "strings"

// Replace only the provider-aware guards and the latest scheduled round guard.
// The separate source graph/authorization migrations remain installed unchanged.
func competitionWinnerReviewProviderMigration() string {
	extract := func(sql, declaration, terminator string) string {
		start := strings.Index(sql, declaration)
		if start < 0 {
			panic("winner provider migration function is absent")
		}
		end := strings.Index(sql[start:], terminator)
		if end < 0 {
			panic("winner provider migration function is unterminated")
		}
		return strings.Replace(sql[start:start+end+len(terminator)], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
	}
	replace := func(sql, old, replacement string, count int) string {
		if strings.Count(sql, old) != count {
			panic("winner provider migration authority anchor is ambiguous")
		}
		return strings.ReplaceAll(sql, old, replacement)
	}
	source := extract(competitionWinnerReviewGuards, "CREATE FUNCTION competition_v2_winner_source_authorized", "END $winner_source$;")
	review := extract(competitionWinnerReviewGuards, "CREATE FUNCTION competition_v2_winner_review_guard", "END $winner_review$;")
	policyCheck := "policy->>'winner_review_policy' IS DISTINCT FROM 'elected_winner_sol61max_v1'"
	supportedPolicies := "policy->>'winner_review_policy' NOT IN ('elected_winner_sol61max_v1','elected_winner_gpt54mini_xhigh_v1')"
	source = replace(source, policyCheck, supportedPolicies, 1)
	review = replace(review, policyCheck, supportedPolicies, 1)
	source = replace(source,
		"review.review_json->'request'->>'reviewer_model' IS DISTINCT FROM 'gpt-6.1-sol'\n  OR review.review_json->'request'->>'reviewer_reasoning' IS DISTINCT FROM 'max'",
		"NOT competition_v2_winner_review_provider(policy->>'winner_review_policy',review.review_json->'request'->>'reviewer_model',review.review_json->'request'->>'reviewer_reasoning')", 1)
	review = replace(review,
		"request->>'reviewer_model' IS DISTINCT FROM 'gpt-6.1-sol' OR request->>'reviewer_reasoning' IS DISTINCT FROM 'max'",
		"NOT competition_v2_winner_review_provider(policy->>'winner_review_policy',request->>'reviewer_model',request->>'reviewer_reasoning')", 2)
	round := replace(competitionScheduledOpeningMigration(),
		"intent->>'winner_review_policy'='elected_winner_sol61max_v1'",
		"intent->>'winner_review_policy' IN ('elected_winner_sol61max_v1','elected_winner_gpt54mini_xhigh_v1')", 1)
	return competitionWinnerReviewProviderSql + source + "\n" + review + "\n" + round
}

const competitionWinnerReviewProviderSql = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION competition_v2_winner_review_provider(policy_name text, model_name text, reasoning_name text)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $provider$
 SELECT COALESCE((policy_name,model_name,reasoning_name) IN (
  ('elected_winner_sol61max_v1','gpt-6.1-sol','max'),
  ('elected_winner_gpt54mini_xhigh_v1','gpt-5.4-mini','xhigh')
 ),false);
$provider$;
`
