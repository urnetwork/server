// Adds the owner-approved two-member Snow policy without changing any earlier
// migration, persisted round, provider tuple or three-member finding predicate.
package server

import "strings"

// Only provider-aware guards and the scheduled round guard gain v2. Historical
// v1 still selects its original eighteen-finding function; v2 selects twelve.
func competitionSnowTwoMemberWinnerReviewMigration() string {
	replace := func(sql, anchor, replacement string, count int) string {
		if strings.Count(sql, anchor) != count {
			panic("two-member Snow migration authority anchor is ambiguous")
		}
		return strings.ReplaceAll(sql, anchor, replacement)
	}
	currentFindings := replace(competitionSnowReviewFindingsSql,
		"competition_v2_snow_review_findings_valid(request jsonb)",
		"competition_v2_snow_review_findings_valid_v2(request jsonb)", 1)
	currentFindings = replace(currentFindings, "<>18", "<>12", 2)
	currentFindings = replace(currentFindings,
		"'qwen3.8-27b','gemma-4-26b-a4b','muse-glimmer-30b'",
		"'qwen3.8-27b','gemma-4-26b-a4b'", 1)
	guards := competitionSnowWinnerReviewMigration()
	guards = replace(guards,
		"('elected_winner_snow_union_v1','snow-qwen-gemma-muse-union','provider-default')",
		"('elected_winner_snow_union_v1','snow-qwen-gemma-muse-union','provider-default'),\n  ('elected_winner_snow_union_v2','snow-qwen-gemma-union','provider-default')", 1)
	guards = replace(guards,
		"'elected_winner_gpt54mini_xhigh_v1','elected_winner_snow_union_v1')",
		"'elected_winner_gpt54mini_xhigh_v1','elected_winner_snow_union_v1','elected_winner_snow_union_v2')", 3)
	guards = replace(guards,
		"policy->>'winner_review_policy'='elected_winner_snow_union_v1' AND NOT competition_v2_snow_review_findings_valid(review.review_json->'request')",
		"policy->>'winner_review_policy' IN ('elected_winner_snow_union_v1','elected_winner_snow_union_v2') AND NOT competition_v2_snow_review_findings_valid_for_policy(review.review_json->'request',policy->>'winner_review_policy')", 1)
	guards = replace(guards,
		"policy->>'winner_review_policy'='elected_winner_snow_union_v1' AND NOT competition_v2_snow_review_findings_valid(request)",
		"policy->>'winner_review_policy' IN ('elected_winner_snow_union_v1','elected_winner_snow_union_v2') AND NOT competition_v2_snow_review_findings_valid_for_policy(request,policy->>'winner_review_policy')", 1)
	guards = replace(guards,
		"round_policy->'automation_policy'->>'winner_review_policy'='elected_winner_snow_union_v1'\n   THEN NOT competition_v2_snow_review_findings_valid(request)",
		"round_policy->'automation_policy'->>'winner_review_policy' IN ('elected_winner_snow_union_v1','elected_winner_snow_union_v2')\n   THEN NOT competition_v2_snow_review_findings_valid_for_policy(request,round_policy->'automation_policy'->>'winner_review_policy')", 1)
	return currentFindings + competitionSnowReviewFindingsPolicySql + guards
}

// An unknown or cross-version selector cannot turn a partial union into valid
// authority. Existing v1 callers retain the unchanged one-argument function.
const competitionSnowReviewFindingsPolicySql = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid_for_policy(request jsonb, policy_name text)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $snow_policy_findings$
 SELECT CASE policy_name
  WHEN 'elected_winner_snow_union_v1' THEN competition_v2_snow_review_findings_valid(request)
  WHEN 'elected_winner_snow_union_v2' THEN competition_v2_snow_review_findings_valid_v2(request)
  ELSE false END;
$snow_policy_findings$;
`
