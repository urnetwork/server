// Prospective finalization authority is additive: the original four-kind hold
// journal, frozen policy and published histories retain their existing shape.
package server

import "strings"

const competitionFinalizationHoldMigrationSql = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE competition_finalization_hold (
 hold_id uuid PRIMARY KEY,
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 kind varchar(32) NOT NULL CHECK(kind='finalization'),
 revision integer NOT NULL CHECK(revision>0), active boolean NOT NULL,
 actor_id varchar(128) NOT NULL, idempotency_key varchar(128) NOT NULL,
 request_sha256 char(64) NOT NULL, hold_json jsonb NOT NULL, created_at timestamp NOT NULL,
 UNIQUE(environment,competition_id,season_id,kind,revision),
 UNIQUE(environment,competition_id,season_id,actor_id,idempotency_key)
);
CREATE TRIGGER competition_finalization_hold_immutable BEFORE UPDATE OR DELETE ON competition_finalization_hold
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_finalization_hold_no_truncate BEFORE TRUNCATE ON competition_finalization_hold
 FOR EACH STATEMENT EXECUTE FUNCTION competition_append_only_guard();

-- An advisory wait does not refresh a repeatable-read snapshot. Both control
-- journals and every first terminal write mutate this same scope tuple, so a
-- stale waiter must fail serialization before consulting stale hold state.
CREATE TABLE competition_finalization_scope_fence (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 generation bigint NOT NULL CHECK(generation>0),
 PRIMARY KEY(environment,competition_id,season_id)
);
CREATE FUNCTION competition_finalization_touch_scope(env text, competition text, season text)
RETURNS void LANGUAGE plpgsql AS $finalization_scope$
BEGIN
 IF env IS NULL OR competition IS NULL OR season IS NULL
  OR env !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
  OR competition !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
  OR season !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$' THEN
  RAISE EXCEPTION 'invalid finalization authority scope';
 END IF;
 PERFORM competition_v2_scope_lock(env,competition,season);
 INSERT INTO competition_finalization_scope_fence(environment,competition_id,season_id,generation)
 VALUES(env,competition,season,1)
 ON CONFLICT(environment,competition_id,season_id) DO UPDATE
 SET generation=competition_finalization_scope_fence.generation+1;
END $finalization_scope$;

-- The new physical journal shares the original public idempotency namespace.
-- Old binaries also pass this insert fence; none of their four kinds changes.
CREATE FUNCTION competition_finalization_hold_guard() RETURNS trigger LANGUAGE plpgsql AS $finalization_hold$
DECLARE previous_revision integer;
BEGIN
 PERFORM competition_finalization_touch_scope(NEW.environment,NEW.competition_id,NEW.season_id);
 IF TG_TABLE_NAME='competition_automation_hold' THEN
  IF EXISTS(SELECT 1 FROM competition_finalization_hold h WHERE h.environment=NEW.environment
   AND h.competition_id=NEW.competition_id AND h.season_id=NEW.season_id
   AND h.actor_id=NEW.actor_id AND h.idempotency_key=NEW.idempotency_key) THEN
   RAISE EXCEPTION 'competition hold idempotency key belongs to another capability';
  END IF;
  RETURN NEW;
 END IF;
 IF EXISTS(SELECT 1 FROM competition_automation_hold h WHERE h.environment=NEW.environment
  AND h.competition_id=NEW.competition_id AND h.season_id=NEW.season_id
  AND h.actor_id=NEW.actor_id AND h.idempotency_key=NEW.idempotency_key) THEN
  RAISE EXCEPTION 'competition hold idempotency key belongs to another capability';
 END IF;
 SELECT COALESCE(max(revision),0) INTO previous_revision FROM competition_finalization_hold
 WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id;
 IF NEW.kind IS DISTINCT FROM 'finalization' OR NEW.revision<>previous_revision+1
  OR NEW.hold_json->>'hold_id' IS DISTINCT FROM NEW.hold_id::text
  OR NEW.hold_json->>'environment' IS DISTINCT FROM NEW.environment
  OR NEW.hold_json->>'competition_id' IS DISTINCT FROM NEW.competition_id
  OR NEW.hold_json->>'season_id' IS DISTINCT FROM NEW.season_id
  OR NEW.hold_json->>'actor_id' IS DISTINCT FROM NEW.actor_id
  OR (NEW.hold_json->>'revision')::integer IS DISTINCT FROM NEW.revision
  OR NEW.hold_json->>'request_sha256' IS DISTINCT FROM NEW.request_sha256
  OR NEW.hold_json->'request'->>'kind' IS DISTINCT FROM 'finalization'
  OR (NEW.hold_json->'request'->>'active')::boolean IS DISTINCT FROM NEW.active
  OR (NEW.hold_json->'request'->>'expected_revision')::integer IS DISTINCT FROM previous_revision
  OR NEW.hold_json->'request'->>'idempotency_key' IS DISTINCT FROM NEW.idempotency_key
  OR NEW.request_sha256 !~ '^[0-9a-f]{64}$'
  OR (NOT NEW.active AND NEW.hold_json->'request'->>'reason_code' IS DISTINCT FROM 'RECOVERY_APPROVED') THEN
  RAISE EXCEPTION 'finalization hold requires its exact scoped compare-and-swap record';
 END IF;
 RETURN NEW;
END $finalization_hold$;
CREATE TRIGGER competition_00_finalization_hold_scope BEFORE INSERT ON competition_automation_hold
 FOR EACH ROW EXECUTE FUNCTION competition_finalization_hold_guard();
