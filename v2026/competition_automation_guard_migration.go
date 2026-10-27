// Database capability fences independently enforce v2 source authority and event ordering.
package server

const competitionAutomationGuardMigration = `
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION competition_v2_scope_lock(env text, competition text, season text)
RETURNS void LANGUAGE sql AS $lock$
 SELECT pg_advisory_xact_lock(hashtextextended('competition-automation:'||env||':'||competition||':'||season,0))
$lock$;

CREATE FUNCTION competition_v2_require_source_lease(env text, competition text, season text)
RETURNS void LANGUAGE plpgsql AS $lease$
BEGIN
 PERFORM competition_v2_scope_lock(env,competition,season);
 IF NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=env AND competition_id=competition AND season_id=season
  AND owner_id=current_setting('urnetwork.competition_v2_owner',true)
  AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true)
  AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'v2 source transition requires the current fenced supervisor lease';
 END IF;
 IF COALESCE((SELECT active FROM competition_automation_hold WHERE environment=env AND competition_id=competition AND season_id=season AND kind='source_activation' ORDER BY revision DESC LIMIT 1),false) THEN
  RAISE EXCEPTION 'v2 source activation is held';
 END IF;
END $lease$;

CREATE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text)
RETURNS void LANGUAGE plpgsql AS $source$
DECLARE previous jsonb; previous_generation bigint; review competition_integrity_review%ROWTYPE;
 source_review_id uuid; reward_disposition text; evaluated_round competition_round%ROWTYPE;
 candidate_score jsonb; recommendation double precision; choice_at timestamp; predecessor_id uuid; lock_content text;
BEGIN
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
 IF NOT FOUND OR (intent->>'target_epoch')::integer IS DISTINCT FROM (previous->>'target_epoch')::integer+1 OR intent->>'base_source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256' THEN
  RAISE EXCEPTION 'v2 source transition does not extend its active predecessor';
 END IF;
 SELECT (s.seal_json->>'source_choice_at')::timestamp,r.round_id INTO choice_at,predecessor_id FROM competition_evaluation_seal s JOIN competition_round r USING(round_id)
 WHERE r.competition_id=competition AND NOT r.staging AND r.epoch_number=(intent->>'target_epoch')::integer-1
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
  IF evaluated_round.competition_id IS DISTINCT FROM competition OR evaluated_round.policy_json->'automation_policy'->>'environment' IS DISTINCT FROM env
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
  recommendation:=GREATEST((previous->>'threshold')::double precision,(candidate_score->'significance'->>'recommended_next_epoch_takeover_margin_percent')::double precision/100);
  IF NOT COALESCE((candidate_score->'significance'->>'recommended_next_epoch_takeover_margin_supported')::boolean,false)
   OR (intent->>'threshold')::double precision IS DISTINCT FROM recommendation
   OR intent->>'threshold_evidence_sha256' IS DISTINCT FROM review.review_json->'request'->>'score_sha256'
   OR ((intent->'commits')-'server') IS DISTINCT FROM ((previous->'commits')-'server') THEN
   RAISE EXCEPTION 'v2 promotion changed protected dependencies or transplanted threshold evidence';
  END IF;
 ELSE RAISE EXCEPTION 'unsupported v2 source decision'; END IF;
END $source$;

CREATE FUNCTION competition_v2_source_insert_or_activate_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 IF NEW.generation IS DISTINCT FROM (NEW.intent_json->>'generation')::bigint OR NEW.target_epoch IS DISTINCT FROM (NEW.intent_json->>'target_epoch')::integer
  OR NEW.intent_sha256 IS DISTINCT FROM NEW.intent_json->>'intent_sha256' THEN
  RAISE EXCEPTION 'v2 source columns disagree with immutable intent';
 END IF;
 IF TG_OP='INSERT' AND NEW.activated_at IS NOT NULL THEN RAISE EXCEPTION 'v2 source must be staged before activation'; END IF;
 PERFORM competition_v2_source_authorized(NEW.intent_json,NEW.environment,NEW.competition_id,NEW.season_id);
 IF TG_OP='UPDATE' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL AND
  (SELECT count(*) FROM competition_source_readiness r WHERE r.environment=NEW.environment AND r.competition_id=NEW.competition_id AND r.season_id=NEW.season_id AND r.generation=NEW.generation
   AND r.receipt_json->>'manifest_sha256'=NEW.intent_json->>'manifest_sha256' AND r.receipt_json->>'source_lock_sha256'=NEW.intent_json->>'source_lock_sha256'
   AND (r.receipt_json->>'ready')::boolean)<>2 THEN
  RAISE EXCEPTION 'v2 source activation requires exact api and worker readiness receipts';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_source_authority BEFORE INSERT OR UPDATE ON competition_source_activation FOR EACH ROW EXECUTE FUNCTION competition_v2_source_insert_or_activate_guard();

CREATE FUNCTION competition_v2_source_readiness_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE intent jsonb;
BEGIN
 PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
 SELECT intent_json INTO intent FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation;
 IF NOT FOUND OR NEW.receipt_json->>'environment' IS DISTINCT FROM NEW.environment OR NEW.receipt_json->>'competition_id' IS DISTINCT FROM NEW.competition_id
  OR NEW.receipt_json->>'season_id' IS DISTINCT FROM NEW.season_id OR (NEW.receipt_json->>'generation')::bigint IS DISTINCT FROM NEW.generation
  OR NEW.receipt_json->>'role' IS DISTINCT FROM NEW.role OR COALESCE(NEW.receipt_json->>'actor_id','')=''
  OR NEW.receipt_json->>'manifest_commit' IS DISTINCT FROM intent->>'manifest_commit'
  OR NEW.receipt_json->>'manifest_sha256' IS DISTINCT FROM intent->>'manifest_sha256'
  OR NEW.receipt_json->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256'
  OR NEW.receipt_json->>'qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR COALESCE(NEW.receipt_json->>'runtime_image_digest','') !~ '^sha256:[0-9a-f]{64}$' OR NOT COALESCE((NEW.receipt_json->>'ready')::boolean,false) THEN
  RAISE EXCEPTION 'v2 readiness receipt does not attest the exact source manifest';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_readiness_identity BEFORE INSERT ON competition_source_readiness FOR EACH ROW EXECUTE FUNCTION competition_v2_source_readiness_guard();

CREATE FUNCTION competition_v2_source_pointer_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE intent jsonb; previous_generation bigint;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'v2 source pointer cannot be deleted'; END IF;
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 IF TG_OP='UPDATE' AND (OLD.environment IS DISTINCT FROM NEW.environment OR OLD.competition_id IS DISTINCT FROM NEW.competition_id OR OLD.season_id IS DISTINCT FROM NEW.season_id) THEN
  RAISE EXCEPTION 'v2 activation pointer scope is immutable';
 END IF;
 SELECT intent_json INTO intent FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation AND activated_at IS NOT NULL;
 SELECT generation INTO previous_generation FROM competition_source_pointer WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id FOR UPDATE;
 IF intent IS NULL OR (intent->>'expected_generation')::bigint IS DISTINCT FROM COALESCE(previous_generation,0) THEN
  RAISE EXCEPTION 'v2 activation pointer must compare-and-swap its exact ready predecessor';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_pointer_authority BEFORE INSERT OR UPDATE OR DELETE ON competition_source_pointer FOR EACH ROW EXECUTE FUNCTION competition_v2_source_pointer_guard();

CREATE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE policy jsonb; intent jsonb; activated timestamp; expected_epoch integer;
BEGIN
 policy:=NEW.policy_json->'automation_policy';
 IF NOT COALESCE((policy->>'enabled')::boolean,false) THEN RETURN NEW; END IF;
 PERFORM competition_v2_require_source_lease(policy->>'environment',NEW.competition_id,policy->>'season_id');
 SELECT a.intent_json,a.activated_at INTO intent,activated FROM competition_source_pointer p JOIN competition_source_activation a USING(environment,competition_id,season_id,generation)
 WHERE p.environment=policy->>'environment' AND p.competition_id=NEW.competition_id AND p.season_id=policy->>'season_id';
 IF NOT FOUND OR activated IS NULL OR NEW.staging OR NEW.finalized_at IS NOT NULL OR NEW.winner_job_id IS NOT NULL
  OR (policy->>'version')::integer IS DISTINCT FROM 2 OR policy->>'required_api_capability' IS DISTINCT FROM 'competition.reward.v2'
  OR (policy->>'source_activation_generation')::bigint IS DISTINCT FROM (intent->>'generation')::bigint
  OR (policy->>'source_generation')::integer IS DISTINCT FROM (intent->>'source_generation')::integer
  OR policy->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256'
  OR NEW.policy_json->>'base_sha' IS DISTINCT FROM intent->'commits'->>'server'
  OR NEW.policy_json->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluator_image_digest'
  OR (NEW.policy_json->'evaluation_policy'->>'takeover_margin')::double precision IS DISTINCT FROM (intent->>'threshold')::double precision
  OR NEW.policy_json->'evaluation_policy'->>'host_qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR NEW.epoch_number IS DISTINCT FROM (intent->>'target_epoch')::integer
  OR NEW.closes_at-NEW.opens_at<>interval '7 days' OR NEW.reveal_at<NEW.closes_at OR NEW.opens_at<activated
  OR NEW.opens_at<(intent->>'prepared_at')::timestamp+interval '16 hours' THEN
  RAISE EXCEPTION 'v2 round requires the exact activated source and qualified full-window schedule';
 END IF;
 SELECT COALESCE(max(epoch_number),0)+1 INTO expected_epoch FROM competition_round WHERE competition_id=NEW.competition_id AND NOT staging;
 IF NEW.epoch_number<>expected_epoch OR NEW.epoch_number>6 OR (NEW.epoch_number>1 AND NOT EXISTS(
  SELECT 1 FROM competition_round r WHERE r.competition_id=NEW.competition_id AND NOT r.staging AND r.epoch_number=NEW.epoch_number-1
   AND NOT EXISTS(SELECT 1 FROM competition_automation_incident i WHERE i.round_id=r.round_id AND i.invalidates_evaluation)
   AND (r.finalized_at IS NOT NULL OR EXISTS(SELECT 1 FROM competition_evaluation_seal s WHERE s.round_id=r.round_id)))) THEN
  RAISE EXCEPTION 'v2 round predecessor is not sealed or season is complete';
 END IF;
 IF COALESCE((SELECT active FROM competition_automation_hold WHERE environment=policy->>'environment' AND competition_id=NEW.competition_id AND season_id=policy->>'season_id' AND kind='admission' ORDER BY revision DESC LIMIT 1),false) THEN
  RAISE EXCEPTION 'v2 admission is held';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_round_source BEFORE INSERT ON competition_round FOR EACH ROW EXECUTE FUNCTION competition_v2_round_insert_guard();

CREATE FUNCTION competition_v2_review_identity_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE seal jsonb; entry jsonb; request jsonb; previous competition_integrity_review%ROWTYPE; reward_revision integer; round_policy jsonb;
BEGIN
 SELECT policy_json INTO round_policy FROM competition_round WHERE round_id=NEW.round_id;
 PERFORM competition_v2_scope_lock(round_policy->'automation_policy'->>'environment',round_policy->>'competition_id',round_policy->'automation_policy'->>'season_id');
 PERFORM 1 FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 request:=NEW.review_json->'request';
 SELECT value INTO entry FROM jsonb_array_elements(seal->'entries') WHERE (value->>'job_id')::uuid=NEW.job_id;
 SELECT COALESCE(max(revision),0) INTO reward_revision FROM competition_reward_decision WHERE round_id=NEW.round_id;
 IF NEW.actor_id IS DISTINCT FROM current_setting('urnetwork.competition_v2_review_actor',true) OR entry IS NULL OR entry->>'state'<>'succeeded' OR (request->>'expected_revision')::integer IS DISTINCT FROM reward_revision
  OR (request->>'job_id')::uuid IS DISTINCT FROM NEW.job_id OR (NEW.review_json->>'round_id')::uuid IS DISTINCT FROM NEW.round_id
  OR (NEW.review_json->>'review_id')::uuid IS DISTINCT FROM NEW.review_id OR NEW.review_json->>'actor_id' IS DISTINCT FROM NEW.actor_id
  OR request->>'idempotency_key' IS DISTINCT FROM NEW.idempotency_key OR NEW.review_json->>'request_sha256' IS DISTINCT FROM NEW.request_sha256
  OR request->>'patch_sha256' IS DISTINCT FROM entry->>'patch_sha256' OR request->>'base_source_lock_sha256' IS DISTINCT FROM entry->>'base_source_lock_sha256'
  OR request->>'score_sha256' IS DISTINCT FROM entry->>'score_sha256' OR request->>'artifact_manifest_sha256' IS DISTINCT FROM entry->>'artifact_manifest_sha256'
  OR request->>'baseline_sha256' IS DISTINCT FROM seal->>'baseline_sha256' OR request->>'criteria_version' IS DISTINCT FROM round_policy->'automation_policy'->>'review_criteria_version'
  OR COALESCE(request->>'reward_disposition','') NOT IN ('approved','disqualified','restored','unchanged') OR COALESCE(request->>'source_disposition','') NOT IN ('approved','rejected','deferred','unchanged')
  OR jsonb_array_length(request->'findings')<>6
  OR (SELECT count(DISTINCT f->>'criterion') FROM jsonb_array_elements(request->'findings') f WHERE f->>'criterion' IN ('measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety'))<>6
  OR octet_length(NEW.review_json::text)>131072 THEN
  RAISE EXCEPTION 'v2 review must bind exact sealed evidence and current reward revision';
 END IF;
 SELECT * INTO previous FROM competition_integrity_review WHERE round_id=NEW.round_id AND job_id=NEW.job_id AND review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1;
 IF (request->>'reward_disposition'='approved' AND previous.review_json->'request'->>'reward_disposition'='disqualified') OR
  (request->>'reward_disposition'='restored' AND (previous.review_json->'request'->>'reward_disposition' IS DISTINCT FROM 'disqualified'
   OR (request->>'supersedes_review_id')::uuid IS DISTINCT FROM previous.review_id OR COALESCE(request->>'appeal_id','')='')) THEN
  RAISE EXCEPTION 'v2 restoration requires an explicit appeal of the latest reward finding';
 END IF;
 IF request->>'source_disposition'='approved' AND request->>'reward_disposition' NOT IN ('approved','restored') THEN
  RAISE EXCEPTION 'v2 source approval requires an affirmative integrity finding';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_review_identity BEFORE INSERT ON competition_integrity_review FOR EACH ROW EXECUTE FUNCTION competition_v2_review_identity_guard();

ALTER TABLE competition_reward_outbox ALTER COLUMN event_cursor DROP DEFAULT;
CREATE FUNCTION competition_v2_outbox_identity_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE decision jsonb; publication timestamp;
BEGIN
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=NEW.round_id AND revision=NEW.revision;
 PERFORM competition_v2_scope_lock(decision->>'environment',decision->>'competition_id',decision->>'season_id');
 SELECT publication_at INTO publication FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 IF decision IS NULL OR NEW.payload_json IS DISTINCT FROM decision OR NEW.publication_at IS DISTINCT FROM publication THEN
  RAISE EXCEPTION 'v2 outbox event differs from immutable decision or publication boundary';
 END IF;
 NEW.event_cursor:=nextval('competition_reward_outbox_event_cursor_seq');
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_outbox_identity BEFORE INSERT ON competition_reward_outbox FOR EACH ROW EXECUTE FUNCTION competition_v2_outbox_identity_guard();

CREATE FUNCTION competition_v2_decision_outbox_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM competition_reward_outbox WHERE round_id=NEW.round_id AND revision=NEW.revision) THEN
  RAISE EXCEPTION 'v2 reward decision and durable event must commit atomically';
 END IF;
 RETURN NEW;
END $guard$;
CREATE CONSTRAINT TRIGGER competition_v2_decision_outbox AFTER INSERT ON competition_reward_decision
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION competition_v2_decision_outbox_guard();

CREATE FUNCTION competition_v2_receipt_identity_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
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
  OR NEW.created_at<event.publication_at THEN
  RAISE EXCEPTION 'v2 settlement receipt does not bind its scoped published event';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_receipt_identity BEFORE INSERT ON competition_settlement_receipt FOR EACH ROW EXECUTE FUNCTION competition_v2_receipt_identity_guard();

CREATE FUNCTION competition_v2_seal_control_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE baseline_raw double precision; baseline_digest text; round_policy jsonb;
BEGIN
 SELECT policy_json INTO round_policy FROM competition_round WHERE round_id=NEW.round_id;
 PERFORM competition_v2_scope_lock(round_policy->'automation_policy'->>'environment',round_policy->>'competition_id',round_policy->'automation_policy'->>'season_id');
 SELECT baseline_sha256 INTO baseline_digest FROM competition_round_baseline WHERE round_id=NEW.round_id;
 SELECT percentile_disc(0.5) WITHIN GROUP(ORDER BY (r->>'raw_score')::double precision) INTO baseline_raw
 FROM competition_round_baseline b,jsonb_array_elements(convert_from(b.baseline_json,'UTF8')::jsonb->'replicates') r WHERE b.round_id=NEW.round_id;
 IF (NEW.seal_json->>'baseline_raw_score')::double precision IS DISTINCT FROM COALESCE(baseline_raw,0)
  OR NEW.seal_json->>'baseline_sha256' IS DISTINCT FROM baseline_digest
  OR (NEW.seal_json->>'takeover_margin')::double precision IS DISTINCT FROM (round_policy->'evaluation_policy'->>'takeover_margin')::double precision THEN
  RAISE EXCEPTION 'v2 seal control median and margin must match immutable scoring evidence';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_seal_control BEFORE INSERT ON competition_evaluation_seal FOR EACH ROW EXECUTE FUNCTION competition_v2_seal_control_guard();
`
