// Adds elected-winner authority prospectively without rewriting historical
// migrations, source locks, seals, or the independent-source legacy policy.
package server

import "strings"

// Reuses the last complete source guard and changes its deadline only after
// the new guard authenticates the predecessor's explicitly frozen policy.
func competitionWinnerReviewMigration() string {
	start := strings.Index(competitionAutomationStagingSourceGraphMigration, "CREATE OR REPLACE FUNCTION competition_v2_source_authorized")
	if start < 0 {
		panic("source authorization migration template is absent")
	}
	source := competitionAutomationStagingSourceGraphMigration[start:]
	anchor := " IF intent->>'kind'='reviewed_source_carry_forward' THEN"
	if strings.Count(source, anchor) != 1 {
		panic("source authorization migration template is ambiguous")
	}
	source = strings.Replace(source, anchor, ` IF competition_v2_winner_source_authorized(intent,predecessor_id) THEN
  choice_at:=(intent->>'prepared_at')::timestamp;
 END IF;
`+anchor, 1)
	round := strings.Replace(competitionAutomationRuntimeBlobMigration,
		"preparation_seconds:=CASE WHEN NEW.staging", "preparation_seconds:=CASE WHEN intent->>'winner_review_policy'='elected_winner_sol61max_v1' THEN 0 WHEN NEW.staging", 1)
	round = strings.Replace(round, " IF NEW.staging IS DISTINCT FROM", " IF COALESCE(policy->>'winner_review_policy','') IS DISTINCT FROM COALESCE(intent->>'winner_review_policy','') THEN RAISE EXCEPTION 'round and activated source closing policy disagree'; END IF;\n IF NEW.staging IS DISTINCT FROM", 1)
	return competitionWinnerReviewGuards + source + round
}

