// Prospective explicit shadow staging does not reinterpret any historical round or source.
package server

const competitionAutomationStagingMigration = `
SET LOCAL lock_timeout = '5s';

DROP INDEX competition_round_epoch_identity;
CREATE UNIQUE INDEX competition_round_epoch_identity ON competition_round(competition_id,staging,epoch_number)
 WHERE NOT staging OR NOT COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false);
CREATE UNIQUE INDEX competition_round_v2_staging_epoch ON competition_round(competition_id,
 (policy_json->'automation_policy'->>'environment'),(policy_json->'automation_policy'->>'season_id'),epoch_number)
 WHERE staging AND COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false);
DROP INDEX competition_round_one_active_staging;
CREATE UNIQUE INDEX competition_round_one_active_staging ON competition_round(competition_id)
 WHERE staging AND NOT canceled AND CASE WHEN COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false)
  THEN automation_sealed_at IS NULL ELSE finalized_at IS NULL END;

CREATE FUNCTION competition_v2_source_mode(intent jsonb) RETURNS void LANGUAGE plpgsql AS $mode$
DECLARE staging_mode boolean; window_seconds integer; preparation_seconds integer;
BEGIN
 staging_mode:=COALESCE((intent->>'staging')::boolean,false);
 window_seconds:=COALESCE((intent->>'staging_round_window_seconds')::integer,0);
 preparation_seconds:=COALESCE((intent->>'staging_preparation_seconds')::integer,0);
 IF staging_mode IS DISTINCT FROM COALESCE(nullif(current_setting('urnetwork.competition_v2_staging',true),'')::boolean,false)
  OR window_seconds IS DISTINCT FROM COALESCE(nullif(current_setting('urnetwork.competition_v2_staging_window',true),'')::integer,0)
  OR preparation_seconds IS DISTINCT FROM COALESCE(nullif(current_setting('urnetwork.competition_v2_staging_preparation',true),'')::integer,0)
  OR (staging_mode AND (window_seconds<60 OR window_seconds>604800 OR preparation_seconds<0 OR preparation_seconds>57600 OR
    (intent->>'threshold')::double precision IS DISTINCT FROM 0::double precision))
  OR (NOT staging_mode AND (window_seconds<>0 OR preparation_seconds<>0 OR NOT COALESCE((intent->>'threshold')::double precision>0,false)))
  OR EXISTS(SELECT 1 FROM jsonb_each(intent->'git_refs') r WHERE
    (staging_mode AND ((r.key='config' AND r.value->>'branch' IS DISTINCT FROM 'main') OR
      (r.key<>'config' AND r.value->>'branch' IS DISTINCT FROM 'sim-latency-staging'))) OR
    (NOT staging_mode AND r.value->>'branch'='sim-latency-staging')) THEN
  RAISE EXCEPTION 'v2 source mode, isolated branch or frozen staging schedule disagrees with its configured authority';
 END IF;
END $mode$;

CREATE FUNCTION competition_v2_score_eligible_mode(score jsonb, baseline_raw double precision, margin double precision, staging_mode boolean)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $eligible$
 SELECT CASE WHEN NOT staging_mode THEN competition_v2_score_eligible(score,baseline_raw,margin) ELSE COALESCE(
  margin=0 AND baseline_raw>0 AND baseline_raw<'Infinity'::double precision
  AND score @> '{"placeable":true,"significance":{"replicate_count":9,"takeover_margin_percent":0}}'::jsonb
  AND score->'gates' ?& ARRAY['G1_success','G2_volume','G3_path_integrity','G4_matchmaking','G5_stability','G6_resources']
  AND NOT EXISTS(SELECT 1 FROM jsonb_each(score->'gates') gate WHERE NOT COALESCE((gate.value->>'passed')::boolean,false))
  AND (score->>'raw_score')::double precision>0 AND (score->>'raw_score')::double precision<'Infinity'::double precision,false) END
$eligible$;

CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text)
RETURNS void LANGUAGE plpgsql AS $source$
DECLARE previous jsonb; previous_generation bigint; review competition_integrity_review%ROWTYPE;
 source_review_id uuid; reward_disposition text; evaluated_round competition_round%ROWTYPE;
 candidate_score jsonb; recommendation double precision; choice_at timestamp; predecessor_id uuid; lock_content text;
BEGIN
 PERFORM competition_v2_source_mode(intent);
 SELECT generation INTO previous_generation FROM competition_source_pointer WHERE environment=env AND competition_id=competition AND season_id=season;
 IF COALESCE(previous_generation,0) IS DISTINCT FROM (intent->>'expected_generation')::bigint THEN
  RAISE EXCEPTION 'v2 source pointer compare-and-swap conflict';
 END IF;
 IF intent->>'environment' IS DISTINCT FROM env OR intent->>'competition_id' IS DISTINCT FROM competition OR intent->>'season_id' IS DISTINCT FROM season
  OR NOT COALESCE((intent->>'generation')::bigint>COALESCE(previous_generation,0),false)
  OR (intent->>'generation')::bigint IS DISTINCT FROM (SELECT COALESCE(max(generation),0)+1 FROM competition_source_activation
   WHERE environment=env AND competition_id=competition AND season_id=season AND generation<(intent->>'generation')::bigint)
  OR jsonb_typeof(intent->'commits') IS DISTINCT FROM 'object'
  OR (SELECT count(*) FROM jsonb_object_keys(intent->'commits'))<>11
  OR NOT COALESCE(intent->'commits' ?& ARRAY['server','connect','sdk','proxy','glog','goidenticons','userwireguard','sn','operator-proxy','warp','gvisor'],false)
  OR EXISTS(SELECT 1 FROM jsonb_each_text(intent->'commits') c WHERE c.value !~ '^[0-9a-f]{40}$') THEN
  RAISE EXCEPTION 'v2 source scope or complete repository identity is invalid';
 END IF;
 SELECT '{"schema":1,"development_snapshot":false,"repositories":{'||string_agg(to_json(c.key)::text||':'||to_json(c.value)::text,',' ORDER BY c.key)||'}}'
 INTO lock_content FROM jsonb_each_text(intent->'commits') c;
 IF intent->>'source_lock_sha256' IS DISTINCT FROM encode(sha256(convert_to(lock_content,'UTF8')),'hex')
  OR intent->>'manifest_sha256' IS DISTINCT FROM encode(sha256(decode(intent->>'manifest','base64')),'hex')
  OR COALESCE(intent->>'manifest','')=''
  OR COALESCE(intent->>'manifest_commit','') !~ '^[0-9a-f]{40}$'
  OR COALESCE(intent->>'config_expected_commit','') !~ '^[0-9a-f]{40}$'
  OR COALESCE(intent->>'source_decision_digest','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'publication_journal_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'threshold_evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'evaluator_image_digest','') !~ '^sha256:[0-9a-f]{64}$'
  OR COALESCE(intent->>'host_qualification_sha256','') !~ '^[0-9a-f]{64}$'
  OR NOT COALESCE((intent->>'threshold')::double precision>=0 AND (intent->>'threshold')::double precision<1,false)
  OR NOT COALESCE((intent->>'prepared_at')::timestamp>'1970-01-01'::timestamp,false)
  OR jsonb_typeof(intent->'git_refs') IS DISTINCT FROM 'object'
  OR (SELECT count(*) FROM jsonb_object_keys(intent->'git_refs'))<>12
  OR EXISTS(SELECT 1 FROM jsonb_each_text(intent->'commits') c WHERE
   intent->'git_refs'->c.key->>'commit' IS DISTINCT FROM c.value
   OR COALESCE(intent->'git_refs'->c.key->>'expected_commit','') !~ '^[0-9a-f]{40}$')
  OR EXISTS(SELECT 1 FROM jsonb_each(intent->'git_refs') r WHERE COALESCE(r.value->>'branch','')='' OR r.value->>'branch' LIKE '-%' OR strpos(r.value->>'branch','..')>0
   OR r.value->>'branch' ~ '[[:space:]~^:?*]' OR strpos(r.value->>'branch','[')>0 OR strpos(r.value->>'branch',E'\\\\')>0)
  OR intent->'git_refs'->'config'->>'commit' IS DISTINCT FROM intent->>'manifest_commit'
  OR intent->'git_refs'->'config'->>'expected_commit' IS DISTINCT FROM intent->>'config_expected_commit' THEN
  RAISE EXCEPTION 'v2 source content or publication recovery identity is invalid';
 END IF;
 IF previous_generation IS NULL THEN
  IF intent->>'kind' IS DISTINCT FROM 'baseline' OR (intent->>'target_epoch')::integer IS DISTINCT FROM 1 OR (intent->>'source_generation')::integer IS DISTINCT FROM 0
   OR intent->>'source_lock_sha256' IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_lock',true)
   OR intent->'commits'->>'server' IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_base',true)
   OR (intent->>'threshold')::double precision IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_margin',true)::double precision
   OR COALESCE(current_setting('urnetwork.competition_v2_policy_approval',true),'') !~ '^[0-9a-f]{64}$' THEN
   RAISE EXCEPTION 'v2 baseline must match prospectively approved bootstrap configuration';
  END IF;
  RETURN;
 END IF;
 SELECT intent_json INTO previous FROM competition_source_activation WHERE environment=env AND competition_id=competition AND season_id=season AND generation=previous_generation AND activated_at IS NOT NULL;
 IF NOT FOUND OR COALESCE((intent->>'staging')::boolean,false) IS DISTINCT FROM COALESCE((previous->>'staging')::boolean,false)
  OR COALESCE((intent->>'staging_round_window_seconds')::integer,0) IS DISTINCT FROM COALESCE((previous->>'staging_round_window_seconds')::integer,0)
  OR COALESCE((intent->>'staging_preparation_seconds')::integer,0) IS DISTINCT FROM COALESCE((previous->>'staging_preparation_seconds')::integer,0)
  OR (intent->>'target_epoch')::integer IS DISTINCT FROM (previous->>'target_epoch')::integer+1 OR intent->>'base_source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256' THEN
  RAISE EXCEPTION 'v2 source transition does not extend its active predecessor';
 END IF;
 SELECT (s.seal_json->>'source_choice_at')::timestamp,r.round_id INTO choice_at,predecessor_id FROM competition_evaluation_seal s JOIN competition_round r USING(round_id)
 WHERE r.competition_id=competition AND r.staging=COALESCE((intent->>'staging')::boolean,false) AND r.epoch_number=(intent->>'target_epoch')::integer-1
  AND r.policy_json->'automation_policy'->>'environment'=env AND r.policy_json->'automation_policy'->>'season_id'=season;
 IF NOT FOUND OR (intent->>'prepared_at')::timestamp<choice_at OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=predecessor_id AND invalidates_evaluation) THEN
  RAISE EXCEPTION 'v2 source choice requires the preceding immutable seal and deadline';
 END IF;
 IF intent->>'kind'='reviewed_source_carry_forward' THEN
  IF intent->'commits' IS DISTINCT FROM previous->'commits' OR intent->>'source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256'
   OR intent->'source_generation' IS DISTINCT FROM previous->'source_generation' OR intent->'threshold' IS DISTINCT FROM previous->'threshold'
   OR intent->>'threshold_evidence_sha256' IS DISTINCT FROM previous->>'threshold_evidence_sha256' OR intent->>'review_id' IS NOT NULL
   OR COALESCE(intent->>'reason','') NOT IN ('review_pending','promotion_not_ready','no_reward_winner','readiness_fallback') THEN
   RAISE EXCEPTION 'v2 carry-forward cannot alter reviewed source or its threshold';
  END IF;
 ELSIF intent->>'kind'='reviewed_source_promotion' THEN
  SELECT * INTO review FROM competition_integrity_review WHERE review_id=(intent->>'review_id')::uuid;
  IF NOT FOUND OR review.review_json->'request'->>'source_disposition' IS DISTINCT FROM 'approved'
   OR review.request_sha256 IS DISTINCT FROM intent->>'review_decision_digest'
   OR review.review_json->'request'->>'base_source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256'
   OR review.review_json->'request'->>'patch_sha256' IS DISTINCT FROM intent->>'approved_patch_sha256'
   OR review.created_at>choice_at OR NOT (intent->>'build_ready_at' IS NOT NULL AND (intent->>'build_ready_at')::timestamp<=choice_at)
   OR (intent->>'source_generation')::integer IS DISTINCT FROM (previous->>'source_generation')::integer+1
   OR intent->>'source_lock_sha256'=previous->>'source_lock_sha256' THEN
   RAISE EXCEPTION 'v2 promotion requires genuine exact-source approval ready by the deadline';
  END IF;
  SELECT * INTO evaluated_round FROM competition_round WHERE round_id=review.round_id;
  IF evaluated_round.staging IS DISTINCT FROM COALESCE((intent->>'staging')::boolean,false) OR evaluated_round.competition_id IS DISTINCT FROM competition OR evaluated_round.policy_json->'automation_policy'->>'environment' IS DISTINCT FROM env
   OR evaluated_round.policy_json->'automation_policy'->>'season_id' IS DISTINCT FROM season
   OR evaluated_round.policy_json->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluated_image_digest'
   OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=review.round_id AND invalidates_evaluation) THEN
   RAISE EXCEPTION 'v2 promotion evidence belongs to another or invalidated evaluation';
  END IF;
  SELECT review_id INTO source_review_id FROM competition_integrity_review WHERE round_id=review.round_id AND job_id=review.job_id
   AND review_json->'request'->>'source_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1;
  SELECT review_json->'request'->>'reward_disposition' INTO reward_disposition FROM competition_integrity_review WHERE round_id=review.round_id AND job_id=review.job_id
   AND review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1;
  IF source_review_id IS DISTINCT FROM review.review_id OR COALESCE(reward_disposition,'') NOT IN ('approved','restored') THEN
   RAISE EXCEPTION 'v2 source approval is no longer current';
  END IF;
  SELECT score_json INTO candidate_score FROM competition_job WHERE job_id=review.job_id AND round_id=review.round_id AND state='succeeded';
  recommendation:=CASE WHEN COALESCE((intent->>'staging')::boolean,false) THEN 0 ELSE GREATEST((previous->>'threshold')::double precision,(candidate_score->'significance'->>'recommended_next_epoch_takeover_margin_percent')::double precision/100) END;
  IF (NOT COALESCE((intent->>'staging')::boolean,false) AND NOT COALESCE((candidate_score->'significance'->>'recommended_next_epoch_takeover_margin_supported')::boolean,false))
   OR (intent->>'threshold')::double precision IS DISTINCT FROM recommendation
   OR intent->>'threshold_evidence_sha256' IS DISTINCT FROM review.review_json->'request'->>'score_sha256'
   OR ((intent->'commits')-'server') IS DISTINCT FROM ((previous->'commits')-'server') THEN
   RAISE EXCEPTION 'v2 promotion changed protected dependencies or transplanted threshold evidence';
  END IF;
 ELSE RAISE EXCEPTION 'unsupported v2 source decision'; END IF;
END $source$;

CREATE OR REPLACE FUNCTION competition_v2_source_insert_or_activate_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF TG_OP='UPDATE' AND OLD.abandoned_at IS NOT NULL THEN RAISE EXCEPTION 'v2 abandoned source intent is immutable'; END IF;
 IF TG_OP='UPDATE' AND OLD.abandoned_at IS NULL AND NEW.abandoned_at IS NOT NULL THEN
  PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
  IF NEW.activated_at IS NOT NULL OR NOT EXISTS(SELECT 1 FROM competition_source_abandonment a
   WHERE a.environment=NEW.environment AND a.competition_id=NEW.competition_id AND a.season_id=NEW.season_id AND a.generation=NEW.generation AND a.created_at=NEW.abandoned_at
    AND a.abandonment_json->>'actor_id'=current_setting('urnetwork.competition_v2_owner',true)
    AND a.abandonment_json->>'fencing_token'=current_setting('urnetwork.competition_v2_fence',true)) THEN
   RAISE EXCEPTION 'v2 abandonment requires its exact durable fenced proof';
  END IF;
  RETURN NEW;
 END IF;
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 IF NEW.generation IS DISTINCT FROM (NEW.intent_json->>'generation')::bigint OR NEW.target_epoch IS DISTINCT FROM (NEW.intent_json->>'target_epoch')::integer
  OR NEW.intent_sha256 IS DISTINCT FROM NEW.intent_json->>'intent_sha256' OR NEW.abandoned_at IS NOT NULL THEN
  RAISE EXCEPTION 'v2 source columns disagree with immutable intent';
 END IF;
 IF TG_OP='INSERT' AND NEW.activated_at IS NOT NULL THEN RAISE EXCEPTION 'v2 source must be staged before activation'; END IF;
 PERFORM competition_v2_source_authorized(NEW.intent_json,NEW.environment,NEW.competition_id,NEW.season_id);
 IF NEW.intent_json->>'kind'='reviewed_source_promotion' AND NOT EXISTS(
  SELECT 1 FROM competition_integrity_review r JOIN competition_evaluation_seal s USING(round_id),jsonb_array_elements(s.seal_json->'entries') e
  WHERE r.review_id=(NEW.intent_json->>'review_id')::uuid AND (e->>'job_id')::uuid=r.job_id AND COALESCE((e->>'eligible')::boolean,false)
   AND competition_v2_score_eligible_mode(e->'score',(s.seal_json->>'baseline_raw_score')::double precision,(s.seal_json->>'takeover_margin')::double precision,COALESCE((NEW.intent_json->>'staging')::boolean,false))) THEN
  RAISE EXCEPTION 'v2 source promotion requires an eligible immutable sealed candidate';
 END IF;
 IF TG_OP='UPDATE' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL AND
  (SELECT count(*) FROM competition_source_readiness r WHERE r.environment=NEW.environment AND r.competition_id=NEW.competition_id AND r.season_id=NEW.season_id AND r.generation=NEW.generation
   AND r.receipt_json->>'manifest_sha256'=NEW.intent_json->>'manifest_sha256' AND r.receipt_json->>'source_lock_sha256'=NEW.intent_json->>'source_lock_sha256'
   AND (r.receipt_json->>'ready')::boolean)<>2 THEN
  RAISE EXCEPTION 'v2 source activation requires exact api and worker readiness receipts';
 END IF;
 RETURN NEW;
END $guard$;

CREATE OR REPLACE FUNCTION competition_v2_seal_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE epoch_round competition_round%ROWTYPE; baseline_raw double precision; expected_count integer; entry jsonb; expected_eligible boolean;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 IF NOT FOUND OR epoch_round.staging IS DISTINCT FROM COALESCE((epoch_round.policy_json->'automation_policy'->>'staging')::boolean,false)
 OR epoch_round.staging IS DISTINCT FROM COALESCE((NEW.seal_json->>'staging')::boolean,false) OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR
 NOT COALESCE((epoch_round.policy_json->'automation_policy'->>'enabled')::boolean,false) OR NEW.sealed_at < epoch_round.closes_at OR NEW.publication_at < epoch_round.reveal_at OR
 EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND state IN ('queued','running')) THEN
  RAISE EXCEPTION 'v2 seal requires closed fully drained exact-mode round';
 END IF;
 SELECT count(*) INTO expected_count FROM competition_job WHERE round_id=NEW.round_id;
 IF jsonb_array_length(NEW.seal_json->'entries') <> expected_count OR
 (SELECT count(DISTINCT e->>'job_id') FROM jsonb_array_elements(NEW.seal_json->'entries') e) <> expected_count THEN
  RAISE EXCEPTION 'v2 seal inventory is incomplete';
 END IF;
 IF EXISTS(SELECT 1 FROM (
   SELECT j.job_id,row_number() OVER(ORDER BY (j.score_json->>'raw_score')::double precision ASC NULLS LAST,j.submitted_at,j.job_id) AS rank
   FROM competition_job j WHERE j.round_id=NEW.round_id) ranked
   JOIN jsonb_array_elements(NEW.seal_json->'entries') e ON (e->>'job_id')::uuid=ranked.job_id
   WHERE (e->>'rank')::integer<>ranked.rank) THEN
  RAISE EXCEPTION 'v2 sealed ranking differs from exact raw order';
 END IF;
 SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY (r->>'raw_score')::double precision) INTO baseline_raw
 FROM competition_round_baseline b, jsonb_array_elements(convert_from(b.baseline_json,'UTF8')::jsonb->'replicates') r WHERE b.round_id=NEW.round_id;
 FOR entry IN SELECT value FROM jsonb_array_elements(NEW.seal_json->'entries') LOOP
  IF NOT EXISTS(SELECT 1 FROM competition_job j WHERE j.round_id=NEW.round_id AND j.job_id=(entry->>'job_id')::uuid
    AND j.state=entry->>'state' AND j.patch_sha256=entry->>'patch_sha256'
    AND j.completed_at=(entry->>'completed_at')::timestamptz AND j.submitted_at=(entry->>'submitted_at')::timestamptz
    AND (j.score_json IS NOT DISTINCT FROM entry->'score')) THEN
   RAISE EXCEPTION 'v2 sealed evidence differs from terminal job';
  END IF;
  expected_eligible := entry->>'state'='succeeded' AND competition_v2_score_eligible_mode(entry->'score',baseline_raw,(epoch_round.policy_json->'evaluation_policy'->>'takeover_margin')::double precision,epoch_round.staging);
  IF expected_eligible AND epoch_round.policy_json->'automation_policy'->>'repeat_reward_policy'='exclude_exact_identity' AND EXISTS(
   SELECT 1 FROM competition_reward_decision d JOIN competition_job j ON j.job_id=(d.decision_json->>'selected_job_id')::uuid
   JOIN competition_round r ON r.round_id=d.round_id WHERE r.competition_id=epoch_round.competition_id
   AND r.policy_json->'automation_policy'->>'environment'=epoch_round.policy_json->'automation_policy'->>'environment'
   AND r.policy_json->'automation_policy'->>'season_id'=epoch_round.policy_json->'automation_policy'->>'season_id'
   AND r.policy_json->'automation_policy'->>'source_lock_sha256'=epoch_round.policy_json->'automation_policy'->>'source_lock_sha256'
   AND j.patch_sha256=entry->>'patch_sha256') THEN expected_eligible:=false; END IF;
  IF COALESCE((entry->>'eligible')::boolean,false) IS DISTINCT FROM expected_eligible THEN
   RAISE EXCEPTION 'v2 sealed eligibility contradicts scorer evidence';
  END IF;
 END LOOP;
 RETURN NEW;
END $guard$;

CREATE OR REPLACE FUNCTION competition_v2_source_abandonment_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE intent jsonb; activated timestamp; abandoned timestamp; pointer_generation bigint;
BEGIN
 PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
 IF NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id
  AND owner_id=current_setting('urnetwork.competition_v2_owner',true) AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'v2 source abandonment requires the current fenced supervisor';
 END IF;
 SELECT intent_json,activated_at,abandoned_at INTO intent,activated,abandoned FROM competition_source_activation
 WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation FOR UPDATE;
 SELECT generation INTO pointer_generation FROM competition_source_pointer WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id;
 IF intent IS NULL OR intent->>'kind' IS DISTINCT FROM 'reviewed_source_promotion' OR activated IS NOT NULL OR abandoned IS NOT NULL
  OR pointer_generation IS DISTINCT FROM (intent->>'expected_generation')::bigint
  OR NEW.abandonment_json->>'environment' IS DISTINCT FROM NEW.environment OR NEW.abandonment_json->>'competition_id' IS DISTINCT FROM NEW.competition_id
  OR NEW.abandonment_json->>'season_id' IS DISTINCT FROM NEW.season_id OR (NEW.abandonment_json->>'generation')::bigint IS DISTINCT FROM NEW.generation
  OR NEW.abandonment_json->>'intent_sha256' IS DISTINCT FROM intent->>'intent_sha256'
  OR NEW.abandonment_json->>'actor_id' IS DISTINCT FROM current_setting('urnetwork.competition_v2_owner',true)
  OR NEW.abandonment_json->>'fencing_token' IS DISTINCT FROM current_setting('urnetwork.competition_v2_fence',true)
  OR (NEW.abandonment_json->>'created_at')::timestamp IS DISTINCT FROM NEW.created_at
  OR COALESCE(NEW.abandonment_json->>'reason','') NOT IN ('publication_failed','readiness_failed','build_failed','source_approval_revoked')
  OR COALESCE(NEW.abandonment_json->>'unpublished_proof_sha256','') !~ '^[0-9a-f]{64}$'
  OR EXISTS(SELECT 1 FROM competition_source_readiness WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation)
  OR EXISTS(SELECT 1 FROM competition_round WHERE competition_id=NEW.competition_id AND staging=COALESCE((intent->>'staging')::boolean,false) AND epoch_number=(intent->>'target_epoch')::integer
   AND policy_json->'automation_policy'->>'environment'=NEW.environment AND policy_json->'automation_policy'->>'season_id'=NEW.season_id) THEN
  RAISE EXCEPTION 'v2 fallback must retain an exact verified-unpublished unactivated promotion';
 END IF;
 RETURN NEW;
END $guard$;

CREATE OR REPLACE FUNCTION competition_v2_receipt_identity_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE event competition_reward_outbox%ROWTYPE; receipt jsonb;
BEGIN
 SELECT * INTO event FROM competition_reward_outbox WHERE event_id=NEW.event_id;
 receipt:=NEW.receipt_json;
 IF NOT FOUND OR NEW.actor_id IS DISTINCT FROM current_setting('urnetwork.competition_v2_settlement_actor',true)
  OR NEW.actor_id IS DISTINCT FROM receipt->>'actor_id' OR NEW.status IS DISTINCT FROM receipt->>'status'
  OR NEW.payload_sha256 IS DISTINCT FROM event.payload_sha256 OR NEW.payload_sha256 IS DISTINCT FROM receipt->>'payload_sha256'
  OR (receipt->>'event_id')::uuid IS DISTINCT FROM NEW.event_id OR (receipt->>'round_id')::uuid IS DISTINCT FROM event.round_id
  OR (receipt->>'revision')::integer IS DISTINCT FROM event.revision
  OR receipt->>'decision_sha256' IS DISTINCT FROM event.payload_json->>'decision_sha256'
  OR receipt->>'environment' IS DISTINCT FROM event.payload_json->>'environment'
  OR receipt->>'competition_id' IS DISTINCT FROM event.payload_json->>'competition_id'
  OR receipt->>'season_id' IS DISTINCT FROM event.payload_json->>'season_id'
  OR (COALESCE((event.payload_json->>'staging')::boolean,false) AND
   (event.payload_json->>'settlement_mode' IS DISTINCT FROM 'shadow' OR COALESCE(receipt->>'chain_transaction','')<>'' OR receipt->>'applied_at' IS NOT NULL OR NEW.status NOT IN ('received','rejected')))
  OR NEW.created_at<event.publication_at THEN
  RAISE EXCEPTION 'v2 settlement receipt does not bind its scoped published event';
 END IF;
 RETURN NEW;
END $guard$;

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
  OR (policy->>'version')::integer IS DISTINCT FROM 2 OR policy->>'required_api_capability' IS DISTINCT FROM 'competition.reward.v2'
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

CREATE FUNCTION competition_v2_readiness_mode_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE intent jsonb;
BEGIN
 SELECT intent_json INTO intent FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation;
 IF intent IS NULL OR COALESCE((NEW.receipt_json->>'staging')::boolean,false) IS DISTINCT FROM COALESCE((intent->>'staging')::boolean,false) THEN
  RAISE EXCEPTION 'v2 readiness mode differs from its exact source intent';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_readiness_mode BEFORE INSERT ON competition_source_readiness FOR EACH ROW EXECUTE FUNCTION competition_v2_readiness_mode_guard();

CREATE FUNCTION competition_v2_pointer_mode_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE intent jsonb; previous jsonb;
BEGIN
 SELECT intent_json INTO intent FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation;
 IF intent IS NULL THEN RAISE EXCEPTION 'v2 source pointer mode is unavailable'; END IF;
 PERFORM competition_v2_source_mode(intent);
 IF TG_OP='UPDATE' THEN
  SELECT intent_json INTO previous FROM competition_source_activation WHERE environment=OLD.environment AND competition_id=OLD.competition_id AND season_id=OLD.season_id AND generation=OLD.generation;
  IF COALESCE((intent->>'staging')::boolean,false) IS DISTINCT FROM COALESCE((previous->>'staging')::boolean,false) THEN
   RAISE EXCEPTION 'v2 source pointer cannot change execution mode';
  END IF;
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_pointer_mode BEFORE INSERT OR UPDATE ON competition_source_pointer FOR EACH ROW EXECUTE FUNCTION competition_v2_pointer_mode_guard();

CREATE FUNCTION competition_v2_decision_mode_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE epoch_round competition_round%ROWTYPE; seal jsonb;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 IF COALESCE((NEW.decision_json->>'staging')::boolean,false) IS DISTINCT FROM epoch_round.staging
  OR COALESCE((seal->>'staging')::boolean,false) IS DISTINCT FROM epoch_round.staging
  OR (epoch_round.staging AND (NEW.decision_json->>'settlement_mode' IS DISTINCT FROM 'shadow'
   OR epoch_round.policy_json->'automation_policy'->>'settlement_mode' IS DISTINCT FROM 'shadow')) THEN
  RAISE EXCEPTION 'v2 reward decision cannot change its sealed staging or settlement mode';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_decision_mode BEFORE INSERT ON competition_reward_decision FOR EACH ROW EXECUTE FUNCTION competition_v2_decision_mode_guard();

CREATE OR REPLACE FUNCTION competition_v2_round_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF COALESCE((OLD.policy_json->'automation_policy'->>'enabled')::boolean,false) THEN
  IF NEW.admission_closed_at IS DISTINCT FROM OLD.admission_closed_at THEN
   RAISE EXCEPTION 'v2 admission must retain its frozen full window';
  END IF;
  IF NEW.finalized_at IS NOT NULL OR NEW.winner_job_id IS NOT NULL THEN
   RAISE EXCEPTION 'v2 must not manufacture legacy finalization';
  END IF;
  IF (OLD.automation_sealed_at IS NOT NULL AND
     (OLD.automation_sealed_at IS DISTINCT FROM NEW.automation_sealed_at OR OLD.automation_publication_at IS DISTINCT FROM NEW.automation_publication_at)) OR
     ((NEW.automation_sealed_at IS DISTINCT FROM OLD.automation_sealed_at OR NEW.automation_publication_at IS DISTINCT FROM OLD.automation_publication_at) AND
     NOT EXISTS(SELECT 1 FROM competition_evaluation_seal WHERE round_id=NEW.round_id AND sealed_at=NEW.automation_sealed_at AND publication_at=NEW.automation_publication_at)) THEN
   RAISE EXCEPTION 'v2 publication requires its immutable seal';
  END IF;
 END IF;
 RETURN NEW;
END $guard$;
`
