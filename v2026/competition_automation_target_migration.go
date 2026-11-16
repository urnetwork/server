// Prospective round identities allow genuine qualification before admission without a scoring baseline.
package server

const competitionAutomationTargetMigration = `
SET LOCAL lock_timeout = '5s';

CREATE UNIQUE INDEX competition_source_reserved_round ON competition_source_activation((intent_json->>'target_round_id'))
 WHERE intent_json ? 'target_round_id' AND intent_json->>'target_round_id'<>'00000000-0000-0000-0000-000000000000';

CREATE FUNCTION competition_v2_source_target_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE source_review competition_integrity_review%ROWTYPE;
BEGIN
 IF TG_OP='UPDATE' AND NEW.abandoned_at IS NOT NULL THEN RETURN NEW; END IF;
 IF COALESCE(NEW.intent_json->>'target_round_id','') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR NEW.intent_json->>'target_round_id'='00000000-0000-0000-0000-000000000000' THEN
  RAISE EXCEPTION 'v2 source must reserve its exact future round identity before qualification';
 END IF;
 IF TG_OP='INSERT' AND EXISTS(SELECT 1 FROM competition_round WHERE round_id=(NEW.intent_json->>'target_round_id')::uuid) THEN
  RAISE EXCEPTION 'v2 source cannot reinterpret an already committed round';
 END IF;
 IF NEW.intent_json->>'kind'='reviewed_source_promotion' THEN
  SELECT * INTO source_review FROM competition_integrity_review WHERE review_id=(NEW.intent_json->>'review_id')::uuid;
  IF NOT FOUND OR EXISTS(SELECT 1 FROM competition_integrity_review r WHERE r.round_id=source_review.round_id AND r.job_id=source_review.job_id
   AND r.review_json->'request'->>'reward_disposition'='disqualified' AND r.review_cursor>=source_review.review_cursor) THEN
   RAISE EXCEPTION 'v2 disqualification revokes prior source approval; a fresh explicit source approval is required';
  END IF;
 END IF;
 IF TG_OP='UPDATE' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL AND
  (SELECT count(*) FROM competition_source_readiness r WHERE r.environment=NEW.environment AND r.competition_id=NEW.competition_id AND r.season_id=NEW.season_id AND r.generation=NEW.generation
   AND r.receipt_json->>'target_round_id'=NEW.intent_json->>'target_round_id' AND COALESCE((r.receipt_json->>'ready')::boolean,false))<>2 THEN
  RAISE EXCEPTION 'v2 activation requires API and worker qualification for the exact reserved round';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_source_target BEFORE INSERT OR UPDATE ON competition_source_activation FOR EACH ROW EXECUTE FUNCTION competition_v2_source_target_guard();

CREATE FUNCTION competition_v2_readiness_target_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE target_id text;
BEGIN
 SELECT intent_json->>'target_round_id' INTO target_id FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation;
 IF target_id IS NULL OR target_id='00000000-0000-0000-0000-000000000000' OR NEW.receipt_json->>'target_round_id' IS DISTINCT FROM target_id THEN
  RAISE EXCEPTION 'v2 readiness must name the exact prospectively reserved round';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_readiness_target BEFORE INSERT ON competition_source_readiness FOR EACH ROW EXECUTE FUNCTION competition_v2_readiness_target_guard();

CREATE FUNCTION competition_v2_round_reserved_identity_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE policy jsonb; target_id uuid;
BEGIN
 policy:=NEW.policy_json->'automation_policy';
 IF NOT COALESCE((policy->>'enabled')::boolean,false) THEN RETURN NEW; END IF;
 PERFORM competition_v2_scope_lock(policy->>'environment',NEW.competition_id,policy->>'season_id');
 SELECT (intent_json->>'target_round_id')::uuid INTO target_id FROM competition_source_activation
 WHERE environment=policy->>'environment' AND competition_id=NEW.competition_id AND season_id=policy->>'season_id'
  AND generation=(policy->>'source_activation_generation')::bigint AND activated_at IS NOT NULL AND abandoned_at IS NULL;
 IF target_id IS NULL OR target_id='00000000-0000-0000-0000-000000000000'::uuid OR NEW.round_id IS DISTINCT FROM target_id THEN
  RAISE EXCEPTION 'v2 round identity must be reserved before source preparation and qualification';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_round_reserved_identity BEFORE INSERT ON competition_round FOR EACH ROW EXECUTE FUNCTION competition_v2_round_reserved_identity_guard();
`
