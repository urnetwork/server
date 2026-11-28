// An append-only epoch-two reviewer substitution preserves the published
// epoch-one waiver and all election, checkpoint, calibration and source guards.
package server

import "strings"

// Exact predecessor anchors prevent an unrelated authority change from being
// silently inherited. No frozen policy, old review or native ledger is edited.
func competitionOfficialReviewEpochTwoMigration() string {
	extract := func(sql, start, end string) string {
		i := strings.Index(sql, start)
		if i < 0 {
			panic("epoch-two official review predecessor absent")
		}
		j := strings.Index(sql[i:], end)
		if j < 0 {
			panic("epoch-two official review predecessor unterminated")
		}
		return sql[i : i+j+len(end)]
	}
	replace := func(sql, old, next string) string {
		if strings.Count(sql, old) != 1 {
			panic("epoch-two official review authority anchor ambiguous")
		}
		return strings.Replace(sql, old, next, 1)
	}
	previous := competitionOfficialReviewMigration()
	authorized := extract(previous, "CREATE FUNCTION competition_official_review_request_authorized", "$official_authorized$;")
	authorized = replace(authorized, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION")
	authorized = replace(authorized, "r.epoch_number=1 AND NOT r.staging", "r.epoch_number IN (1,2) AND NOT r.staging")
	authorized = replace(authorized, "AND competition_official_review_findings_valid(request)",
		"AND (CASE WHEN r.epoch_number=1 THEN competition_official_review_findings_valid(request) ELSE competition_official_review_epoch_two_findings_valid(request) AND competition_official_review_epoch_two_binding(round_uuid,request,a.created_at) END)")
	authorized = replace(authorized, "AND (request->'official_review_override'->>'completed_at')::timestamp>=(s.seal_json->>'publication_at')::timestamp",
		"AND (r.epoch_number=2 OR (request->'official_review_override'->>'completed_at')::timestamp>=(s.seal_json->>'publication_at')::timestamp)")
	grant := extract(previous, "CREATE FUNCTION competition_official_review_authorization_guard()", "END $official_grant$;")
	grant = replace(grant, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION")
	grant = replace(grant, "r.epoch_number<>1 OR r.staging", "r.epoch_number NOT IN (1,2) OR r.staging")
	grant = replace(grant, "OR NOT competition_official_review_findings_valid(request)",
		"OR NOT (CASE WHEN r.epoch_number=1 THEN competition_official_review_findings_valid(request) ELSE competition_official_review_epoch_two_findings_valid(request) AND competition_official_review_epoch_two_binding(NEW.round_id,request,NEW.created_at) END)")
	grant = replace(grant, "exact independently authorized epoch-one evidence", "exact independently authorized epoch-specific evidence")
	identity := extract(previous, "CREATE OR REPLACE FUNCTION competition_v2_review_identity_guard()", "END $guard$;")
	identity = replace(identity, "THEN NOT competition_official_review_findings_valid(request)",
		"THEN NOT (competition_official_review_findings_valid(request) OR competition_official_review_epoch_two_findings_valid(request))")
	review := extract(previous, "CREATE OR REPLACE FUNCTION competition_v2_winner_review_guard", "END $winner_review$;")
	review = replace(review, "IF NOT competition_official_review_request_authorized(NEW.round_id,request) THEN RAISE EXCEPTION 'official review lacks exact operator authority'; END IF;",
		"IF NOT competition_official_review_request_authorized(NEW.round_id,request) THEN RAISE EXCEPTION 'official review lacks exact operator authority'; END IF;\n  IF epoch=2 AND NOT competition_official_review_epoch_two_binding(NEW.round_id,request,NEW.created_at) THEN RAISE EXCEPTION 'epoch-two import must retain the actual checkpoint deadline'; END IF;")
	// Only epoch one retains the historical clock exception. Epoch-two source
	// promotion must still observe the real persisted import inside its checkpoint.
	source := extract(previous, "CREATE OR REPLACE FUNCTION competition_v2_winner_source_authorized", "END $winner_source$;")
	clock := "(competition_official_review_request_authorized(review.round_id,review.review_json->'request') OR (competition_review_handoff_observed"
	source = replace(source, clock, "((competition_official_review_findings_valid(review.review_json->'request') AND competition_official_review_request_authorized(review.round_id,review.review_json->'request')) OR (competition_review_handoff_observed")
	return competitionOfficialReviewEpochTwoSql + authorized + "\n" + grant + "\n" + identity + "\n" + source + "\n" + review
}

const competitionOfficialReviewEpochTwoSql = `
SET LOCAL lock_timeout = '5s';
CREATE FUNCTION competition_official_review_epoch_two_findings_valid(request jsonb)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $official_two_findings$
DECLARE finding jsonb; reason jsonb; supported_failure boolean:=false;
 allowed_reasons text[]:=ARRAY['APPROVED','SCORE_TAMPERING','MEASUREMENT_FABRICATION','WORKLOAD_SPECIAL_CASING','BENCHMARK_DETECTION','SANDBOX_ESCAPE','SECRET_ACCESS','EXFILTRATION','UNSAFE_BEHAVIOR','PERSISTENCE','DENIAL_OF_SERVICE','PATCH_POLICY_VIOLATION','UNRELATED_CHANGE','REWARD_DUPLICATE','REWARD_RULE_EVASION','SOURCE_NOT_READY','PRODUCT_INTEGRATION_RISK','EVALUATION_INTEGRITY_INCIDENT','APPEAL_UPHELD','NO_PRODUCTION_BENEFIT'];
BEGIN
 IF jsonb_typeof(request) IS DISTINCT FROM 'object'
  OR jsonb_typeof(request->'findings') IS DISTINCT FROM 'array'
  OR jsonb_typeof(request->'reason_codes') IS DISTINCT FROM 'array'
  OR jsonb_typeof(request->'official_review_override') IS DISTINCT FROM 'object' THEN RETURN false; END IF;
 IF request->>'reviewer_model' IS DISTINCT FROM 'gpt-6-astra' OR request->>'reviewer_reasoning' IS DISTINCT FROM 'max'
  OR COALESCE(request->>'handoff_id','')<>'' OR COALESCE(request->>'appeal_id','')<>''
  OR request ? 'supersedes_review_id' OR request ? 'expected_replacement_job_id'
  OR request->'official_review_override'->'native_qualification_waived' IS DISTINCT FROM 'false'::jsonb
  OR jsonb_typeof(request->'official_review_override'->'unaccepted_calibration_acknowledged') IS DISTINCT FROM 'boolean'
  OR COALESCE(request->'official_review_override'->>'authorization_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(request->'official_review_override'->>'completed_at','')=''
  OR COALESCE(request->>'expected_decision_sha256','') !~ '^[0-9a-f]{64}$'
  OR jsonb_array_length(request->'findings')<>6 OR jsonb_array_length(request->'reason_codes') NOT BETWEEN 1 AND 16
  OR COALESCE(request->>'reward_disposition','') NOT IN ('approved','disqualified','unchanged')
  OR COALESCE(request->>'source_disposition','') NOT IN ('approved','rejected','deferred','unchanged')
  OR ((request->>'reward_disposition'='disqualified') IS DISTINCT FROM (request->>'source_disposition'='rejected'))
  OR (request->>'source_disposition'='approved' AND request->>'reward_disposition'<>'approved')
  OR NOT (request ? 'production_benefit_addendum') OR NOT competition_v2_production_benefit_review_valid(request) THEN RETURN false; END IF;
 FOR reason IN SELECT value FROM jsonb_array_elements(request->'reason_codes') LOOP
  IF jsonb_typeof(reason) IS DISTINCT FROM 'string' OR NOT ((reason#>>'{}')=ANY(allowed_reasons))
   OR (request->>'reward_disposition'='disqualified' AND (reason#>>'{}') IN ('APPROVED','APPEAL_UPHELD','SOURCE_NOT_READY','PRODUCT_INTEGRATION_RISK','EVALUATION_INTEGRITY_INCIDENT')) THEN RETURN false; END IF;
 END LOOP;
 IF (SELECT count(DISTINCT f->>'criterion') FROM jsonb_array_elements(request->'findings') f
  WHERE f->>'criterion' IN ('measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety'))<>6 THEN RETURN false; END IF;
 FOR finding IN SELECT value FROM jsonb_array_elements(request->'findings') LOOP
  IF jsonb_typeof(finding) IS DISTINCT FROM 'object' OR COALESCE(finding->>'model','')<>''
   OR COALESCE(finding->>'disposition','') NOT IN ('pass','fail','not_applicable')
   OR NOT COALESCE((finding->>'reason_code')=ANY(allowed_reasons),false)
   OR jsonb_typeof(finding->'explanation') IS DISTINCT FROM 'string' OR octet_length(finding->>'explanation') NOT BETWEEN 1 AND 4096
   OR COALESCE(finding->>'evidence_sha256','') !~ '^[0-9a-f]{64}$'
   OR (finding->>'disposition'='fail' AND (request->>'reward_disposition'='approved' OR request->>'source_disposition'='approved')) THEN RETURN false; END IF;
  IF finding->>'disposition'='fail' AND request->'reason_codes' ? (finding->>'reason_code') THEN supported_failure:=true; END IF;
 END LOOP;
 RETURN request->>'reward_disposition'<>'disqualified' OR supported_failure;
END $official_two_findings$;

CREATE FUNCTION competition_official_review_epoch_two_binding(round_uuid uuid,request jsonb,imported_at timestamp)
RETURNS boolean LANGUAGE sql STABLE AS $official_two_binding$
 SELECT COALESCE(EXISTS(
  SELECT 1 FROM competition_round r JOIN competition_evaluation_seal s USING(round_id)
  JOIN LATERAL(SELECT decision_json FROM competition_reward_decision WHERE round_id=r.round_id ORDER BY revision DESC LIMIT 1) d ON true
  JOIN LATERAL(SELECT transition_json->'winner_review' AS checkpoint FROM competition_supervisor_transition
   WHERE environment=r.policy_json->'automation_policy'->>'environment' AND competition_id=r.competition_id
    AND season_id=r.policy_json->'automation_policy'->>'season_id' AND target_epoch=r.epoch_number+1 ORDER BY revision DESC LIMIT 1) t ON true
  WHERE r.round_id=round_uuid AND r.epoch_number=2 AND NOT r.staging
   AND r.policy_json->'automation_policy'->>'winner_review_policy' IN ('elected_winner_sol61max_v1','elected_winner_gpt54mini_xhigh_v1','elected_winner_snow_union_v1','elected_winner_snow_union_v2')
   AND competition_official_review_epoch_two_findings_valid(request)
   AND (r.policy_json->'automation_policy'->>'repeat_reward_policy'='exclude_exact_identity' OR NOT (request->'reason_codes' ?| ARRAY['REWARD_DUPLICATE','REWARD_RULE_EVASION']))
   AND request->>'criteria_version'=r.policy_json->'automation_policy'->>'review_criteria_version'
   AND request->>'job_id'=d.decision_json->>'selected_job_id' AND request->>'expected_revision'=d.decision_json->>'revision'
   AND request->>'expected_decision_sha256'=d.decision_json->>'decision_sha256'
   AND request->>'baseline_sha256'=s.seal_json->>'baseline_sha256'
   AND COALESCE(checkpoint->>'handoff_id','')='' AND checkpoint->>'round_id'=r.round_id::text
   AND checkpoint->>'job_id'=request->>'job_id' AND checkpoint->>'reward_revision'=request->>'expected_revision'
   AND checkpoint->>'reward_decision_sha256'=request->>'expected_decision_sha256'
   AND imported_at>=(checkpoint->>'started_at')::timestamp AND imported_at<(checkpoint->>'deadline')::timestamp
   AND (request->'official_review_override'->>'completed_at')::timestamp<=imported_at
   AND EXISTS(SELECT 1 FROM competition_round_baseline b WHERE b.round_id=r.round_id AND b.baseline_sha256=request->>'baseline_sha256'
    AND b.created_at<=(request->'official_review_override'->>'completed_at')::timestamp)
   AND EXISTS(SELECT 1 FROM jsonb_array_elements(s.seal_json->'entries') e WHERE e->>'job_id'=request->>'job_id' AND e->>'state'='succeeded'
    AND (e->>'completed_at')::timestamp<=(request->'official_review_override'->>'completed_at')::timestamp
    AND e->>'patch_sha256'=request->>'patch_sha256' AND e->>'base_source_lock_sha256'=request->>'base_source_lock_sha256'
    AND e->>'score_sha256'=request->>'score_sha256' AND e->>'artifact_manifest_sha256'=request->>'artifact_manifest_sha256')
   AND NOT EXISTS(SELECT 1 FROM competition_automation_incident i WHERE i.round_id=r.round_id AND i.invalidates_evaluation)
 ),false);
$official_two_binding$;
`
