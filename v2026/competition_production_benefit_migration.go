// An additive migration admits the new eligibility reason only with its exact
// addendum and supporting finding. Old binaries and immutable reviews still work.
package server

import "strings"

// Only the reason predicates gain the addendum; no published migration bytes,
// source/provider guards, round policies or historical rows are rewritten.
func competitionProductionBenefitReviewMigration() string {
	replace := func(sql, anchor, replacement string, count int) string {
		if strings.Count(sql, anchor) != count {
			panic("production-benefit migration anchor is ambiguous")
		}
		return strings.ReplaceAll(sql, anchor, replacement)
	}
	findings := replace(competitionSnowReviewFindingsSql,
		"'EVALUATION_INTEGRITY_INCIDENT','APPEAL_UPHELD'];",
		"'EVALUATION_INTEGRITY_INCIDENT','APPEAL_UPHELD','NO_PRODUCTION_BENEFIT'];", 1)
	findings = replace(findings, "BEGIN\n IF jsonb_typeof(request)",
		"BEGIN\n IF NOT competition_v2_production_benefit_review_valid(request) THEN RETURN false; END IF;\n IF jsonb_typeof(request)", 1)
	twoMembers := replace(findings, "competition_v2_snow_review_findings_valid(request jsonb)", "competition_v2_snow_review_findings_valid_v2(request jsonb)", 1)
	twoMembers = replace(twoMembers, "<>18", "<>12", 2)
	twoMembers = replace(twoMembers, "'qwen3.8-27b','gemma-4-26b-a4b','muse-glimmer-30b'", "'qwen3.8-27b','gemma-4-26b-a4b'", 1)
	policy := replace(competitionProductionBenefitReviewSql, "@production_benefit_version@", CompetitionProductionBenefitVersion, 1)
	policy = replace(policy, "@production_benefit_sha256@", CompetitionProductionBenefitAddendumSha256(), 1)
	return policy + findings + twoMembers
}

// The migration is compatible before code rollout: absence without the new
// reason remains valid. The application enforces mandatory identity on NEW
// inserts after exact replay; a migration must not strand the previous release.
const competitionProductionBenefitReviewSql = `
SET LOCAL lock_timeout = '5s';
CREATE FUNCTION competition_v2_production_benefit_review_valid(request jsonb)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $production_benefit$
DECLARE finding jsonb; has_reason boolean; supported boolean:=false; generalized boolean:=false;
BEGIN
 IF jsonb_typeof(request) IS DISTINCT FROM 'object'
  OR jsonb_typeof(request->'reason_codes') IS DISTINCT FROM 'array'
  OR jsonb_typeof(request->'findings') IS DISTINCT FROM 'array' THEN RETURN false; END IF;
 has_reason:=(request->'reason_codes') ? 'NO_PRODUCTION_BENEFIT';
 FOR finding IN SELECT value FROM jsonb_array_elements(request->'findings') LOOP
  IF finding->>'reason_code'='NO_PRODUCTION_BENEFIT' THEN
   IF finding->>'criterion' IS DISTINCT FROM 'workload_generalization' OR finding->>'disposition' IS DISTINCT FROM 'fail' OR NOT has_reason THEN RETURN false; END IF;
   supported:=true;
  END IF;
 END LOOP;
 IF NOT (request ? 'production_benefit_addendum') THEN RETURN NOT has_reason AND NOT supported; END IF;
 IF request->'production_benefit_addendum' IS DISTINCT FROM jsonb_build_object('version','@production_benefit_version@','sha256','@production_benefit_sha256@') THEN RETURN false; END IF;
 IF has_reason AND (NOT supported OR request->>'reward_disposition' IS DISTINCT FROM 'disqualified' OR request->>'source_disposition' IS DISTINCT FROM 'rejected') THEN RETURN false; END IF;
 IF request->>'reward_disposition' IN ('approved','restored') OR request->>'source_disposition'='approved' THEN
  FOR finding IN SELECT value FROM jsonb_array_elements(request->'findings') LOOP
   IF finding->>'criterion'='workload_generalization' THEN
    generalized:=true;
    IF finding->>'disposition' IS DISTINCT FROM 'pass' THEN RETURN false; END IF;
   END IF;
  END LOOP;
  IF NOT generalized THEN RETURN false; END IF;
 END IF;
 RETURN true;
END $production_benefit$;
CREATE FUNCTION competition_v2_production_benefit_review_guard()
RETURNS trigger LANGUAGE plpgsql AS $production_benefit_guard$
BEGIN
 IF NOT competition_v2_production_benefit_review_valid(NEW.review_json->'request') THEN RAISE EXCEPTION 'production-benefit review addendum or supporting eligibility finding is invalid'; END IF;
 RETURN NEW;
END $production_benefit_guard$;
CREATE TRIGGER competition_v2_production_benefit_review BEFORE INSERT ON competition_integrity_review FOR EACH ROW EXECUTE FUNCTION competition_v2_production_benefit_review_guard();
`