CREATE TRIGGER competition_00_finalization_hold_scope BEFORE INSERT ON competition_finalization_hold
 FOR EACH ROW EXECUTE FUNCTION competition_finalization_hold_guard();

CREATE FUNCTION competition_finalization_require_policy_unheld(policy jsonb, competition text)
RETURNS void LANGUAGE plpgsql AS $finalization_policy$
DECLARE env text; season text;
BEGIN
 IF NOT COALESCE((policy->'automation_policy'->>'enabled')::boolean,false) THEN RETURN; END IF;
 IF policy->>'competition_id' IS DISTINCT FROM competition THEN
  RAISE EXCEPTION 'finalization requires the immutable competition scope';
 END IF;
 env:=policy->'automation_policy'->>'environment';
 season:=policy->'automation_policy'->>'season_id';
 PERFORM competition_finalization_touch_scope(env,competition,season);
 IF COALESCE((SELECT active FROM competition_finalization_hold
  WHERE environment=env AND competition_id=competition AND season_id=season
  ORDER BY revision DESC LIMIT 1),false) THEN
  RAISE EXCEPTION 'competition finalization is held' USING ERRCODE='55000';
 END IF;
END $finalization_policy$;

CREATE FUNCTION competition_finalization_require_round_unheld(target_round uuid)
RETURNS void LANGUAGE plpgsql AS $finalization_round_scope$
DECLARE frozen_policy jsonb; competition text;
BEGIN
 SELECT policy_json,competition_id INTO frozen_policy,competition FROM competition_round WHERE round_id=target_round;
 IF NOT FOUND THEN RAISE EXCEPTION 'finalization requires an existing immutable round'; END IF;
 PERFORM competition_finalization_require_policy_unheld(frozen_policy,competition);
END $finalization_round_scope$;

CREATE FUNCTION competition_finalization_seal_guard() RETURNS trigger LANGUAGE plpgsql AS $finalization_seal$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM competition_evaluation_seal WHERE round_id=NEW.round_id) THEN
  PERFORM competition_finalization_require_round_unheld(NEW.round_id);
 END IF;
 RETURN NEW;
END $finalization_seal$;
CREATE TRIGGER competition_00_finalization_seal BEFORE INSERT ON competition_evaluation_seal
 FOR EACH ROW EXECUTE FUNCTION competition_finalization_seal_guard();

CREATE FUNCTION competition_finalization_decision_guard() RETURNS trigger LANGUAGE plpgsql AS $finalization_decision$
BEGIN
 IF NEW.revision=1 AND NEW.decision_json->>'action' IS DISTINCT FROM 'invalidated'
  AND NOT EXISTS(SELECT 1 FROM competition_reward_decision WHERE round_id=NEW.round_id) THEN
  PERFORM competition_finalization_require_round_unheld(NEW.round_id);
 END IF;
 RETURN NEW;
END $finalization_decision$;
CREATE TRIGGER competition_00_finalization_decision BEFORE INSERT ON competition_reward_decision
 FOR EACH ROW EXECUTE FUNCTION competition_finalization_decision_guard();

CREATE FUNCTION competition_finalization_round_guard() RETURNS trigger LANGUAGE plpgsql AS $finalization_round$
DECLARE terminal_transition boolean;
BEGIN
 IF TG_OP='INSERT' THEN
  terminal_transition:=NEW.finalized_at IS NOT NULL OR NEW.winner_job_id IS NOT NULL
   OR NEW.automation_sealed_at IS NOT NULL OR NEW.automation_publication_at IS NOT NULL;
  IF terminal_transition THEN
   PERFORM competition_finalization_require_policy_unheld(NEW.policy_json,NEW.competition_id);
  END IF;
 ELSE
  terminal_transition:=(OLD.finalized_at IS NULL AND NEW.finalized_at IS NOT NULL)
   OR (OLD.winner_job_id IS NULL AND NEW.winner_job_id IS NOT NULL)
   OR (OLD.automation_sealed_at IS NULL AND NEW.automation_sealed_at IS NOT NULL)
   OR (OLD.automation_publication_at IS NULL AND NEW.automation_publication_at IS NOT NULL);
  IF terminal_transition THEN
   PERFORM competition_finalization_require_policy_unheld(OLD.policy_json,OLD.competition_id);
  END IF;
 END IF;
 RETURN NEW;
END $finalization_round$;
CREATE TRIGGER competition_00_finalization_round BEFORE INSERT OR UPDATE ON competition_round
 FOR EACH ROW EXECUTE FUNCTION competition_finalization_round_guard();
`

// Positional migration identity follows the additive SQL after every published
// origin migration; no historical migration or monitor predicate is replaced.
func CompetitionFinalizationHoldMigrationVersion() int {
	return sqlMigrationVersion(competitionFinalizationHoldMigrationSql)
}

// Read-only catalog consumers pin the exact new function bodies independently
// of a reported schema head. None of the previous capability guards is replaced.
func CompetitionFinalizationHoldBodiesSql() (scope, hold, policy, roundScope, seal, decision, round string) {
	return strings.Split(competitionFinalizationHoldMigrationSql, "$finalization_scope$")[1],
		strings.Split(competitionFinalizationHoldMigrationSql, "$finalization_hold$")[1],
		strings.Split(competitionFinalizationHoldMigrationSql, "$finalization_policy$")[1],
		strings.Split(competitionFinalizationHoldMigrationSql, "$finalization_round_scope$")[1],
		strings.Split(competitionFinalizationHoldMigrationSql, "$finalization_seal$")[1],
		strings.Split(competitionFinalizationHoldMigrationSql, "$finalization_decision$")[1],
		strings.Split(competitionFinalizationHoldMigrationSql, "$finalization_round$")[1]
}
