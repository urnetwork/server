// Independent holds and incidents preserve the historical score/election ledger.
package server

const competitionAutomationIncidentMigration = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE competition_automation_hold (
 hold_id uuid PRIMARY KEY,
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 kind varchar(32) NOT NULL CHECK(kind IN ('admission','evaluation','source_activation','reward_delivery')),
 revision integer NOT NULL CHECK(revision > 0), active boolean NOT NULL,
 actor_id varchar(128) NOT NULL, idempotency_key varchar(128) NOT NULL,
 request_sha256 char(64) NOT NULL, hold_json jsonb NOT NULL, created_at timestamp NOT NULL,
 UNIQUE(environment,competition_id,season_id,kind,revision),
 UNIQUE(environment,competition_id,season_id,actor_id,idempotency_key)
);
CREATE TABLE competition_automation_incident (
 incident_id uuid PRIMARY KEY, round_id uuid NOT NULL REFERENCES competition_round(round_id),
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 invalidates_evaluation boolean NOT NULL,
 actor_id varchar(128) NOT NULL, idempotency_key varchar(128) NOT NULL,
 request_sha256 char(64) NOT NULL, incident_json jsonb NOT NULL, created_at timestamp NOT NULL,
 UNIQUE(round_id,actor_id,idempotency_key)
);
CREATE INDEX competition_automation_incident_round ON competition_automation_incident(round_id);
CREATE TRIGGER competition_automation_hold_immutable BEFORE UPDATE OR DELETE ON competition_automation_hold
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_automation_incident_immutable BEFORE UPDATE OR DELETE ON competition_automation_incident
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();

CREATE OR REPLACE FUNCTION competition_v2_decision_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
DECLARE prior competition_reward_decision%ROWTYPE; chosen uuid; seal jsonb; invalidated boolean;
BEGIN
 PERFORM 1 FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT * INTO prior FROM competition_reward_decision WHERE round_id=NEW.round_id ORDER BY revision DESC LIMIT 1;
 IF NEW.revision <> COALESCE(prior.revision,0)+1 OR NEW.previous_decision_sha256 <> COALESCE(prior.decision_sha256,'') THEN
  RAISE EXCEPTION 'v2 reward revision predecessor conflict';
 END IF;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 SELECT EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=NEW.round_id AND invalidates_evaluation) INTO invalidated;
 IF invalidated THEN
  IF NEW.decision_json->>'action' IS DISTINCT FROM 'invalidated' OR NEW.decision_json->>'selected_job_id' IS NOT NULL THEN
   RAISE EXCEPTION 'invalidated evaluation cannot elect a reward recipient';
  END IF;
 ELSE
  IF NEW.decision_json->>'action' = 'invalidated' THEN
   RAISE EXCEPTION 'invalidation requires an authenticated incident record';
  END IF;
  SELECT (e->>'job_id')::uuid INTO chosen FROM jsonb_array_elements(seal->'entries') e
  WHERE (e->>'eligible')::boolean AND COALESCE((SELECT r.review_json->'request'->>'reward_disposition'
   FROM competition_integrity_review r WHERE r.round_id=NEW.round_id AND r.job_id=(e->>'job_id')::uuid
    AND r.review_json->'request'->>'reward_disposition' <> 'unchanged'
   ORDER BY review_cursor DESC LIMIT 1),'approved') <> 'disqualified'
  ORDER BY (e->>'rank')::integer LIMIT 1;
  IF chosen IS DISTINCT FROM (NEW.decision_json->>'selected_job_id')::uuid THEN
   RAISE EXCEPTION 'v2 reward recipient differs from immutable eligible ranking';
  END IF;
 END IF;
 IF (NEW.decision_json->>'revision')::integer <> NEW.revision OR NEW.decision_json->>'seal_sha256' <> seal->>'seal_sha256' THEN
  RAISE EXCEPTION 'v2 reward revision or seal identity conflict';
 END IF;
 RETURN NEW;
END $guard$;
`
