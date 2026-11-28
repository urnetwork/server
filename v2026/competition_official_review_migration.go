// An append-only operator amendment accepts an exact independently approved
// epoch-one review. Historical policies, clocks, scores and owners stay intact.
package server

import "strings"

func competitionOfficialReviewMigration() string {
	extract := func(sql, start, end string) string {
		i := strings.Index(sql, start)
		if i < 0 {
			panic("official review function anchor absent")
		}
		j := strings.Index(sql[i:], end)
		if j < 0 {
			panic("official review terminator absent")
		}
		return sql[i : i+j+len(end)]
	}
	replace := func(sql, old, next string) string {
		if strings.Count(sql, old) != 1 {
			panic("official review authority anchor ambiguous")
		}
		return strings.Replace(sql, old, next, 1)
	}
	previous := competitionReviewHandoffMigration()
	source := extract(previous, "CREATE OR REPLACE FUNCTION competition_v2_winner_source_authorized", "END $winner_source$;")
	exception := "competition_official_review_request_authorized(review.round_id,review.review_json->'request')"
	provider := "NOT competition_v2_winner_review_provider(policy->>'winner_review_policy',review.review_json->'request'->>'reviewer_model',review.review_json->'request'->>'reviewer_reasoning')"
	source = replace(source, provider, "("+provider+" AND NOT "+exception+")")
	census := "policy->>'winner_review_policy' IN ('elected_winner_snow_union_v1','elected_winner_snow_union_v2') AND NOT competition_v2_snow_review_findings_valid_for_policy(review.review_json->'request',policy->>'winner_review_policy')"
	source = replace(source, census, census+" AND NOT "+exception)
	handoff := "COALESCE(checkpoint->>'handoff_id','') IS DISTINCT FROM COALESCE(review.review_json->'request'->>'handoff_id','')"
	source = replace(source, handoff, "("+handoff+" AND NOT "+exception+")")
	clock := "competition_review_handoff_observed(review.review_json,review.created_at)>=(checkpoint->>'started_at')::timestamp AND competition_review_handoff_observed(review.review_json,review.created_at)<(checkpoint->>'deadline')::timestamp"
	source = replace(source, clock, "("+exception+" OR ("+clock+"))")
	review := extract(previous, "CREATE OR REPLACE FUNCTION competition_v2_winner_review_guard", "END $winner_review$;")
	review = replace(review, " request:=NEW.review_json->'request';", ` request:=NEW.review_json->'request';
 IF request ? 'official_review_override' THEN
  IF NOT competition_official_review_request_authorized(NEW.round_id,request) THEN RAISE EXCEPTION 'official review lacks exact operator authority'; END IF;
  RETURN NEW;
 END IF;`)
	identity := extract(competitionSnowTwoMemberWinnerReviewMigration(), "CREATE OR REPLACE FUNCTION competition_v2_review_identity_guard()", "END $guard$;")
	identity = replace(identity, "CASE WHEN round_policy->'automation_policy'->>'winner_review_policy' IN ('elected_winner_snow_union_v1','elected_winner_snow_union_v2')",
		"CASE WHEN competition_official_review_request_authorized(NEW.round_id,request) THEN NOT competition_official_review_findings_valid(request) WHEN round_policy->'automation_policy'->>'winner_review_policy' IN ('elected_winner_snow_union_v1','elected_winner_snow_union_v2')")
	return competitionOfficialReviewSql + source + "\n" + review + "\n" + identity
}

const competitionOfficialReviewSql = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE competition_official_review_authorization (
 authorization_sha256 text PRIMARY KEY CHECK(authorization_sha256 ~ '^[0-9a-f]{64}$'),
 round_id uuid NOT NULL REFERENCES competition_round(round_id),
 actor_id text NOT NULL,
 import_sha256 text NOT NULL CHECK(import_sha256 ~ '^[0-9a-f]{64}$'),
 import_json jsonb NOT NULL,
 created_at timestamp NOT NULL
);
CREATE TRIGGER competition_official_review_immutable BEFORE UPDATE OR DELETE ON competition_official_review_authorization FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();

