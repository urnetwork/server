// Failed unpublished promotions can be replaced without rewriting their recovery history.
package server

const competitionAutomationFallbackMigration = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE competition_source_activation ADD COLUMN abandoned_at timestamp NULL;
DO $constraints$
DECLARE target_constraint text;
BEGIN
 FOR target_constraint IN SELECT conname FROM pg_constraint WHERE conrelid='competition_source_activation'::regclass AND contype='u' LOOP
  EXECUTE format('ALTER TABLE competition_source_activation DROP CONSTRAINT %I',target_constraint);
 END LOOP;
END $constraints$;
CREATE UNIQUE INDEX competition_source_target_active ON competition_source_activation(environment,competition_id,season_id,target_epoch) WHERE abandoned_at IS NULL;
CREATE TABLE competition_source_abandonment (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 generation bigint NOT NULL, abandonment_json jsonb NOT NULL, created_at timestamp NOT NULL,
 PRIMARY KEY(environment,competition_id,season_id,generation),
 FOREIGN KEY(environment,competition_id,season_id,generation) REFERENCES competition_source_activation(environment,competition_id,season_id,generation)
);
CREATE TRIGGER competition_v2_abandonment_immutable BEFORE UPDATE OR DELETE ON competition_source_abandonment FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();

CREATE FUNCTION competition_v2_source_abandonment_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
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
  OR EXISTS(SELECT 1 FROM competition_round WHERE competition_id=NEW.competition_id AND NOT staging AND epoch_number=(intent->>'target_epoch')::integer) THEN
  RAISE EXCEPTION 'v2 fallback must retain an exact verified-unpublished unactivated promotion';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_abandonment_identity BEFORE INSERT ON competition_source_abandonment FOR EACH ROW EXECUTE FUNCTION competition_v2_source_abandonment_guard();

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
   AND competition_v2_score_eligible(e->'score',(s.seal_json->>'baseline_raw_score')::double precision,(s.seal_json->>'takeover_margin')::double precision)) THEN
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

CREATE FUNCTION competition_v2_source_abandonment_committed_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation AND abandoned_at=NEW.created_at) THEN
  RAISE EXCEPTION 'v2 abandonment audit and marker must commit atomically';
 END IF;
 RETURN NEW;
END $guard$;
CREATE CONSTRAINT TRIGGER competition_v2_abandonment_committed AFTER INSERT ON competition_source_abandonment DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION competition_v2_source_abandonment_committed_guard();

CREATE FUNCTION competition_v2_source_not_abandoned_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF EXISTS(SELECT 1 FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation AND abandoned_at IS NOT NULL) THEN
  RAISE EXCEPTION 'v2 abandoned source cannot receive readiness';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_readiness_not_abandoned BEFORE INSERT ON competition_source_readiness FOR EACH ROW EXECUTE FUNCTION competition_v2_source_not_abandoned_guard();

CREATE FUNCTION competition_v2_review_decision_committed_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE elected uuid; selected uuid; decision_action text;
BEGIN
 SELECT (decision_json->>'selected_job_id')::uuid,decision_json->>'action' INTO elected,decision_action FROM competition_reward_decision WHERE round_id=NEW.round_id ORDER BY revision DESC LIMIT 1;
 IF EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=NEW.round_id AND invalidates_evaluation) THEN
  IF decision_action IS DISTINCT FROM 'invalidated' OR elected IS NOT NULL THEN RAISE EXCEPTION 'v2 invalidation requires its null-recipient revision'; END IF;
  RETURN NEW;
 END IF;
 SELECT (e->>'job_id')::uuid INTO selected FROM competition_evaluation_seal s,jsonb_array_elements(s.seal_json->'entries') e
 WHERE s.round_id=NEW.round_id AND COALESCE((e->>'eligible')::boolean,false)
 AND COALESCE((SELECT r.review_json->'request'->>'reward_disposition' FROM competition_integrity_review r
  WHERE r.round_id=NEW.round_id AND r.job_id=(e->>'job_id')::uuid AND r.review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY r.review_cursor DESC LIMIT 1),'unchanged')<>'disqualified'
 ORDER BY (e->>'rank')::integer LIMIT 1;
 IF elected IS DISTINCT FROM selected THEN RAISE EXCEPTION 'v2 review and replacement election must commit atomically'; END IF;
 RETURN NEW;
END $guard$;
CREATE CONSTRAINT TRIGGER competition_v2_review_decision_committed AFTER INSERT ON competition_integrity_review DEFERRABLE INITIALLY DEFERRED
 FOR EACH ROW EXECUTE FUNCTION competition_v2_review_decision_committed_guard();

ALTER TABLE competition_integrity_review ALTER COLUMN review_cursor DROP DEFAULT;
CREATE FUNCTION competition_v2_ledger_scope_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE epoch_round competition_round%ROWTYPE;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 PERFORM competition_v2_scope_lock(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
 IF TG_TABLE_NAME='competition_integrity_review' THEN NEW.review_cursor:=nextval('competition_integrity_review_review_cursor_seq'); END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_review_aa_scope BEFORE INSERT ON competition_integrity_review FOR EACH ROW EXECUTE FUNCTION competition_v2_ledger_scope_guard();
CREATE TRIGGER competition_v2_decision_aa_scope BEFORE INSERT ON competition_reward_decision FOR EACH ROW EXECUTE FUNCTION competition_v2_ledger_scope_guard();

CREATE FUNCTION competition_v2_worker_write_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE automated boolean;
BEGIN
 SELECT COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false) INTO automated FROM competition_round WHERE round_id=NEW.round_id;
 IF automated AND current_setting('urnetwork.competition_v2_worker',true) IS DISTINCT FROM 'on' THEN
  IF (TG_OP='INSERT' AND NEW.state<>'queued') OR (TG_OP='UPDATE' AND OLD.state IN ('queued','running') AND
   (OLD.state IS DISTINCT FROM NEW.state OR OLD.lease_owner IS DISTINCT FROM NEW.lease_owner OR OLD.lease_expires_at IS DISTINCT FROM NEW.lease_expires_at
    OR OLD.score_json IS DISTINCT FROM NEW.score_json OR OLD.completed_at IS DISTINCT FROM NEW.completed_at)) THEN
   RAISE EXCEPTION 'legacy worker cannot complete or change v2 execution state';
  END IF;
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_worker_execution BEFORE INSERT OR UPDATE ON competition_job FOR EACH ROW EXECUTE FUNCTION competition_v2_worker_write_guard();
`