const competitionWinnerReviewGuards = `
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION competition_v2_winner_source_authorized(intent jsonb, predecessor_id uuid)
RETURNS boolean LANGUAGE plpgsql AS $winner_source$
DECLARE policy jsonb; decision jsonb; checkpoint jsonb; actual_checkpoint jsonb;
 review competition_integrity_review%ROWTYPE;
BEGIN
 SELECT policy_json->'automation_policy' INTO policy FROM competition_round WHERE round_id=predecessor_id;
 IF COALESCE(policy->>'winner_review_policy','')='' THEN
  IF COALESCE(intent->>'winner_review_policy','')<>'' OR COALESCE(intent->'winner_review','null'::jsonb)<>'null'::jsonb THEN
   RAISE EXCEPTION 'cannot apply elected-winner policy to a historical round';
  END IF;
  RETURN false;
 END IF;
 IF policy->>'winner_review_policy' IS DISTINCT FROM 'elected_winner_sol61max_v1'
  OR intent->>'winner_review_policy' IS DISTINCT FROM policy->>'winner_review_policy' THEN
  RAISE EXCEPTION 'elected-winner source policy mismatch';
 END IF;
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=predecessor_id ORDER BY revision DESC LIMIT 1;
 IF NOT FOUND OR intent->>'reward_decision_digest' IS DISTINCT FROM decision->>'decision_sha256' THEN
  RAISE EXCEPTION 'source must bind the current elected reward decision';
 END IF;
 IF decision->>'selected_job_id' IS NULL THEN
  IF intent->>'kind' IS DISTINCT FROM 'reviewed_source_carry_forward' OR intent->>'reason' IS DISTINCT FROM 'no_reward_winner'
   OR COALESCE(intent->'winner_review','null'::jsonb)<>'null'::jsonb THEN
   RAISE EXCEPTION 'only an exhausted eligible ranking permits elected-winner carry-forward';
  END IF;
  RETURN true;
 END IF;
 SELECT * INTO review FROM competition_integrity_review WHERE review_id=(intent->>'review_id')::uuid;
 IF NOT FOUND OR intent->>'kind' IS DISTINCT FROM 'reviewed_source_promotion' OR review.round_id IS DISTINCT FROM predecessor_id
  OR review.job_id::text IS DISTINCT FROM decision->>'selected_job_id'
  OR review.review_json->'request'->>'reward_disposition' NOT IN ('approved','restored')
  OR review.review_json->'request'->>'source_disposition' IS DISTINCT FROM 'approved'
  OR review.review_json->'request'->>'reviewer_model' IS DISTINCT FROM 'gpt-6.1-sol'
  OR review.review_json->'request'->>'reviewer_reasoning' IS DISTINCT FROM 'max' THEN
  RAISE EXCEPTION 'source promotion requires the genuinely reviewed elected winner';
 END IF;
 checkpoint:=intent->'winner_review';
 SELECT transition_json->'winner_review' INTO actual_checkpoint FROM competition_supervisor_transition
  WHERE environment=intent->>'environment' AND competition_id=intent->>'competition_id' AND season_id=intent->>'season_id'
   AND target_epoch=(intent->>'target_epoch')::integer ORDER BY revision DESC LIMIT 1;
 IF checkpoint IS NULL OR checkpoint IS DISTINCT FROM actual_checkpoint OR checkpoint->>'round_id' IS DISTINCT FROM predecessor_id::text
  OR checkpoint->>'job_id' IS DISTINCT FROM decision->>'selected_job_id'
  OR checkpoint->>'reward_decision_sha256' IS DISTINCT FROM review.review_json->'request'->>'expected_decision_sha256'
  OR checkpoint->>'reward_revision' IS DISTINCT FROM review.review_json->'request'->>'expected_revision'
  OR NOT COALESCE(review.created_at>=(checkpoint->>'started_at')::timestamp AND review.created_at<(checkpoint->>'deadline')::timestamp,false)
  OR NOT COALESCE((intent->>'build_ready_at')::timestamp<=(intent->>'prepared_at')::timestamp,false) THEN
  RAISE EXCEPTION 'winner source lacks the durable in-deadline review and completed build';
 END IF;
 RETURN true;
END $winner_source$;

CREATE FUNCTION competition_v2_winner_review_guard() RETURNS trigger LANGUAGE plpgsql AS $winner_review$
DECLARE policy jsonb; request jsonb; decision jsonb; checkpoint jsonb; epoch integer;
BEGIN
 SELECT policy_json->'automation_policy',epoch_number INTO policy,epoch FROM competition_round WHERE round_id=NEW.round_id;
 IF COALESCE(policy->>'winner_review_policy','')='' THEN RETURN NEW; END IF;
 request:=NEW.review_json->'request';
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=NEW.round_id ORDER BY revision DESC LIMIT 1;
 IF request->>'reward_disposition'='restored' THEN
  IF request->>'reviewer_model' IS DISTINCT FROM 'gpt-6.1-sol' OR request->>'reviewer_reasoning' IS DISTINCT FROM 'max'
   OR request->>'expected_decision_sha256' IS DISTINCT FROM decision->>'decision_sha256'
   OR request->>'source_disposition' IS DISTINCT FROM 'approved' OR COALESCE(request->>'appeal_id','')=''
   OR NOT EXISTS(SELECT 1 FROM competition_integrity_review old_review WHERE old_review.review_id=(request->>'supersedes_review_id')::uuid
    AND old_review.round_id=NEW.round_id AND old_review.job_id=NEW.job_id AND old_review.review_json->'request'->>'reward_disposition'='disqualified') THEN
   RAISE EXCEPTION 'restoration requires genuine append-only appeal authority';
  END IF;
  RETURN NEW;
 END IF;
 SELECT transition_json->'winner_review' INTO checkpoint FROM competition_supervisor_transition
  WHERE environment=policy->>'environment' AND competition_id=(SELECT competition_id FROM competition_round WHERE round_id=NEW.round_id)
   AND season_id=policy->>'season_id' AND target_epoch=epoch+1 ORDER BY revision DESC LIMIT 1;
 IF policy->>'winner_review_policy' IS DISTINCT FROM 'elected_winner_sol61max_v1'
  OR request->>'reviewer_model' IS DISTINCT FROM 'gpt-6.1-sol' OR request->>'reviewer_reasoning' IS DISTINCT FROM 'max'
  OR request->>'expected_decision_sha256' IS DISTINCT FROM decision->>'decision_sha256'
  OR request->>'job_id' IS DISTINCT FROM decision->>'selected_job_id'
  OR request->>'job_id' IS DISTINCT FROM checkpoint->>'job_id'
  OR request->>'expected_revision' IS DISTINCT FROM checkpoint->>'reward_revision'
  OR request->>'expected_decision_sha256' IS DISTINCT FROM checkpoint->>'reward_decision_sha256'
  OR checkpoint IS NULL OR NOT COALESCE(NEW.created_at>=(checkpoint->>'started_at')::timestamp AND NEW.created_at<(checkpoint->>'deadline')::timestamp,false) THEN
  RAISE EXCEPTION 'review must bind the current elected winner and its durable review deadline';
 END IF;
 RETURN NEW;
END $winner_review$;
CREATE TRIGGER competition_v2_winner_review_before_insert BEFORE INSERT ON competition_integrity_review
 FOR EACH ROW EXECUTE FUNCTION competition_v2_winner_review_guard();

CREATE FUNCTION competition_v2_winner_checkpoint_guard() RETURNS trigger LANGUAGE plpgsql AS $winner_checkpoint$
DECLARE policy jsonb; predecessor_id uuid; decision jsonb; prior jsonb; checkpoint jsonb; publication_at timestamp;
BEGIN
 SELECT r.policy_json->'automation_policy',r.round_id,(s.seal_json->>'publication_at')::timestamp INTO policy,predecessor_id,publication_at
  FROM competition_round r LEFT JOIN competition_evaluation_seal s USING(round_id)
  WHERE r.competition_id=NEW.competition_id AND r.epoch_number=NEW.target_epoch-1
   AND r.policy_json->'automation_policy'->>'environment'=NEW.environment
   AND r.policy_json->'automation_policy'->>'season_id'=NEW.season_id;
 IF COALESCE(policy->>'winner_review_policy','')='' THEN RETURN NEW; END IF;
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=predecessor_id ORDER BY revision DESC LIMIT 1;
 SELECT transition_json->'winner_review' INTO prior FROM competition_supervisor_transition
  WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND target_epoch=NEW.target_epoch ORDER BY revision DESC LIMIT 1;
 checkpoint:=NULLIF(NEW.transition_json->'winner_review','null'::jsonb);
 IF checkpoint IS NULL THEN
  IF prior IS NOT NULL AND decision->>'selected_job_id' IS NOT NULL THEN RAISE EXCEPTION 'elected review checkpoint cannot be cleared while a winner remains'; END IF;
  RETURN NEW;
 END IF;
 IF prior->>'job_id'=checkpoint->>'job_id' AND prior->>'reward_decision_sha256'=checkpoint->>'reward_decision_sha256' AND prior IS DISTINCT FROM checkpoint THEN
  RAISE EXCEPTION 'unchanged elected winner review deadline cannot reset';
 END IF;
 IF checkpoint->>'round_id' IS DISTINCT FROM predecessor_id::text OR checkpoint->>'job_id' IS DISTINCT FROM decision->>'selected_job_id'
  OR checkpoint->>'reward_revision' IS DISTINCT FROM decision->>'revision'
  OR checkpoint->>'reward_decision_sha256' IS DISTINCT FROM decision->>'decision_sha256'
  OR NOT COALESCE((checkpoint->>'deadline')::timestamp=(checkpoint->>'started_at')::timestamp+make_interval(secs=>(policy->>'winner_review_timeout_seconds')::integer)
   AND (checkpoint->>'started_at')::timestamp>=publication_at AND (checkpoint->>'started_at')::timestamp<=NEW.updated_at+interval '1 millisecond',false) THEN
  RAISE EXCEPTION 'elected review checkpoint must bind the published decision and frozen timeout';
 END IF;
 RETURN NEW;
END $winner_checkpoint$;
CREATE TRIGGER competition_v2_winner_checkpoint_before_insert BEFORE INSERT ON competition_supervisor_transition
 FOR EACH ROW EXECUTE FUNCTION competition_v2_winner_checkpoint_guard();
`
