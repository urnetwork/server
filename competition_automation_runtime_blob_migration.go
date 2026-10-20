// Adds the independently verified runtime-blob capability prospectively.
// Historical migrations and frozen policies remain byte-for-byte unchanged.
package server

const competitionAutomationRuntimeBlobMigration = `
SET LOCAL lock_timeout = '5s';

CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE policy jsonb; intent jsonb; activated timestamp; expected_epoch integer; window_seconds integer; preparation_seconds integer;
BEGIN
 policy:=NEW.policy_json->'automation_policy';
 PERFORM pg_advisory_xact_lock(hashtextextended('competition-round-v1',0));
 IF NOT NEW.staging AND EXISTS(SELECT 1 FROM competition_round WHERE competition_id=NEW.competition_id AND staging AND NOT canceled
  AND COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false) AND automation_sealed_at IS NULL) THEN
  RAISE EXCEPTION 'v2 round cannot strand another active execution mode';
 END IF;
 IF NOT COALESCE((policy->>'enabled')::boolean,false) THEN RETURN NEW; END IF;
 IF NEW.staging AND EXISTS(SELECT 1 FROM competition_round WHERE competition_id=NEW.competition_id AND NOT staging) THEN
  RAISE EXCEPTION 'v2 round cannot strand another active execution mode';
 END IF;
 PERFORM competition_v2_require_source_lease(policy->>'environment',NEW.competition_id,policy->>'season_id');
 SELECT a.intent_json,a.activated_at INTO intent,activated FROM competition_source_pointer p JOIN competition_source_activation a USING(environment,competition_id,season_id,generation)
 WHERE p.environment=policy->>'environment' AND p.competition_id=NEW.competition_id AND p.season_id=policy->>'season_id' AND a.abandoned_at IS NULL;
 IF NOT FOUND OR activated IS NULL THEN RAISE EXCEPTION 'v2 round requires its exact active source pointer'; END IF;
 PERFORM competition_v2_source_mode(intent);
 window_seconds:=CASE WHEN NEW.staging THEN COALESCE((policy->>'staging_round_window_seconds')::integer,0) ELSE 604800 END;
 preparation_seconds:=CASE WHEN NEW.staging THEN COALESCE((policy->>'staging_preparation_seconds')::integer,0) ELSE 57600 END;
 IF NEW.staging IS DISTINCT FROM COALESCE((policy->>'staging')::boolean,false)
  OR NEW.staging IS DISTINCT FROM COALESCE((intent->>'staging')::boolean,false)
  OR COALESCE((policy->>'staging_round_window_seconds')::integer,0) IS DISTINCT FROM COALESCE((intent->>'staging_round_window_seconds')::integer,0)
  OR COALESCE((policy->>'staging_preparation_seconds')::integer,0) IS DISTINCT FROM COALESCE((intent->>'staging_preparation_seconds')::integer,0)
  OR NEW.finalized_at IS NOT NULL OR NEW.winner_job_id IS NOT NULL OR NEW.admission_closed_at IS NOT NULL
  OR (policy->>'version')::integer IS DISTINCT FROM 2 OR COALESCE(policy->>'required_api_capability','') NOT IN ('competition.reward.v2','competition.reward.v2.runtime-blob.v1')
  OR (policy->>'source_activation_generation')::bigint IS DISTINCT FROM (intent->>'generation')::bigint
  OR (policy->>'source_generation')::integer IS DISTINCT FROM (intent->>'source_generation')::integer
  OR policy->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256'
  OR NEW.policy_json->>'base_sha' IS DISTINCT FROM intent->'commits'->>'server'
  OR NEW.policy_json->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluator_image_digest'
  OR (NEW.policy_json->'evaluation_policy'->>'takeover_margin')::double precision IS DISTINCT FROM (intent->>'threshold')::double precision
  OR NEW.policy_json->'evaluation_policy'->>'host_qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR NEW.epoch_number IS DISTINCT FROM (intent->>'target_epoch')::integer
  OR NEW.closes_at-NEW.opens_at<>make_interval(secs=>window_seconds) OR NEW.reveal_at<NEW.closes_at OR NEW.opens_at<activated
  OR NEW.opens_at<(intent->>'prepared_at')::timestamp+make_interval(secs=>preparation_seconds)
  OR (NEW.staging AND (policy->>'settlement_mode' IS DISTINCT FROM 'shadow'
   OR (NEW.policy_json->'evaluation_policy'->>'takeover_margin')::double precision IS DISTINCT FROM 0::double precision
   OR (NEW.policy_json->'evaluation_policy'->>'replicates')::integer IS DISTINCT FROM 9
   OR (NEW.policy_json->'season_policy'->>'submission_fee_usd')::double precision IS DISTINCT FROM 0::double precision
   OR (NEW.policy_json->'season_policy'->>'epoch_count')::integer IS DISTINCT FROM 6
   OR (NEW.policy_json->'season_policy'->>'submission_window_seconds')::integer IS DISTINCT FROM window_seconds
   OR (NEW.policy_json->'season_policy'->>'preparation_window_seconds')::integer IS DISTINCT FROM preparation_seconds)) THEN
  RAISE EXCEPTION 'v2 round requires the exact activated mode, zero-fee shadow staging policy and qualified full-window schedule';
 END IF;
 SELECT COALESCE(max(epoch_number),0)+1 INTO expected_epoch FROM competition_round
 WHERE competition_id=NEW.competition_id AND staging=NEW.staging AND
  (NOT NEW.staging OR (COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false)
   AND policy_json->'automation_policy'->>'environment'=policy->>'environment' AND policy_json->'automation_policy'->>'season_id'=policy->>'season_id'));
 IF NEW.epoch_number<>expected_epoch OR NEW.epoch_number<1 OR NEW.epoch_number>6 OR (NEW.epoch_number>1 AND NOT EXISTS(
  SELECT 1 FROM competition_round r WHERE r.competition_id=NEW.competition_id AND r.staging=NEW.staging AND r.epoch_number=NEW.epoch_number-1
   AND r.policy_json->'automation_policy'->>'environment'=policy->>'environment' AND r.policy_json->'automation_policy'->>'season_id'=policy->>'season_id'
   AND NOT EXISTS(SELECT 1 FROM competition_automation_incident i WHERE i.round_id=r.round_id AND i.invalidates_evaluation)
   AND (r.finalized_at IS NOT NULL OR EXISTS(SELECT 1 FROM competition_evaluation_seal s WHERE s.round_id=r.round_id)))) THEN
  RAISE EXCEPTION 'v2 round predecessor is not sealed in the exact mode and season, or season is complete';
 END IF;
 IF COALESCE((SELECT active FROM competition_automation_hold WHERE environment=policy->>'environment' AND competition_id=NEW.competition_id AND season_id=policy->>'season_id' AND kind='admission' ORDER BY revision DESC LIMIT 1),false) THEN
  RAISE EXCEPTION 'v2 admission is held';
 END IF;
 RETURN NEW;
END $guard$;
`
