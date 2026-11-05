// Native and PostgreSQL evidence contracts share the versioned public addendum;
// historical predicates and old-release writes retain their original meaning.
package server

import (
	"strings"
	"testing"
)

// A text change requires an explicit new version and qualification, not an
// accidental change to what an existing version/hash is understood to mean.
func TestCompetitionProductionBenefitAddendumIdentityIsPinned(t *testing.T) {
	if CompetitionProductionBenefitVersion != "production-benefit/1" || CompetitionProductionBenefitAddendumSha256() != "6e2246f2cdef9f8d4348da95578b8f8a1676ce7afc7b5dc747ca5ce5c774ce32" {
		t.Fatal("published production-benefit addendum bytes changed")
	}
}

// This compatibility expansion is appended, never substituted into a published
// migration, and does not install a mandatory field before old code can send it.
func TestCompetitionProductionBenefitMigrationIsAdditive(t *testing.T) {
	historical := competitionSnowTwoMemberWinnerReviewMigration()
	sql := competitionProductionBenefitReviewMigration()
	if historical != competitionSnowTwoMemberWinnerReviewMigration() || strings.Contains(historical, "NO_PRODUCTION_BENEFIT") {
		t.Fatal("eligibility migration rewrote historical SQL")
	}
	for _, required := range []string{"competition_v2_production_benefit_review_valid(request jsonb)", "'NO_PRODUCTION_BENEFIT'", CompetitionProductionBenefitVersion, CompetitionProductionBenefitAddendumSha256(), "RETURN NOT has_reason AND NOT supported", "competition_v2_snow_review_findings_valid(request jsonb)", "competition_v2_snow_review_findings_valid_v2(request jsonb)", "<>18", "<>12"} {
		if !strings.Contains(sql, required) {
			t.Errorf("eligibility migration omitted %q", required)
		}
	}
	for _, forbidden := range []string{"UPDATE competition_", "DELETE FROM competition_", "FUNCTION competition_v2_source_authorized(", "FUNCTION competition_v2_round_insert_guard("} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("eligibility migration changes unrelated history or authority: %s", forbidden)
		}
	}
}

// Runs the real PostgreSQL predicates for both unions and old/new requests in
// the existing disposable fixture. No live database or provider is contacted.
func TestCompetitionProductionBenefitDatabaseRejectsUnboundReason(t *testing.T) {
	assertions := strings.ReplaceAll(competitionProductionBenefitSqlAssertions, "@addendum_sha256@", CompetitionProductionBenefitAddendumSha256())
	testCompetitionWinnerReviewDatabaseGuards(t,
		competitionWinnerReviewMigration()+competitionWinnerReviewProviderMigration()+competitionSnowWinnerReviewMigration()+competitionSnowTwoMemberWinnerReviewMigration()+competitionProductionBenefitReviewMigration(), assertions)
}

const competitionProductionBenefitSqlAssertions = `
DO $test_production_benefit$
DECLARE findings jsonb; request jsonb; rejected jsonb; altered jsonb; policy_name text; members text[];
BEGIN
 FOR policy_name,members IN SELECT 'elected_winner_snow_union_v1',ARRAY['qwen3.8-27b','gemma-4-26b-a4b','muse-glimmer-30b']
  UNION ALL SELECT 'elected_winner_snow_union_v2',ARRAY['qwen3.8-27b','gemma-4-26b-a4b'] LOOP
  SELECT jsonb_agg(jsonb_build_object('model',member,'criterion',criterion,'disposition','pass','reason_code','APPROVED','explanation','Synthetic code supports production benefit.','evidence_sha256',repeat('a',64))) INTO findings
   FROM unnest(members) member CROSS JOIN unnest(ARRAY['measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety']) criterion;
  request:=jsonb_build_object('findings',findings,'reason_codes',jsonb_build_array('APPROVED'),'reward_disposition','approved','source_disposition','approved');
  IF NOT competition_v2_snow_review_findings_valid_for_policy(request,policy_name) THEN RAISE EXCEPTION 'old release request stranded'; END IF;
  request:=request||jsonb_build_object('production_benefit_addendum',jsonb_build_object('version','production-benefit/1','sha256','@addendum_sha256@'));
  IF NOT competition_v2_snow_review_findings_valid_for_policy(request,policy_name) THEN RAISE EXCEPTION 'bound new approval refused'; END IF;
  SELECT jsonb_agg(CASE WHEN f->>'criterion'='workload_generalization' THEN f||jsonb_build_object('disposition','fail','reason_code','NO_PRODUCTION_BENEFIT') ELSE f END) INTO findings FROM jsonb_array_elements(findings) f;
  rejected:=request||jsonb_build_object('findings',findings,'reason_codes',jsonb_build_array('NO_PRODUCTION_BENEFIT'),'reward_disposition','disqualified','source_disposition','rejected');
  IF NOT competition_v2_snow_review_findings_valid_for_policy(rejected,policy_name) THEN RAISE EXCEPTION 'explicit non-misconduct eligibility rejection refused'; END IF;
  FOR altered IN SELECT rejected-'production_benefit_addendum'
   UNION ALL SELECT jsonb_set(rejected,'{production_benefit_addendum,sha256}',to_jsonb(repeat('0',64)))
   UNION ALL SELECT jsonb_set(rejected,'{production_benefit_addendum,version}','"production-benefit/0"'::jsonb)
   UNION ALL SELECT rejected||jsonb_build_object('findings',request->'findings')
   UNION ALL SELECT rejected||jsonb_build_object('reason_codes',jsonb_build_array('UNSAFE_BEHAVIOR')) LOOP
   IF competition_v2_production_benefit_review_valid(altered) OR competition_v2_snow_review_findings_valid_for_policy(altered,policy_name) THEN RAISE EXCEPTION 'unbound/unsupported eligibility reason accepted'; END IF;
  END LOOP;
 END LOOP;
END $test_production_benefit$;
`