CREATE FUNCTION competition_official_review_findings_valid(request jsonb)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $official_findings$
 SELECT COALESCE(
  request->>'reviewer_model'='gpt-6-astra' AND request->>'reviewer_reasoning'='max'
  AND COALESCE(request->>'handoff_id','')=''
  AND request->>'reward_disposition'='approved' AND request->>'source_disposition'='approved'
  AND COALESCE(request->>'appeal_id','')='' AND NOT(request ? 'supersedes_review_id') AND NOT(request ? 'expected_replacement_job_id')
  AND request->'reason_codes'='["APPROVED"]'::jsonb
  AND jsonb_typeof(request->'official_review_override')='object'
  AND request->'official_review_override'->>'authorization_sha256' ~ '^[0-9a-f]{64}$'
  AND request->'official_review_override'->'native_qualification_waived'='true'::jsonb
  AND request->'official_review_override'->'unaccepted_calibration_acknowledged'='true'::jsonb
  AND jsonb_array_length(request->'findings')=6
  AND (SELECT count(DISTINCT f->>'criterion') FROM jsonb_array_elements(request->'findings') f WHERE f->>'criterion' IN ('measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety'))=6
  AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(request->'findings') f WHERE COALESCE(f->>'model','')<>'' OR f->>'disposition' IS DISTINCT FROM 'pass' OR f->>'reason_code' IS DISTINCT FROM 'APPROVED' OR COALESCE(f->>'evidence_sha256','') !~ '^[0-9a-f]{64}$' OR octet_length(COALESCE(f->>'explanation','')) NOT BETWEEN 1 AND 4096)
  AND competition_v2_production_benefit_review_valid(request),false);
$official_findings$;

CREATE FUNCTION competition_official_review_request_authorized(round_uuid uuid,request jsonb)
RETURNS boolean LANGUAGE sql STABLE AS $official_authorized$
 SELECT COALESCE(EXISTS(
  SELECT 1 FROM competition_official_review_authorization a
  JOIN competition_round r ON r.round_id=a.round_id
  JOIN competition_evaluation_seal s ON s.round_id=r.round_id
  JOIN LATERAL(SELECT decision_json FROM competition_reward_decision WHERE round_id=r.round_id ORDER BY revision DESC LIMIT 1) d ON true
  WHERE a.round_id=round_uuid AND r.epoch_number=1 AND NOT r.staging
   AND a.authorization_sha256=request->'official_review_override'->>'authorization_sha256'
   AND a.import_json->'review'=request AND a.import_json->>'round_id'=r.round_id::text
   AND competition_official_review_findings_valid(request)
   AND request->>'job_id'=d.decision_json->>'selected_job_id'
   AND request->>'expected_revision'=d.decision_json->>'revision'
   AND request->>'expected_decision_sha256'=d.decision_json->>'decision_sha256'
   AND request->>'baseline_sha256'=s.seal_json->>'baseline_sha256'
   AND (request->'official_review_override'->>'completed_at')::timestamp<=a.created_at
   AND (request->'official_review_override'->>'completed_at')::timestamp>=(s.seal_json->>'publication_at')::timestamp
   AND EXISTS(SELECT 1 FROM jsonb_array_elements(s.seal_json->'entries') e WHERE e->>'job_id'=request->>'job_id' AND e->>'state'='succeeded'
    AND e->>'patch_sha256'=request->>'patch_sha256' AND e->>'base_source_lock_sha256'=request->>'base_source_lock_sha256'
    AND e->>'score_sha256'=request->>'score_sha256' AND e->>'artifact_manifest_sha256'=request->>'artifact_manifest_sha256')
   AND NOT EXISTS(SELECT 1 FROM competition_automation_incident i WHERE i.round_id=r.round_id AND i.invalidates_evaluation)
 ),false);
$official_authorized$;

CREATE FUNCTION competition_official_review_authorization_guard() RETURNS trigger LANGUAGE plpgsql AS $official_grant$
DECLARE r competition_round%ROWTYPE; request jsonb;
BEGIN
 SELECT * INTO r FROM competition_round WHERE round_id=NEW.round_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'official review round absent'; END IF;
 PERFORM competition_v2_scope_lock(r.policy_json->'automation_policy'->>'environment',r.competition_id,r.policy_json->'automation_policy'->>'season_id');
 PERFORM 1 FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 request:=NEW.import_json->'review';
 IF r.epoch_number<>1 OR r.staging OR NEW.actor_id IS DISTINCT FROM current_setting('urnetwork.competition_official_review_actor',true)
  OR NEW.import_sha256 IS DISTINCT FROM current_setting('urnetwork.competition_official_review_import_sha256',true)
  OR NEW.created_at IS DISTINCT FROM NULLIF(current_setting('urnetwork.competition_review_handoff_clock',true),'')::timestamp
  OR NEW.import_json->>'round_id' IS DISTINCT FROM NEW.round_id::text
  OR NEW.authorization_sha256 IS DISTINCT FROM request->'official_review_override'->>'authorization_sha256'
  OR NOT competition_official_review_findings_valid(request)
  OR NOT COALESCE((request->'official_review_override'->>'completed_at')::timestamp<=NEW.created_at,false)
  OR octet_length(NEW.import_json::text)>131072 THEN RAISE EXCEPTION 'official review requires exact independently authorized epoch-one evidence'; END IF;
 RETURN NEW;
END $official_grant$;
CREATE TRIGGER competition_official_review_authorization_before_insert BEFORE INSERT ON competition_official_review_authorization FOR EACH ROW EXECUTE FUNCTION competition_official_review_authorization_guard();
`
