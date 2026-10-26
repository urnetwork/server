// Prospective Snow SQL preserves historical authority and tests its real
// eighteen-finding predicate only inside a fresh networkless fixture database.
package server

import (
	"strings"
	"testing"
)

// An IF condition's unparenthesized CASE exposes its inner THEN to PL/pgSQL's
// expression terminator. Keep it grouped; the database test compiles this body.
func TestCompetitionSnowReviewMigrationGroupsConditionalCase(t *testing.T) {
	sql := competitionSnowWinnerReviewMigration()
	if !strings.Contains(sql, "OR (CASE WHEN round_policy->'automation_policy'") ||
		!strings.Contains(sql, "COALESCE(f->>'model','')<>'') END)") ||
		strings.Contains(sql, "OR CASE WHEN round_policy->'automation_policy'") {
		t.Fatal("Snow identity IF condition contains an ungrouped CASE expression")
	}
}

// Scope changes replace guards, never historical rows, the source graph or triggers.
func TestCompetitionSnowReviewMigrationPreservesHistoricalAndSourceGuards(t *testing.T) {
	historical := competitionWinnerReviewProviderMigration()
	sql := competitionSnowWinnerReviewMigration()
	for _, required := range []string{
		"('elected_winner_sol61max_v1','gpt-6.1-sol','max')",
		"('elected_winner_gpt54mini_xhigh_v1','gpt-5.4-mini','xhigh')",
		"('elected_winner_snow_union_v1','snow-qwen-gemma-muse-union','provider-default')",
		"CREATE OR REPLACE FUNCTION competition_v2_review_identity_guard",
		"THEN NOT competition_v2_snow_review_findings_valid(request)",
		"ELSE jsonb_array_length(request->'findings')<>6",
		"jsonb_array_length(request->'findings')<>18",
		"count(DISTINCT (f->>'model',f->>'criterion'))",
		"'qwen3.8-27b','gemma-4-26b-a4b','muse-glimmer-30b'",
		"competition_v2_snow_review_findings_valid(review.review_json->'request')",
		"checkpoint IS DISTINCT FROM actual_checkpoint",
		"round and activated source closing policy disagree",
		"NEW.opens_at IS DISTINCT FROM (intent->>'opens_at')::timestamp",
		"NEW.created_at<(checkpoint->>'deadline')::timestamp",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("Snow SQL omitted %q", required)
		}
	}
	if sql != competitionSnowWinnerReviewMigration() || historical != competitionWinnerReviewProviderMigration() || strings.Contains(historical, "snow_union") {
		t.Fatal("Snow migration was nondeterministic or changed a published migration")
	}
	for _, forbidden := range []string{"CREATE TRIGGER", "FUNCTION competition_v2_source_authorized(", "FUNCTION competition_v2_source_graph_valid(", "UPDATE competition_", "DELETE FROM competition_"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("Snow migration rewrote unrelated authority: %s", forbidden)
		}
	}
}

// Both policy/provider cross-products and malformed model-attributed findings
// are rejected by PostgreSQL, not a string assertion or reimplemented parser.
func TestCompetitionSnowReviewDatabaseProviderAndFindingGuards(t *testing.T) {
	testCompetitionWinnerReviewDatabaseGuards(t,
		competitionWinnerReviewMigration()+competitionWinnerReviewProviderMigration()+competitionSnowWinnerReviewMigration(),
		snowReviewSqlAssertions)
}

