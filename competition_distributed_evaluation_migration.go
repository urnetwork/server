// Prospective multi-host execution is additive: the legacy singleton and its
// guards remain available unchanged to the running epoch-one release.
package server

import "strings"

const competitionDistributedEvaluationMigrationSql = `
SET LOCAL lock_timeout = '2s';

ALTER TABLE competition_job
 ADD COLUMN executor_host_id varchar(128) NULL,
 ADD COLUMN executor_slot_id integer NULL,
 ADD COLUMN executor_fencing_token bigint NULL,
 ADD COLUMN execution_deadline_at timestamp NULL,
 ADD CONSTRAINT competition_executor_shape CHECK (
  (executor_host_id IS NULL AND executor_slot_id IS NULL AND executor_fencing_token IS NULL AND execution_deadline_at IS NULL) OR
  (executor_host_id IS NOT NULL AND executor_slot_id IS NOT NULL AND executor_fencing_token IS NOT NULL AND
   executor_slot_id>0 AND executor_fencing_token>0 AND execution_deadline_at IS NOT NULL));

CREATE TABLE competition_execution_slot (
 slot_id integer PRIMARY KEY CHECK(slot_id>0),
 host_id varchar(128) NOT NULL UNIQUE,
 round_id uuid NULL REFERENCES competition_round(round_id),
 worker_id varchar(128) NULL,
 job_id uuid NULL UNIQUE REFERENCES competition_job(job_id),
 lease_expires_at timestamp NULL,
 heartbeat_at timestamp NULL,
 fencing_token bigint NOT NULL DEFAULT 0 CHECK(fencing_token>=0),
 CHECK ((worker_id IS NULL AND job_id IS NULL AND lease_expires_at IS NULL) OR
        (worker_id IS NOT NULL AND job_id IS NOT NULL AND lease_expires_at IS NOT NULL AND round_id IS NOT NULL AND fencing_token>0))
);
CREATE INDEX competition_execution_slot_round ON competition_execution_slot(round_id,slot_id) WHERE job_id IS NOT NULL;
CREATE INDEX competition_baseline_reservation_owner ON competition_job_event(actor_id,job_id)
 WHERE event_type='round_baseline_preparation_reserved';

CREATE FUNCTION competition_executor_legacy_slot_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_legacy_slot$
BEGIN
 IF NEW.worker_id IS NOT NULL AND EXISTS(SELECT 1 FROM competition_execution_slot WHERE job_id IS NOT NULL) THEN
  RAISE EXCEPTION 'legacy singleton cannot overlap a distributed evaluator owner';
 END IF;
 RETURN NEW;
END $executor_legacy_slot$;
CREATE TRIGGER competition_executor_legacy_slot_exclusion BEFORE UPDATE ON competition_worker_slot
 FOR EACH ROW EXECUTE FUNCTION competition_executor_legacy_slot_guard();

CREATE FUNCTION competition_executor_round_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_round$
DECLARE policy jsonb; host_count integer;
BEGIN
 policy:=NEW.policy_json->'distributed_evaluation_policy';
 IF policy IS NULL OR policy='null'::jsonb THEN RETURN NEW; END IF;
 IF NEW.epoch_number<2 OR NEW.staging OR (policy->>'schema')::integer IS DISTINCT FROM 1 OR
    (policy->>'from_epoch')::integer IS DISTINCT FROM 2 OR
    COALESCE(policy->>'approval_sha256','') !~ '^[0-9a-f]{64}$' OR COALESCE(policy->>'coordinator_id','')='' OR
    NOT COALESCE((NEW.policy_json->'automation_policy'->>'enabled')::boolean,false) OR
    NEW.policy_json->'evaluation_policy'->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1' OR
    (NEW.policy_json->'evaluation_policy'->>'score_timeout_seconds')::integer IS DISTINCT FROM 10800 OR
    (NEW.policy_json->'evaluation_policy'->>'replicates')::integer IS DISTINCT FROM 9 OR
    policy->'equivalence_policy'->>'screen_kind' IS DISTINCT FROM 'operational-admission/v1' OR
    (policy->'equivalence_policy'->>'maximum_raw_score_relative_difference')::numeric IS DISTINCT FROM 0.05::numeric OR
    (policy->'equivalence_policy'->>'confidence_z')::numeric IS DISTINCT FROM 1.96::numeric OR
    jsonb_typeof(policy->'hosts') IS DISTINCT FROM 'array' THEN
  RAISE EXCEPTION 'distributed policy must be prospectively frozen for epoch two or later';
 END IF;
 host_count:=jsonb_array_length(policy->'hosts');
 IF host_count<2 OR host_count>16 OR
    (SELECT count(DISTINCT entry->>'host_id') FROM jsonb_array_elements(policy->'hosts') entry)<>host_count OR
    (SELECT count(DISTINCT entry->>'slot_id') FROM jsonb_array_elements(policy->'hosts') entry)<>host_count OR
    (SELECT count(*) FROM jsonb_array_elements(policy->'hosts') entry WHERE entry->>'transport'='local')<>1 OR
    EXISTS(SELECT 1 FROM jsonb_array_elements(policy->'hosts') entry WHERE
     COALESCE(entry->>'host_id','') !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$' OR
     NOT COALESCE((entry->>'slot_id')::integer>0,false) OR COALESCE(entry->>'transport','') NOT IN ('local','ssh') OR
     COALESCE(entry->>'qualification_sha256','') !~ '^[0-9a-f]{64}$' OR
     length(decode(COALESCE(entry->>'agent_public_key_base64',''),'base64'))<>32) THEN
  RAISE EXCEPTION 'distributed round requires distinct independently authenticated executor hosts';
 END IF;
 RETURN NEW;
END $executor_round$;
CREATE TRIGGER competition_executor_round_identity BEFORE INSERT ON competition_round
 FOR EACH ROW EXECUTE FUNCTION competition_executor_round_guard();

CREATE TABLE competition_executor_equivalence (
 round_id uuid NOT NULL REFERENCES competition_round(round_id),
 host_id varchar(128) NOT NULL,
 receipt_json jsonb NOT NULL,
 receipt_sha256 char(64) NOT NULL CHECK(receipt_sha256 ~ '^[0-9a-f]{64}$'),
 baseline_sha256 char(64) NOT NULL CHECK(baseline_sha256 ~ '^[0-9a-f]{64}$'),
 created_at timestamp NOT NULL,
 PRIMARY KEY(round_id,host_id)
);
CREATE TRIGGER competition_executor_equivalence_append_only BEFORE UPDATE OR DELETE ON competition_executor_equivalence
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();

CREATE TABLE competition_executor_qualification_run (
 round_id uuid NOT NULL REFERENCES competition_round(round_id),
 host_id varchar(128) NOT NULL,
 job_id uuid NOT NULL UNIQUE,
 started_at timestamp NOT NULL,
 deadline_at timestamp NOT NULL,
 api_image_digest varchar(71) NOT NULL CHECK(api_image_digest ~ '^sha256:[0-9a-f]{64}$'),
 worker_image_digest varchar(71) NOT NULL CHECK(worker_image_digest ~ '^sha256:[0-9a-f]{64}$'),
 PRIMARY KEY(round_id,host_id),
 CHECK(job_id<>'00000000-0000-0000-0000-000000000000'::uuid),
 CHECK(deadline_at=started_at+interval '3 hours')
);
CREATE TRIGGER competition_executor_qualification_run_append_only BEFORE UPDATE OR DELETE ON competition_executor_qualification_run
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();

CREATE FUNCTION competition_executor_qualification_run_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_qualification_run$
DECLARE epoch_round competition_round%ROWTYPE;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 IF epoch_round.epoch_number<2 OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR epoch_round.automation_sealed_at IS NOT NULL OR
    NOT EXISTS(SELECT 1 FROM competition_round_baseline WHERE round_id=NEW.round_id) OR
    EXISTS(SELECT 1 FROM competition_job WHERE job_id=NEW.job_id) OR
    NOT EXISTS(SELECT 1 FROM jsonb_array_elements(epoch_round.policy_json->'distributed_evaluation_policy'->'hosts') host WHERE host->>'host_id'=NEW.host_id) THEN
  RAISE EXCEPTION 'executor qualification run requires its exact frozen host and shared baseline';
 END IF;
 PERFORM competition_v2_scope_lock(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
 IF NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=epoch_round.policy_json->'automation_policy'->>'environment'
   AND competition_id=epoch_round.competition_id AND season_id=epoch_round.policy_json->'automation_policy'->>'season_id'
   AND owner_id=current_setting('urnetwork.competition_v2_owner',true)
   AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'executor qualification requires the current fenced coordinator';
 END IF;
 RETURN NEW;
END $executor_qualification_run$;
CREATE TRIGGER competition_executor_qualification_run_identity BEFORE INSERT ON competition_executor_qualification_run
 FOR EACH ROW EXECUTE FUNCTION competition_executor_qualification_run_guard();

CREATE FUNCTION competition_executor_equivalence_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_equivalence$
DECLARE epoch_round competition_round%ROWTYPE; baseline competition_round_baseline%ROWTYPE; host jsonb;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 SELECT * INTO baseline FROM competition_round_baseline WHERE round_id=NEW.round_id;
 SELECT entry INTO host FROM jsonb_array_elements(epoch_round.policy_json->'distributed_evaluation_policy'->'hosts') entry WHERE entry->>'host_id'=NEW.host_id;
 IF NOT FOUND OR epoch_round.epoch_number<2 OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR epoch_round.automation_sealed_at IS NOT NULL OR
    baseline.round_id IS NULL OR NEW.baseline_sha256 IS DISTINCT FROM baseline.baseline_sha256 OR
    (NEW.receipt_json->>'schema')::integer IS DISTINCT FROM 1 OR
    NEW.receipt_json->>'round_id' IS DISTINCT FROM NEW.round_id::text OR NEW.receipt_json->>'host_id' IS DISTINCT FROM NEW.host_id OR
    NEW.receipt_json->>'baseline_sha256' IS DISTINCT FROM NEW.baseline_sha256 OR
    NEW.receipt_json->>'source_lock_sha256' IS DISTINCT FROM epoch_round.policy_json->'automation_policy'->>'source_lock_sha256' OR
    NEW.receipt_json->>'providers_sha256' IS DISTINCT FROM epoch_round.providers_sha256 OR
    NEW.receipt_json->>'evaluator_image_digest' IS DISTINCT FROM epoch_round.policy_json->>'evaluator_image_digest' OR
    NEW.receipt_json->>'qualification_sha256' IS DISTINCT FROM host->>'qualification_sha256' OR
    NEW.receipt_json->>'policy_approval_sha256' IS DISTINCT FROM epoch_round.policy_json->'distributed_evaluation_policy'->>'approval_sha256' OR
    NOT COALESCE((NEW.receipt_json->>'passed')::boolean,false) OR NOT COALESCE((NEW.receipt_json->>'noop')::boolean,false) OR
    COALESCE(NEW.receipt_json->>'archive_manifest_sha256','') !~ '^[0-9a-f]{64}$' OR
    (NEW.receipt_json->>'created_at')::timestamp IS DISTINCT FROM NEW.created_at OR
    NOT EXISTS(SELECT 1 FROM competition_executor_qualification_run q WHERE q.round_id=NEW.round_id AND q.host_id=NEW.host_id
     AND NEW.receipt_json->>'qualification_job_id'=q.job_id::text
     AND (NEW.receipt_json->>'qualification_started_at')::timestamp=q.started_at
     AND (NEW.receipt_json->>'qualification_deadline_at')::timestamp=q.deadline_at AND NEW.created_at<=q.deadline_at) THEN
  RAISE EXCEPTION 'executor equivalence requires exact retained no-op shared-baseline evidence';
 END IF;
 PERFORM competition_v2_scope_lock(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
 IF NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=epoch_round.policy_json->'automation_policy'->>'environment'
   AND competition_id=epoch_round.competition_id AND season_id=epoch_round.policy_json->'automation_policy'->>'season_id'
   AND owner_id=current_setting('urnetwork.competition_v2_owner',true)
   AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'executor equivalence requires the current fenced coordinator';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM competition_executor_qualification_run WHERE round_id=NEW.round_id AND host_id=NEW.host_id AND clock_timestamp()<=deadline_at) THEN
  RAISE EXCEPTION 'executor equivalence publication exceeded its original absolute deadline';
 END IF;
 RETURN NEW;
END $executor_equivalence$;
CREATE TRIGGER competition_executor_equivalence_identity BEFORE INSERT ON competition_executor_equivalence
 FOR EACH ROW EXECUTE FUNCTION competition_executor_equivalence_guard();

CREATE OR REPLACE FUNCTION competition_round_baseline_insert_guard()
RETURNS trigger LANGUAGE plpgsql AS $executor_baseline$
DECLARE epoch_round competition_round%ROWTYPE;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 IF epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL THEN
  RAISE EXCEPTION 'competition round baseline cannot be attached after finalization or cancellation';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM competition_round_baseline WHERE round_id=NEW.round_id) AND
    EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND state='succeeded') THEN
  RAISE EXCEPTION 'competition round baseline cannot replace legacy ranking policy';
 END IF;
 IF EXISTS(SELECT 1 FROM competition_job WHERE job_id=NEW.source_job_id AND round_id=NEW.round_id AND state = 'running' AND attempt_count >= NEW.source_attempt) THEN RETURN NEW; END IF;
 IF epoch_round.epoch_number>=2 AND
    COALESCE((epoch_round.policy_json->'distributed_evaluation_policy'->>'from_epoch')::integer BETWEEN 2 AND epoch_round.epoch_number,false) AND
    NEW.source_attempt=1 AND NEW.source_artifact_manifest_sha256=current_setting('urnetwork.competition_executor_baseline_manifest',true) AND
    EXISTS(SELECT 1 FROM competition_job j JOIN competition_job_event e ON e.job_id=j.job_id
     JOIN competition_worker_slot s ON s.slot_id=1 AND s.job_id IS NULL AND s.worker_id=e.actor_id AND clock_timestamp()<s.lease_expires_at
     WHERE j.job_id=NEW.source_job_id AND j.round_id=NEW.round_id AND j.state='queued' AND j.attempt_count=0 AND j.started_at IS NULL
      AND e.event_type='round_baseline_preparation_reserved'
      AND e.actor_id=current_setting('urnetwork.competition_executor_baseline_owner',true)
      AND e.payload_json->>'context_sha256'=current_setting('urnetwork.competition_executor_baseline_context',true)) AND
    EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=epoch_round.policy_json->'automation_policy'->>'environment'
     AND competition_id=epoch_round.competition_id AND season_id=epoch_round.policy_json->'automation_policy'->>'season_id'
     AND owner_id=current_setting('urnetwork.competition_v2_owner',true)
     AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN RETURN NEW; END IF;
 RAISE EXCEPTION 'competition round baseline source does not belong to its round attempt';
END $executor_baseline$;

CREATE FUNCTION competition_executor_authority(policy jsonb, competition text, epoch integer, host text, slot integer, fence bigint)
RETURNS void LANGUAGE plpgsql AS $executor_authority$
DECLARE approved jsonb;
BEGIN
 IF epoch<2 OR (policy->>'schema')::integer IS DISTINCT FROM 1 OR
    NOT COALESCE((policy->>'from_epoch')::integer BETWEEN 2 AND epoch,false) OR
    COALESCE(policy->>'approval_sha256','') !~ '^[0-9a-f]{64}$' OR
    COALESCE(policy->>'coordinator_id','')='' OR jsonb_typeof(policy->'hosts') IS DISTINCT FROM 'array' OR
    current_setting('urnetwork.competition_executor_host',true) IS DISTINCT FROM host OR
    current_setting('urnetwork.competition_executor_slot',true) IS DISTINCT FROM slot::text OR
    current_setting('urnetwork.competition_executor_fence',true) IS DISTINCT FROM fence::text THEN
  RAISE EXCEPTION 'distributed evaluator requires exact prospectively approved executor authority';
 END IF;
 SELECT entry INTO approved FROM jsonb_array_elements(policy->'hosts') entry
  WHERE entry->>'host_id'=host AND (entry->>'slot_id')::integer=slot;
 IF NOT FOUND OR COALESCE(approved->>'transport','') NOT IN ('local','ssh') OR
    COALESCE(approved->>'qualification_sha256','') !~ '^[0-9a-f]{64}$' THEN
  RAISE EXCEPTION 'distributed executor is not qualified in the immutable round';
 END IF;
END $executor_authority$;

CREATE FUNCTION competition_executor_job_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_job$
DECLARE epoch_round competition_round%ROWTYPE; policy jsonb; authority_host text; authority_slot integer; authority_fence bigint;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 policy:=epoch_round.policy_json->'distributed_evaluation_policy';
 IF policy IS NULL OR policy='null'::jsonb THEN
  IF NEW.executor_host_id IS NOT NULL THEN RAISE EXCEPTION 'legacy round cannot acquire distributed authority'; END IF;
  RETURN NEW;
 END IF;
 IF TG_OP='INSERT' THEN
  IF NEW.executor_host_id IS NOT NULL OR NEW.state<>'queued' OR EXISTS(SELECT 1 FROM competition_executor_qualification_run WHERE job_id=NEW.job_id) THEN
   RAISE EXCEPTION 'distributed admission cannot manufacture execution authority or reuse a qualification run';
  END IF;
  RETURN NEW;
 END IF;
 IF OLD.state='queued' AND NEW.state='queued' AND OLD.executor_host_id IS NULL AND NEW.executor_host_id IS NULL THEN RETURN NEW; END IF;
 IF OLD.executor_host_id IS NULL AND NEW.executor_host_id IS NULL AND NEW.state IN ('failed','canceled') THEN RETURN NEW; END IF;
 authority_host:=COALESCE(NEW.executor_host_id,OLD.executor_host_id);
 authority_slot:=COALESCE(NEW.executor_slot_id,OLD.executor_slot_id);
 authority_fence:=COALESCE(NEW.executor_fencing_token,OLD.executor_fencing_token);
 PERFORM competition_v2_scope_lock(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
 PERFORM competition_executor_authority(policy,epoch_round.competition_id,epoch_round.epoch_number,authority_host,authority_slot,authority_fence);
 IF NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=epoch_round.policy_json->'automation_policy'->>'environment'
   AND competition_id=epoch_round.competition_id AND season_id=epoch_round.policy_json->'automation_policy'->>'season_id'
   AND owner_id=current_setting('urnetwork.competition_v2_owner',true)
   AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'distributed execution requires the current coordinator supervisor fence';
 END IF;
 IF NEW.executor_host_id IS NULL OR NEW.executor_slot_id IS NULL OR NEW.executor_fencing_token IS NULL OR NEW.execution_deadline_at IS NULL OR
    (OLD.execution_deadline_at IS NOT NULL AND OLD.execution_deadline_at IS DISTINCT FROM NEW.execution_deadline_at) OR
    (OLD.state='running' AND (OLD.executor_host_id IS DISTINCT FROM NEW.executor_host_id OR OLD.executor_slot_id IS DISTINCT FROM NEW.executor_slot_id)) OR
    (OLD.executor_host_id=NEW.executor_host_id AND OLD.executor_fencing_token IS NOT NULL AND NEW.executor_fencing_token<OLD.executor_fencing_token) THEN
  RAISE EXCEPTION 'distributed run identity and absolute deadline are immutable';
 END IF;
 IF NEW.execution_deadline_at IS DISTINCT FROM
    (CASE WHEN NEW.evaluation_cycle>0 THEN NEW.cycle_started_at ELSE NEW.started_at END)+
      make_interval(secs=>(epoch_round.policy_json->'evaluation_policy'->>'score_timeout_seconds')::integer) THEN
  RAISE EXCEPTION 'distributed deadline must retain its original complete execution budget';
 END IF;
 IF OLD.state='running' AND NOT EXISTS(SELECT 1 FROM competition_execution_slot s WHERE s.slot_id=OLD.executor_slot_id
    AND s.host_id=OLD.executor_host_id AND s.job_id=OLD.job_id AND s.worker_id=OLD.lease_owner AND
    s.fencing_token=OLD.executor_fencing_token AND s.round_id=OLD.round_id) THEN
  RAISE EXCEPTION 'distributed writer lost its current host slot';
 END IF;
 IF OLD.state='running' AND NEW.state='running' AND
    (NEW.attempt_count IS DISTINCT FROM OLD.attempt_count OR NEW.worker_image_digest IS DISTINCT FROM OLD.worker_image_digest) THEN
  RAISE EXCEPTION 'distributed reconciliation cannot create another execution attempt';
 END IF;
 IF NEW.state='running' AND (NOT EXISTS(SELECT 1 FROM competition_round_baseline WHERE round_id=NEW.round_id) OR
    EXISTS(SELECT 1 FROM jsonb_array_elements(policy->'hosts') host WHERE NOT EXISTS(
     SELECT 1 FROM competition_executor_equivalence e JOIN competition_round_baseline b USING(round_id)
     WHERE e.round_id=NEW.round_id AND e.host_id=host->>'host_id' AND e.baseline_sha256=b.baseline_sha256
      AND e.receipt_json->>'policy_approval_sha256'=policy->>'approval_sha256' AND COALESCE((e.receipt_json->>'passed')::boolean,false)))) THEN
  RAISE EXCEPTION 'distributed work requires its shared baseline and every qualified no-op executor';
 END IF;
 IF OLD.state='running' AND NEW.state IN ('succeeded','failed','queued','canceled') THEN
  IF current_setting('urnetwork.competition_executor_artifacts_verified',true) IS DISTINCT FROM 'on' AND
     NOT (NEW.state='failed' AND NEW.score_json IS NULL AND NEW.eval_error_json->>'code'='evaluation_time_budget_exhausted' AND clock_timestamp()>=NEW.execution_deadline_at) THEN
   RAISE EXCEPTION 'distributed outcome requires coordinator-verified retained artifacts';
  END IF;
  IF NEW.state='succeeded' AND clock_timestamp()>=NEW.execution_deadline_at THEN
   RAISE EXCEPTION 'distributed result exceeded its immutable absolute job deadline';
  END IF;
 END IF;
 RETURN NEW;
END $executor_job$;
CREATE TRIGGER competition_executor_job_authority BEFORE INSERT OR UPDATE ON competition_job
 FOR EACH ROW EXECUTE FUNCTION competition_executor_job_guard();

CREATE FUNCTION competition_executor_slot_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_slot$
DECLARE target uuid; job competition_job%ROWTYPE; epoch_round competition_round%ROWTYPE;
BEGIN
 IF TG_OP='INSERT' THEN
  IF NEW.worker_id IS NOT NULL OR NEW.job_id IS NOT NULL OR NEW.fencing_token<>0 THEN RAISE EXCEPTION 'new executor slot must be unclaimed'; END IF;
  RETURN NEW;
 END IF;
 IF NEW.slot_id IS DISTINCT FROM OLD.slot_id OR NEW.host_id IS DISTINCT FROM OLD.host_id OR NEW.fencing_token<OLD.fencing_token THEN
  RAISE EXCEPTION 'executor host and fencing sequence are immutable';
 END IF;
 target:=COALESCE(NEW.job_id,OLD.job_id);
 IF target IS NULL THEN RETURN NEW; END IF;
 IF NEW.job_id IS NOT NULL AND (OLD.job_id IS DISTINCT FROM NEW.job_id OR NEW.fencing_token>OLD.fencing_token) THEN
  -- A write, not just a lock, forces old repeatable-read claim snapshots to
  -- retry before they can miss newly occupied distributed slots.
  UPDATE competition_worker_slot SET heartbeat_at=clock_timestamp() WHERE slot_id=1 AND worker_id IS NULL AND job_id IS NULL;
  IF NOT FOUND THEN RAISE EXCEPTION 'distributed executor lost the legacy exclusion row'; END IF;
 END IF;
 IF NEW.job_id IS NOT NULL AND (EXISTS(SELECT 1 FROM competition_worker_slot WHERE worker_id IS NOT NULL OR job_id IS NOT NULL) OR
    EXISTS(SELECT 1 FROM competition_job WHERE state='running' AND executor_host_id IS NULL)) THEN
  RAISE EXCEPTION 'distributed executor cannot overlap the legacy singleton or its running job';
 END IF;
 SELECT * INTO job FROM competition_job WHERE job_id=target;
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=job.round_id;
 PERFORM competition_executor_authority(epoch_round.policy_json->'distributed_evaluation_policy',epoch_round.competition_id,epoch_round.epoch_number,NEW.host_id,NEW.slot_id,NEW.fencing_token);
 IF job.executor_host_id IS DISTINCT FROM NEW.host_id OR job.executor_slot_id IS DISTINCT FROM NEW.slot_id OR
    job.executor_fencing_token IS DISTINCT FROM NEW.fencing_token OR NEW.round_id IS DISTINCT FROM job.round_id OR
    (NEW.job_id IS NOT NULL AND (job.state<>'running' OR NEW.worker_id IS DISTINCT FROM job.lease_owner OR NEW.lease_expires_at IS DISTINCT FROM job.lease_expires_at)) OR
    (NEW.job_id IS NULL AND job.state='running') THEN
  RAISE EXCEPTION 'executor slot must move atomically with its exact fenced job';
 END IF;
 RETURN NEW;
END $executor_slot$;
CREATE TRIGGER competition_executor_slot_authority BEFORE INSERT OR UPDATE ON competition_execution_slot
 FOR EACH ROW EXECUTE FUNCTION competition_executor_slot_guard();
`

