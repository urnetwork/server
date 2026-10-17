// Additive v2 competition guards leave every historical migration and winner unchanged.
package server

const competitionAutomationMigration = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE competition_round
 ADD COLUMN automation_sealed_at timestamp NULL,
 ADD COLUMN automation_publication_at timestamp NULL;

CREATE TABLE competition_evaluation_seal (
 round_id uuid PRIMARY KEY REFERENCES competition_round(round_id),
 sealed_at timestamp NOT NULL, publication_at timestamp NOT NULL,
 seal_sha256 char(64) NOT NULL CHECK (seal_sha256 ~ '^[0-9a-f]{64}$'),
 seal_json jsonb NOT NULL CHECK (jsonb_typeof(seal_json) = 'object'),
 CHECK (sealed_at <= publication_at)
);
CREATE TABLE competition_reward_decision (
 round_id uuid NOT NULL REFERENCES competition_evaluation_seal(round_id),
 revision integer NOT NULL CHECK (revision > 0),
 decision_sha256 char(64) NOT NULL UNIQUE CHECK (decision_sha256 ~ '^[0-9a-f]{64}$'),
 previous_decision_sha256 varchar(64) NOT NULL,
 decision_json jsonb NOT NULL, created_at timestamp NOT NULL,
 PRIMARY KEY (round_id, revision)
);
CREATE TABLE competition_integrity_review (
 review_cursor bigserial UNIQUE NOT NULL,
 review_id uuid PRIMARY KEY, round_id uuid NOT NULL REFERENCES competition_evaluation_seal(round_id),
 job_id uuid NOT NULL REFERENCES competition_job(job_id), actor_id varchar(128) NOT NULL,
 idempotency_key varchar(128) NOT NULL, request_sha256 char(64) NOT NULL,
 review_json jsonb NOT NULL, created_at timestamp NOT NULL,
 UNIQUE (round_id, actor_id, idempotency_key)
);
CREATE INDEX competition_integrity_review_latest ON competition_integrity_review(round_id, job_id, review_cursor DESC);
CREATE TABLE competition_reward_outbox (
 event_cursor bigserial UNIQUE NOT NULL, event_id uuid PRIMARY KEY,
 round_id uuid NOT NULL, revision integer NOT NULL,
 payload_sha256 char(64) NOT NULL, payload_json jsonb NOT NULL,
 publication_at timestamp NOT NULL, created_at timestamp NOT NULL,
 UNIQUE (round_id, revision),
 FOREIGN KEY (round_id, revision) REFERENCES competition_reward_decision(round_id, revision)
);
CREATE TABLE competition_settlement_receipt (
 event_id uuid NOT NULL REFERENCES competition_reward_outbox(event_id),
 actor_id varchar(128) NOT NULL, status varchar(32) NOT NULL,
 payload_sha256 char(64) NOT NULL, receipt_json jsonb NOT NULL, created_at timestamp NOT NULL,
 PRIMARY KEY (event_id, actor_id, status),
 CHECK (status IN ('received','applied','confirmed','rejected'))
);
CREATE TABLE competition_source_activation (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 generation bigint NOT NULL CHECK (generation > 0), target_epoch integer NOT NULL CHECK(target_epoch BETWEEN 1 AND 6),
 intent_sha256 char(64) NOT NULL, intent_json jsonb NOT NULL, created_at timestamp NOT NULL,
 activated_at timestamp NULL,
 PRIMARY KEY (environment, competition_id, season_id, generation),
 UNIQUE (environment, competition_id, season_id, target_epoch)
);
CREATE TABLE competition_source_readiness (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 generation bigint NOT NULL, role varchar(16) NOT NULL CHECK(role IN ('api','worker')),
 receipt_json jsonb NOT NULL, created_at timestamp NOT NULL,
 PRIMARY KEY (environment, competition_id, season_id, generation, role),
 FOREIGN KEY (environment, competition_id, season_id, generation)
 REFERENCES competition_source_activation(environment, competition_id, season_id, generation)
);
CREATE TABLE competition_source_pointer (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 generation bigint NOT NULL,
 PRIMARY KEY (environment, competition_id, season_id),
 FOREIGN KEY (environment, competition_id, season_id, generation)
 REFERENCES competition_source_activation(environment, competition_id, season_id, generation)
);
CREATE TABLE competition_supervisor_lease (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 owner_id varchar(128) NOT NULL, fencing_token bigint NOT NULL CHECK(fencing_token > 0),
 lease_expires_at timestamp NOT NULL, heartbeat_at timestamp NOT NULL,
 PRIMARY KEY (environment, competition_id, season_id)
);
CREATE TABLE competition_supervisor_transition (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 target_epoch integer NOT NULL CHECK(target_epoch BETWEEN 1 AND 7), revision integer NOT NULL CHECK(revision > 0),
 state varchar(64) NOT NULL, transition_json jsonb NOT NULL, updated_at timestamp NOT NULL,
 PRIMARY KEY (environment, competition_id, season_id, target_epoch, revision)
);

