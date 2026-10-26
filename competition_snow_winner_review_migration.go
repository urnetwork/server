// Adds Snow union authority prospectively; published provider migrations and
// retained single-model reviews keep their original meanings and digests.
package server

import "strings"

// Reuses only the latest provider/scheduled-opening definitions, leaving the
// newer mono-root source authorization installed. The identity trigger gains
// a policy-scoped eighteen-finding census without rewriting historical rows.
func competitionSnowWinnerReviewMigration() string {
	replace := func(sql, anchor, replacement string, count int) string {
		if strings.Count(sql, anchor) != count {
			panic("Snow review migration authority anchor is ambiguous")
		}
		return strings.ReplaceAll(sql, anchor, replacement)
	}
	provider := competitionWinnerReviewProviderMigration()
	provider = replace(provider,
		"('elected_winner_gpt54mini_xhigh_v1','gpt-5.4-mini','xhigh')",
		"('elected_winner_gpt54mini_xhigh_v1','gpt-5.4-mini','xhigh'),\n  ('elected_winner_snow_union_v1','snow-qwen-gemma-muse-union','provider-default')", 1)
	provider = replace(provider,
		"('elected_winner_sol61max_v1','elected_winner_gpt54mini_xhigh_v1')",
		"('elected_winner_sol61max_v1','elected_winner_gpt54mini_xhigh_v1','elected_winner_snow_union_v1')", 3)
	provider = replace(provider,
		"NOT competition_v2_winner_review_provider(policy->>'winner_review_policy',review.review_json->'request'->>'reviewer_model',review.review_json->'request'->>'reviewer_reasoning')",
		"NOT competition_v2_winner_review_provider(policy->>'winner_review_policy',review.review_json->'request'->>'reviewer_model',review.review_json->'request'->>'reviewer_reasoning')\n  OR (policy->>'winner_review_policy'='elected_winner_snow_union_v1' AND NOT competition_v2_snow_review_findings_valid(review.review_json->'request'))", 1)
	provider = replace(provider, " request:=NEW.review_json->'request';",
		" request:=NEW.review_json->'request';\n IF policy->>'winner_review_policy'='elected_winner_snow_union_v1' AND NOT competition_v2_snow_review_findings_valid(request) THEN RAISE EXCEPTION 'Snow review requires the complete model-attributed union'; END IF;", 1)
	declaration := "CREATE FUNCTION competition_v2_review_identity_guard()"
	start := strings.Index(competitionAutomationGuardMigration, declaration)
	if start < 0 {
		panic("review identity migration function is absent")
	}
	end := strings.Index(competitionAutomationGuardMigration[start:], "END $guard$;")
	if end < 0 {
		panic("review identity migration function is unterminated")
	}
	identity := competitionAutomationGuardMigration[start : start+end+len("END $guard$;")]
	identity = strings.Replace(identity, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
	identity = replace(identity,
		"  OR jsonb_array_length(request->'findings')<>6\n  OR (SELECT count(DISTINCT f->>'criterion') FROM jsonb_array_elements(request->'findings') f WHERE f->>'criterion' IN ('measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety'))<>6",
		`  OR CASE WHEN round_policy->'automation_policy'->>'winner_review_policy'='elected_winner_snow_union_v1'
   THEN NOT competition_v2_snow_review_findings_valid(request)
   ELSE jsonb_array_length(request->'findings')<>6
    OR (SELECT count(DISTINCT f->>'criterion') FROM jsonb_array_elements(request->'findings') f WHERE f->>'criterion' IN ('measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety'))<>6
    OR EXISTS(SELECT 1 FROM jsonb_array_elements(request->'findings') f WHERE COALESCE(f->>'model','')<>'') END`, 1)
	return competitionSnowReviewFindingsSql + provider + "\n" + identity
}

// Findings retain every member's uncertainty without allowing uncertainty to
// approve a source or disqualify a winner without a matched substantive fail.
const competitionSnowReviewFindingsSql = `
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid(request jsonb)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $snow_findings$
DECLARE finding jsonb; reason jsonb; disposition text; supported_failure boolean:=false;
 allowed_reasons text[]:=ARRAY['APPROVED','SCORE_TAMPERING','MEASUREMENT_FABRICATION','WORKLOAD_SPECIAL_CASING','BENCHMARK_DETECTION','SANDBOX_ESCAPE','SECRET_ACCESS','EXFILTRATION','UNSAFE_BEHAVIOR','PERSISTENCE','DENIAL_OF_SERVICE','PATCH_POLICY_VIOLATION','UNRELATED_CHANGE','REWARD_DUPLICATE','REWARD_RULE_EVASION','SOURCE_NOT_READY','PRODUCT_INTEGRATION_RISK','EVALUATION_INTEGRITY_INCIDENT','APPEAL_UPHELD'];
BEGIN
 IF jsonb_typeof(request) IS DISTINCT FROM 'object'
  OR jsonb_typeof(request->'findings') IS DISTINCT FROM 'array'
  OR jsonb_typeof(request->'reason_codes') IS DISTINCT FROM 'array' THEN RETURN false; END IF;
 IF jsonb_array_length(request->'findings')<>18 OR jsonb_array_length(request->'reason_codes') NOT BETWEEN 1 AND 16
  OR COALESCE(request->>'reward_disposition','') NOT IN ('approved','restored','disqualified')
  OR (request->>'reward_disposition' IN ('approved','restored') AND request->>'source_disposition' IS DISTINCT FROM 'approved')
  OR (request->>'reward_disposition'='disqualified' AND request->>'source_disposition' IS DISTINCT FROM 'rejected') THEN RETURN false; END IF;
 FOR reason IN SELECT value FROM jsonb_array_elements(request->'reason_codes') LOOP
  IF jsonb_typeof(reason) IS DISTINCT FROM 'string' OR NOT ((reason#>>'{}')=ANY(allowed_reasons))
   OR (request->>'reward_disposition'='disqualified' AND (reason#>>'{}') IN ('APPROVED','APPEAL_UPHELD','SOURCE_NOT_READY','PRODUCT_INTEGRATION_RISK','EVALUATION_INTEGRITY_INCIDENT')) THEN RETURN false; END IF;
 END LOOP;
 IF (SELECT count(DISTINCT (f->>'model',f->>'criterion')) FROM jsonb_array_elements(request->'findings') f
  WHERE f->>'model' IN ('qwen3.8-27b','gemma-4-26b-a4b','muse-glimmer-30b')
   AND f->>'criterion' IN ('measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety'))<>18 THEN RETURN false; END IF;
 FOR finding IN SELECT value FROM jsonb_array_elements(request->'findings') LOOP
  disposition:=finding->>'disposition';
  IF jsonb_typeof(finding) IS DISTINCT FROM 'object'
   OR jsonb_typeof(finding->'model') IS DISTINCT FROM 'string'
   OR jsonb_typeof(finding->'criterion') IS DISTINCT FROM 'string'
   OR jsonb_typeof(finding->'disposition') IS DISTINCT FROM 'string'
   OR jsonb_typeof(finding->'reason_code') IS DISTINCT FROM 'string'
   OR NOT ((finding->>'reason_code')=ANY(allowed_reasons))
   OR jsonb_typeof(finding->'explanation') IS DISTINCT FROM 'string'
   OR octet_length(finding->>'explanation') NOT BETWEEN 1 AND 4096
   OR jsonb_typeof(finding->'evidence_sha256') IS DISTINCT FROM 'string'
   OR (finding->>'evidence_sha256')!~'^[0-9a-f]{64}$'
   OR disposition NOT IN ('pass','fail','not_applicable','uncertain')
   OR (disposition='uncertain' AND request->>'reward_disposition'<>'disqualified')
   OR (disposition IN ('fail','uncertain') AND request->>'source_disposition'='approved') THEN RETURN false; END IF;
  IF disposition='fail' AND (request->'reason_codes') ? (finding->>'reason_code') THEN supported_failure:=true; END IF;
 END LOOP;
 IF request->>'reward_disposition'='disqualified' AND NOT supported_failure THEN RETURN false; END IF;
 RETURN true;
END $snow_findings$;
`
