// Prospective staging retires one unused repository; historical production locks and applied migrations stay unchanged.
package server

const competitionAutomationStagingSourceGraphMigration = `
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION competition_v2_source_graph_valid(commits jsonb, staging_mode boolean)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $graph$
BEGIN
 IF jsonb_typeof(commits) IS DISTINCT FROM 'object' OR staging_mode IS NULL THEN RETURN false; END IF;
 RETURN (SELECT count(*) FROM jsonb_object_keys(commits))=(CASE WHEN staging_mode THEN 10 ELSE 11 END)
  AND commits ?& ARRAY['server','connect','sdk','proxy','glog','goidenticons','userwireguard','sn','warp','gvisor']
  AND (commits ? 'operator-proxy')=(NOT staging_mode)
  AND NOT EXISTS(SELECT 1 FROM jsonb_each_text(commits) c WHERE c.value IS NULL OR c.value !~ '^[0-9a-f]{40}$');
END $graph$;

CREATE OR REPLACE FUNCTION competition_v2_source_mode(intent jsonb) RETURNS void LANGUAGE plpgsql AS $mode$
DECLARE staging_mode boolean; window_seconds integer; preparation_seconds integer;
BEGIN
 staging_mode:=COALESCE((intent->>'staging')::boolean,false);
 IF NOT competition_v2_source_graph_valid(intent->'commits',staging_mode) THEN
  RAISE EXCEPTION 'v2 source mode repository graph disagrees with its explicit staging flag';
 END IF;
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
  OR NOT competition_v2_source_graph_valid(intent->'commits',COALESCE((intent->>'staging')::boolean,false)) THEN
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
  OR (SELECT count(*) FROM jsonb_object_keys(intent->'git_refs'))<>(CASE WHEN COALESCE((intent->>'staging')::boolean,false) THEN 11 ELSE 12 END)
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
`