CREATE FUNCTION competition_v2_score_eligible(score jsonb, baseline_raw double precision, margin double precision)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $eligible$
 SELECT COALESCE(
  score @> '{"placeable":true,"takeover_eligible":true,"significance":{"statistically_significant":true,"recommended_next_epoch_takeover_margin_supported":true,"replicate_count":9}}'::jsonb
  AND score->'gates' ?& ARRAY['G1_success','G2_volume','G3_path_integrity','G4_matchmaking','G5_stability','G6_resources']
  AND NOT EXISTS (SELECT 1 FROM jsonb_each(score->'gates') gate WHERE NOT COALESCE((gate.value->>'passed')::boolean,false))
  AND (score->>'raw_score')::double precision > 0
  AND (score->>'raw_score')::double precision <= baseline_raw * (1-margin)
  AND abs((score->'significance'->>'takeover_margin_percent')::double precision - margin*100) <= 0.000000001,
 false)
$eligible$;

CREATE FUNCTION competition_v2_evidence_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE target_round uuid; epoch_round competition_round%ROWTYPE;
BEGIN
 target_round := CASE WHEN TG_OP = 'DELETE' THEN OLD.round_id ELSE NEW.round_id END;
 SELECT * INTO epoch_round FROM competition_round WHERE round_id = target_round FOR UPDATE;
 IF COALESCE((epoch_round.policy_json->'automation_policy'->>'enabled')::boolean,false) THEN
  IF EXISTS(SELECT 1 FROM competition_evaluation_seal WHERE round_id = target_round) THEN
   RAISE EXCEPTION 'v2 sealed evaluation evidence is immutable';
  END IF;
  IF TG_TABLE_NAME = 'competition_job' AND TG_OP <> 'DELETE' THEN
   IF NEW.state = 'running' AND current_setting('urnetwork.competition_v2_worker',true) IS DISTINCT FROM 'on' THEN
    RAISE EXCEPTION 'legacy worker cannot claim v2 work';
   END IF;
  END IF;
 END IF;
 IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_job_fence BEFORE INSERT OR UPDATE OR DELETE ON competition_job
 FOR EACH ROW EXECUTE FUNCTION competition_v2_evidence_guard();
CREATE TRIGGER competition_v2_baseline_fence BEFORE INSERT ON competition_round_baseline
 FOR EACH ROW EXECUTE FUNCTION competition_v2_evidence_guard();

CREATE FUNCTION competition_v2_round_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF COALESCE((OLD.policy_json->'automation_policy'->>'enabled')::boolean,false) THEN
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
CREATE TRIGGER competition_v2_round_fence BEFORE UPDATE ON competition_round FOR EACH ROW EXECUTE FUNCTION competition_v2_round_guard();