const snowReviewSqlAssertions = `
DO $test_snow$
DECLARE findings jsonb; request jsonb; altered jsonb; provider text;
BEGIN
 IF NOT competition_v2_winner_review_provider('elected_winner_snow_union_v1','snow-qwen-gemma-muse-union','provider-default')
  OR NOT competition_v2_winner_review_provider('elected_winner_sol61max_v1','gpt-6.1-sol','max')
  OR NOT competition_v2_winner_review_provider('elected_winner_gpt54mini_xhigh_v1','gpt-5.4-mini','xhigh')
  OR competition_v2_winner_review_provider('elected_winner_snow_union_v1','gpt-5.4-mini','xhigh')
  OR competition_v2_winner_review_provider('elected_winner_gpt54mini_xhigh_v1','snow-qwen-gemma-muse-union','provider-default')
  OR competition_v2_winner_review_provider('elected_winner_snow_union_v1','qwen3.8-27b','provider-default')
  OR competition_v2_winner_review_provider('elected_winner_snow_union_v1','snow-qwen-gemma-muse-union','xhigh')
  OR competition_v2_winner_review_provider('elected_winner_snow_dual_v1','snow-qwen-gemma-muse-union','provider-default')
  OR competition_v2_winner_review_provider(NULL,'snow-qwen-gemma-muse-union','provider-default') THEN
  RAISE EXCEPTION 'Snow provider tuple mixed provenance';
 END IF;
 SELECT jsonb_agg(jsonb_build_object('model',m,'criterion',c,'disposition','pass','reason_code','APPROVED',
  'explanation','Synthetic complete evidence.','evidence_sha256',repeat('c',64)) ORDER BY m,c) INTO findings
 FROM unnest(ARRAY['qwen3.8-27b','gemma-4-26b-a4b','muse-glimmer-30b']) m
 CROSS JOIN unnest(ARRAY['measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety']) c;
 request:=jsonb_build_object('reward_disposition','approved','source_disposition','approved','reason_codes',jsonb_build_array('APPROVED'),'findings',findings);
 IF NOT competition_v2_snow_review_findings_valid(request) THEN RAISE EXCEPTION 'complete Snow approval refused'; END IF;
 altered:=jsonb_set(request,'{findings}',findings-17);
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'missing member criterion accepted'; END IF;
 altered:=jsonb_set(request,'{findings,17}',findings->0);
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'duplicate member criterion accepted'; END IF;
 altered:=jsonb_set(request,'{findings,0,model}',to_jsonb('synthetic-unapproved-member'::text));
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'unknown member accepted'; END IF;
 altered:=jsonb_set(request,'{findings,0,disposition}',to_jsonb('uncertain'::text));
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'uncertainty approved a source'; END IF;
 altered:=jsonb_set(jsonb_set(request,'{reward_disposition}',to_jsonb('unchanged'::text)),'{source_disposition}',to_jsonb('deferred'::text));
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'uncertain union conferred API authority'; END IF;
 request:=request||jsonb_build_object('reward_disposition','disqualified','source_disposition','rejected','reason_codes',jsonb_build_array('UNSAFE_BEHAVIOR'));
 request:=jsonb_set(jsonb_set(request,'{findings,0,disposition}',to_jsonb('fail'::text)),'{findings,0,reason_code}',to_jsonb('UNSAFE_BEHAVIOR'::text));
 request:=jsonb_set(jsonb_set(request,'{findings,7,disposition}',to_jsonb('uncertain'::text)),'{findings,7,reason_code}',to_jsonb('SOURCE_NOT_READY'::text));
 IF NOT competition_v2_snow_review_findings_valid(request) THEN RAISE EXCEPTION 'substantiated rejection lost another member uncertainty'; END IF;
 altered:=jsonb_set(request,'{findings,0,disposition}',to_jsonb('uncertain'::text));
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'uncertainty alone disqualified reward'; END IF;
 altered:=jsonb_set(request,'{reason_codes}',jsonb_build_array('SCORE_TAMPERING'));
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'unmatched finding disqualified reward'; END IF;
 altered:=jsonb_set(request,'{reason_codes}',jsonb_build_array('APPROVED'));
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'approval code disqualified reward'; END IF;
 altered:=jsonb_set(request,'{reason_codes}',jsonb_build_array('SOURCE_NOT_READY'));
 IF competition_v2_snow_review_findings_valid(altered) THEN RAISE EXCEPTION 'source concern alone disqualified reward'; END IF;
END $test_snow$;
`