// The schema monitor follows the actual append-only migration position.
func CompetitionDistributedEvaluationMigrationVersion() int {
	return sqlMigrationVersion(competitionDistributedEvaluationMigrationSql)
}

// Returns the actual immutable guard bodies without duplicating SQL in monitors.
func CompetitionDistributedEvaluationBodiesSql() (authority, jobGuard, slotGuard, equivalenceGuard, baselineGuard string) {
	return strings.Split(competitionDistributedEvaluationMigrationSql, "$executor_authority$")[1],
		strings.Split(competitionDistributedEvaluationMigrationSql, "$executor_job$")[1],
		strings.Split(competitionDistributedEvaluationMigrationSql, "$executor_slot$")[1],
		strings.Split(competitionDistributedEvaluationMigrationSql, "$executor_equivalence$")[1],
		strings.Split(competitionDistributedEvaluationMigrationSql, "$executor_baseline$")[1]
}

// The additional round guard leaves the existing source/schedule guards intact.
func CompetitionDistributedRoundGuardBodySql() string {
	return strings.Split(competitionDistributedEvaluationMigrationSql, "$executor_round$")[1]
}

// Existing binaries keep their singleton SQL but cannot overlap new execution.
func CompetitionDistributedLegacySlotGuardBodySql() string {
	return strings.Split(competitionDistributedEvaluationMigrationSql, "$executor_legacy_slot$")[1]
}

// Qualification's private run identity and clock survive coordinator restarts.
func CompetitionDistributedQualificationRunGuardBodySql() string {
	return strings.Split(competitionDistributedEvaluationMigrationSql, "$executor_qualification_run$")[1]
}