CREATE FUNCTION competition_v2_seal_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE epoch_round competition_round%ROWTYPE; baseline_raw double precision; expected_count integer; entry jsonb; expected_eligible boolean;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 IF NOT FOUND OR epoch_round.staging OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR
 NOT COALESCE((epoch_round.policy_json->'automation_policy'->>'enabled')::boolean,false) OR NEW.sealed_at < epoch_round.closes_at OR NEW.publication_at < epoch_round.reveal_at OR
 EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND state IN ('queued','running')) THEN
  RAISE EXCEPTION 'v2 seal requires closed fully drained production round';
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
  expected_eligible := entry->>'state'='succeeded' AND competition_v2_score_eligible(entry->'score',baseline_raw,(epoch_round.policy_json->'evaluation_policy'->>'takeover_margin')::double precision);
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
CREATE TRIGGER competition_v2_seal_insert BEFORE INSERT ON competition_evaluation_seal FOR EACH ROW EXECUTE FUNCTION competition_v2_seal_guard();

CREATE FUNCTION competition_v2_decision_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE prior competition_reward_decision%ROWTYPE; chosen uuid; seal jsonb;
BEGIN
 PERFORM 1 FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT * INTO prior FROM competition_reward_decision WHERE round_id=NEW.round_id ORDER BY revision DESC LIMIT 1;
 IF NEW.revision <> COALESCE(prior.revision,0)+1 OR NEW.previous_decision_sha256 <> COALESCE(prior.decision_sha256,'') THEN
  RAISE EXCEPTION 'v2 reward revision predecessor conflict';
 END IF;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 SELECT (e->>'job_id')::uuid INTO chosen FROM jsonb_array_elements(seal->'entries') e
 WHERE (e->>'eligible')::boolean AND COALESCE((SELECT r.review_json->'request'->>'reward_disposition'
  FROM competition_integrity_review r WHERE r.round_id=NEW.round_id AND r.job_id=(e->>'job_id')::uuid
   AND r.review_json->'request'->>'reward_disposition' <> 'unchanged'
  ORDER BY review_cursor DESC LIMIT 1),'approved') <> 'disqualified'
 ORDER BY (e->>'rank')::integer LIMIT 1;
 IF chosen IS DISTINCT FROM (NEW.decision_json->>'selected_job_id')::uuid OR
 (NEW.decision_json->>'revision')::integer <> NEW.revision OR NEW.decision_json->>'seal_sha256' <> seal->>'seal_sha256' THEN
  RAISE EXCEPTION 'v2 reward recipient differs from immutable eligible ranking';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_decision_insert BEFORE INSERT ON competition_reward_decision FOR EACH ROW EXECUTE FUNCTION competition_v2_decision_guard();

CREATE FUNCTION competition_v2_activation_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF TG_OP='DELETE' OR OLD.environment IS DISTINCT FROM NEW.environment OR OLD.competition_id IS DISTINCT FROM NEW.competition_id OR
 OLD.season_id IS DISTINCT FROM NEW.season_id OR OLD.generation IS DISTINCT FROM NEW.generation OR OLD.target_epoch IS DISTINCT FROM NEW.target_epoch OR
 OLD.intent_json IS DISTINCT FROM NEW.intent_json OR OLD.intent_sha256 IS DISTINCT FROM NEW.intent_sha256 OR OLD.created_at IS DISTINCT FROM NEW.created_at OR
 (OLD.activated_at IS NOT NULL AND OLD.activated_at IS DISTINCT FROM NEW.activated_at) THEN
  RAISE EXCEPTION 'v2 source activation intent is immutable';
 END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER competition_v2_activation_immutable BEFORE UPDATE OR DELETE ON competition_source_activation FOR EACH ROW EXECUTE FUNCTION competition_v2_activation_guard();

CREATE TRIGGER competition_v2_seal_immutable BEFORE UPDATE OR DELETE ON competition_evaluation_seal FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_v2_decision_immutable BEFORE UPDATE OR DELETE ON competition_reward_decision FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_v2_review_immutable BEFORE UPDATE OR DELETE ON competition_integrity_review FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_v2_outbox_immutable BEFORE UPDATE OR DELETE ON competition_reward_outbox FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_v2_receipt_immutable BEFORE UPDATE OR DELETE ON competition_settlement_receipt FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_v2_readiness_immutable BEFORE UPDATE OR DELETE ON competition_source_readiness FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_v2_transition_immutable BEFORE UPDATE OR DELETE ON competition_supervisor_transition FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
`
