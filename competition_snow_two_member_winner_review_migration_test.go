// The new policy has its own finding census; the published three-member SQL
// remains byte-identical and continues to validate immutable historical rounds.
package server

import (
	"strings"
	"testing"
)

// Append-only generation preserves v1 and changes only explicitly scoped
// provider/policy guards, never historical rows, source graphs or triggers.
func TestCompetitionSnowTwoMemberMigrationPreservesHistoricalAuthority(t *testing.T) {
	historical := competitionSnowWinnerReviewMigration()
	sql := competitionSnowTwoMemberWinnerReviewMigration()
	if historical != competitionSnowWinnerReviewMigration() || !strings.Contains(sql, competitionSnowReviewFindingsSql) {
		t.Fatal("two-member migration changed the original Snow finding predicate")
	}
	for _, required := range []string{
		"('elected_winner_snow_union_v1','snow-qwen-gemma-muse-union','provider-default')",
		"('elected_winner_snow_union_v2','snow-qwen-gemma-union','provider-default')",
		"competition_v2_snow_review_findings_valid_v2(request jsonb)",
		"jsonb_array_length(request->'findings')<>12",
		"jsonb_array_length(request->'findings')<>18",
		"WHEN 'elected_winner_snow_union_v1' THEN competition_v2_snow_review_findings_valid(request)",
		"WHEN 'elected_winner_snow_union_v2' THEN competition_v2_snow_review_findings_valid_v2(request)",
		"competition_v2_snow_review_findings_valid_for_policy(review.review_json->'request',policy->>'winner_review_policy')",
		"THEN NOT competition_v2_snow_review_findings_valid_for_policy(request,round_policy->'automation_policy'->>'winner_review_policy')",
		"checkpoint IS DISTINCT FROM actual_checkpoint",
		"round and activated source closing policy disagree",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("two-member migration omitted %q", required)
		}
	}
	for _, forbidden := range []string{"CREATE TRIGGER", "FUNCTION competition_v2_source_authorized(", "FUNCTION competition_v2_source_graph_valid(", "UPDATE competition_", "DELETE FROM competition_"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("two-member migration rewrote unrelated authority: %s", forbidden)
		}
	}
}

// PostgreSQL executes both real predicates and the policy/provider dispatcher;
// neither policy accepts a missing, extra, duplicate or wrong model's finding.
func TestCompetitionSnowTwoMemberDatabasePreservesBothExactCensuses(t *testing.T) {
	testCompetitionWinnerReviewDatabaseGuards(t,
		competitionWinnerReviewMigration()+competitionWinnerReviewProviderMigration()+competitionSnowWinnerReviewMigration()+competitionSnowTwoMemberWinnerReviewMigration(),
		snowTwoMemberReviewSqlAssertions)
}

const snowTwoMemberReviewSqlAssertions = `
DO $test_snow_two_member$
DECLARE findings jsonb; request jsonb; altered jsonb; policy_name text; aggregate_model text; members text[]; expected_count integer;
BEGIN
 FOR policy_name,aggregate_model,members,expected_count IN
  SELECT 'elected_winner_snow_union_v1','snow-qwen-gemma-muse-union',ARRAY['qwen3.8-27b','gemma-4-26b-a4b','muse-glimmer-30b'],18
  UNION ALL SELECT 'elected_winner_snow_union_v2','snow-qwen-gemma-union',ARRAY['qwen3.8-27b','gemma-4-26b-a4b'],12
 LOOP
  IF NOT competition_v2_winner_review_provider(policy_name,aggregate_model,'provider-default') THEN RAISE EXCEPTION 'exact Snow version tuple refused'; END IF;
  SELECT jsonb_agg(jsonb_build_object('model',member,'criterion',criterion,'disposition','pass','reason_code','APPROVED','explanation','synthetic complete criterion','evidence_sha256',repeat('a',64)))
   INTO findings FROM unnest(members) member CROSS JOIN unnest(ARRAY['measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety']) criterion;
  request:=jsonb_build_object('findings',findings,'reason_codes',jsonb_build_array('APPROVED'),'reward_disposition','approved','source_disposition','approved');
  IF jsonb_array_length(findings)<>expected_count OR NOT competition_v2_snow_review_findings_valid_for_policy(request,policy_name) THEN RAISE EXCEPTION 'complete exact Snow census refused'; END IF;
  altered:=jsonb_set(request,'{findings}',findings-0);
  IF competition_v2_snow_review_findings_valid_for_policy(altered,policy_name) THEN RAISE EXCEPTION 'missing finding accepted'; END IF;
  altered:=jsonb_set(request,'{findings}',findings||jsonb_build_array(findings->0));
  IF competition_v2_snow_review_findings_valid_for_policy(altered,policy_name) THEN RAISE EXCEPTION 'extra finding accepted'; END IF;
  altered:=jsonb_set(request,'{findings,0,model}','"synthetic-unapproved-member"'::jsonb);
  IF competition_v2_snow_review_findings_valid_for_policy(altered,policy_name) THEN RAISE EXCEPTION 'wrong member accepted'; END IF;
  altered:=jsonb_set(request,'{findings,1}',findings->0);
  IF competition_v2_snow_review_findings_valid_for_policy(altered,policy_name) THEN RAISE EXCEPTION 'duplicate model criterion accepted'; END IF;
  altered:=jsonb_set(request,'{findings,0,disposition}','"uncertain"'::jsonb);
  IF competition_v2_snow_review_findings_valid_for_policy(altered,policy_name) THEN RAISE EXCEPTION 'uncertainty approved source'; END IF;
  IF competition_v2_snow_review_findings_valid_for_policy(request,CASE policy_name WHEN 'elected_winner_snow_union_v1' THEN 'elected_winner_snow_union_v2' ELSE 'elected_winner_snow_union_v1' END) THEN RAISE EXCEPTION 'cross-version census accepted'; END IF;
 END LOOP;
 IF competition_v2_winner_review_provider('elected_winner_snow_union_v2','snow-qwen-gemma-muse-union','provider-default')
  OR competition_v2_winner_review_provider('elected_winner_snow_union_v1','snow-qwen-gemma-union','provider-default')
  OR competition_v2_snow_review_findings_valid_for_policy(request,'synthetic-unknown-policy') THEN RAISE EXCEPTION 'cross-version or unknown policy accepted'; END IF;
END $test_snow_two_member$;
`
