package server

// Additive before deployment: legacy rows keep cycle zero and retain their
// original guard. Only a same-transaction immutable operator audit admits one
// failed-to-queued recovery. Recovered rows also reject old, unfenced writers.
var competitionJobReevaluationMigration = newSqlMigration(`
	SET LOCAL lock_timeout = '2s';
	ALTER TABLE competition_job
	 ADD COLUMN evaluation_cycle integer NOT NULL DEFAULT 0 CHECK (evaluation_cycle IN (0,1)),
	 ADD COLUMN cycle_attempt_offset integer NOT NULL DEFAULT 0 CHECK (cycle_attempt_offset>=0),
	 ADD COLUMN cycle_started_at timestamp NULL,
	 ADD CONSTRAINT competition_job_cycle_shape CHECK (
	  (evaluation_cycle=0 AND cycle_attempt_offset=0 AND cycle_started_at IS NULL) OR
	  (evaluation_cycle=1 AND cycle_attempt_offset>0 AND cycle_attempt_offset<=attempt_count));

	CREATE TABLE competition_job_reevaluation (
	 job_id uuid PRIMARY KEY REFERENCES competition_job(job_id),
	 round_id uuid NOT NULL REFERENCES competition_round(round_id),
	 environment varchar(128) NOT NULL,
	 competition_id varchar(128) NOT NULL,
	 season_id varchar(128) NOT NULL,
	 operator_id varchar(128) NOT NULL,
	 idempotency_key varchar(128) NOT NULL,
	 request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
	 authorization_sha256 char(64) NOT NULL CHECK (authorization_sha256 ~ '^[0-9a-f]{64}$'),
	 authorized_at timestamp NOT NULL,
	 original_terminal_json jsonb NOT NULL,
	 authorization_json jsonb NOT NULL,
	 receipt_json jsonb NOT NULL,
	 transaction_id xid8 NOT NULL DEFAULT pg_current_xact_id(),
	 UNIQUE(environment,competition_id,season_id,idempotency_key)
	);
	CREATE TRIGGER competition_job_reevaluation_append_only
	 BEFORE UPDATE OR DELETE ON competition_job_reevaluation
	 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();

	CREATE FUNCTION competition_job_reevaluation_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
	BEGIN
	 IF NEW.transaction_id IS DISTINCT FROM pg_current_xact_id() OR
	    NEW.operator_id IS DISTINCT FROM current_setting('urnetwork.competition_reevaluation_operator',true) OR
	    NEW.request_sha256 IS DISTINCT FROM current_setting('urnetwork.competition_reevaluation_request',true) OR
	    NOT EXISTS(SELECT 1 FROM competition_job j JOIN competition_round r ON r.round_id=j.round_id
	     WHERE j.job_id=NEW.job_id AND j.round_id=NEW.round_id AND r.competition_id=NEW.competition_id
	      AND j.state='failed' AND j.evaluation_cycle=0 AND j.attempt_count>0 AND j.started_at IS NOT NULL
	      AND (j.score_json IS NULL OR j.score_json='null'::jsonb)
	      AND j.eval_error_json->>'kind'='infrastructure'
	      AND j.eval_error_json->>'code' IN ('artifact_archive_failed','evaluation_time_budget_exhausted')
	      AND j.lease_owner IS NULL AND j.lease_expires_at IS NULL
	      AND to_jsonb(j)=NEW.original_terminal_json AND NOT r.canceled AND r.finalized_at IS NULL
	      AND r.automation_sealed_at IS NULL AND r.admission_closed_at IS NULL AND r.winner_job_id IS NULL
	      AND r.opens_at<=NEW.authorized_at AND NEW.authorized_at<r.closes_at
	      AND r.policy_json->'automation_policy'->>'environment'=NEW.environment
	      AND r.policy_json->'automation_policy'->>'season_id'=NEW.season_id) THEN
	  RAISE EXCEPTION 'reevaluation audit requires exact current operator authority';
	 END IF;
	 RETURN NEW;
	END $guard$;
	CREATE TRIGGER competition_job_reevaluation_insert
	 BEFORE INSERT ON competition_job_reevaluation
	 FOR EACH ROW EXECUTE FUNCTION competition_job_reevaluation_insert_guard();

	CREATE FUNCTION competition_job_reevaluation_commit_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
	BEGIN
	 IF NOT EXISTS(SELECT 1 FROM competition_job j WHERE j.job_id=NEW.job_id AND j.evaluation_cycle=1
	    AND j.state='queued' AND j.cycle_started_at IS NULL
	    AND j.cycle_attempt_offset=(NEW.original_terminal_json->>'attempt_count')::integer
	    AND j.attempt_count=j.cycle_attempt_offset AND j.available_at=NEW.authorized_at) THEN
	  RAISE EXCEPTION 'reevaluation audit and queue transition must commit together';
	 END IF;
	 RETURN NEW;
	END $guard$;
	CREATE CONSTRAINT TRIGGER competition_job_reevaluation_atomic
	 AFTER INSERT ON competition_job_reevaluation DEFERRABLE INITIALLY DEFERRED
	 FOR EACH ROW EXECUTE FUNCTION competition_job_reevaluation_commit_guard();

	CREATE OR REPLACE FUNCTION competition_job_immutable_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
	DECLARE audit competition_job_reevaluation%ROWTYPE;
	BEGIN
	 IF OLD.evaluation_cycle=0 AND NEW.evaluation_cycle=1 THEN
	  SELECT * INTO audit FROM competition_job_reevaluation WHERE job_id=OLD.job_id;
	  IF NOT FOUND OR audit.transaction_id IS DISTINCT FROM pg_current_xact_id() OR
	     audit.operator_id IS DISTINCT FROM current_setting('urnetwork.competition_reevaluation_operator',true) OR
	     audit.request_sha256 IS DISTINCT FROM current_setting('urnetwork.competition_reevaluation_request',true) OR
	     audit.original_terminal_json IS DISTINCT FROM to_jsonb(OLD) OR OLD.state<>'failed' OR NEW.state<>'queued' OR
	     NEW.cycle_attempt_offset<>OLD.attempt_count OR NEW.cycle_started_at IS NOT NULL OR
	     NEW.completed_at IS NOT NULL OR NEW.score_json IS NOT NULL OR NEW.eval_error_json IS NOT NULL OR
	     NEW.artifact_manifest_json IS NOT NULL OR NEW.artifact_manifest_sha256 IS NOT NULL OR
	     NEW.lease_owner IS NOT NULL OR NEW.lease_expires_at IS NOT NULL OR
	     NEW.available_at IS DISTINCT FROM audit.authorized_at OR
	     (to_jsonb(NEW)-ARRAY['evaluation_cycle','cycle_attempt_offset','cycle_started_at','state','completed_at',
	       'score_json','eval_error_json','artifact_manifest_json','artifact_manifest_sha256','available_at'])
	      IS DISTINCT FROM
	     (to_jsonb(OLD)-ARRAY['evaluation_cycle','cycle_attempt_offset','cycle_started_at','state','completed_at',
	       'score_json','eval_error_json','artifact_manifest_json','artifact_manifest_sha256','available_at']) THEN
	   RAISE EXCEPTION 'competition job recovery requires its exact immutable audit';
	  END IF;
	  RETURN NEW;
	 END IF;
	 IF OLD.evaluation_cycle IS DISTINCT FROM NEW.evaluation_cycle OR
	    OLD.cycle_attempt_offset IS DISTINCT FROM NEW.cycle_attempt_offset THEN
	  RAISE EXCEPTION 'competition execution cycle is immutable';
	 END IF;
	 IF OLD.evaluation_cycle>0 THEN
	  IF current_setting('urnetwork.competition_job_authority_id',true) IS DISTINCT FROM OLD.job_id::text OR
	     current_setting('urnetwork.competition_job_authority_cycle',true) IS DISTINCT FROM OLD.evaluation_cycle::text OR
	     current_setting('urnetwork.competition_job_authority_attempt',true) IS DISTINCT FROM OLD.attempt_count::text OR
	     OLD.started_at IS DISTINCT FROM NEW.started_at OR
	     (OLD.cycle_started_at IS NOT NULL AND OLD.cycle_started_at IS DISTINCT FROM NEW.cycle_started_at) OR
	     (OLD.cycle_started_at IS NULL AND NEW.cycle_started_at IS NOT NULL AND
	       NOT (OLD.state='queued' AND NEW.state='running' AND NEW.attempt_count=OLD.attempt_count+1)) THEN
	   RAISE EXCEPTION 'competition recovery writer has stale execution authority';
	  END IF;
	 END IF;
	 IF (OLD.state IN ('succeeded','failed','canceled') AND OLD IS DISTINCT FROM NEW) OR
	    OLD.job_id IS DISTINCT FROM NEW.job_id OR OLD.round_id IS DISTINCT FROM NEW.round_id OR
	    OLD.patch_bytes IS DISTINCT FROM NEW.patch_bytes OR OLD.patch_sha256 IS DISTINCT FROM NEW.patch_sha256 OR
	    OLD.cache_key IS DISTINCT FROM NEW.cache_key OR OLD.submitted_at IS DISTINCT FROM NEW.submitted_at OR
	    OLD.api_image_digest IS DISTINCT FROM NEW.api_image_digest OR
	    OLD.artifact_retain_until IS DISTINCT FROM NEW.artifact_retain_until OR NEW.attempt_count<OLD.attempt_count OR
	    (OLD.score_json IS NOT NULL AND OLD.score_json IS DISTINCT FROM NEW.score_json) OR
	    (OLD.eval_error_json IS NOT NULL AND OLD.eval_error_json IS DISTINCT FROM NEW.eval_error_json) OR
	    (OLD.artifact_manifest_json IS NOT NULL AND OLD.artifact_manifest_json IS DISTINCT FROM NEW.artifact_manifest_json) OR
	    (OLD.artifact_manifest_sha256 IS NOT NULL AND OLD.artifact_manifest_sha256 IS DISTINCT FROM NEW.artifact_manifest_sha256) THEN
	  RAISE EXCEPTION 'competition job immutable fields changed';
	 END IF;
	 RETURN NEW;
	END $guard$;

	CREATE FUNCTION competition_recovery_slot_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
	DECLARE target uuid; generation integer; attempt integer;
	BEGIN
	 FOR target IN SELECT DISTINCT candidate FROM (VALUES(OLD.job_id),(NEW.job_id)) jobs(candidate) WHERE candidate IS NOT NULL LOOP
	  SELECT evaluation_cycle,attempt_count INTO generation,attempt FROM competition_job WHERE job_id=target;
	  IF generation>0 AND
	     (current_setting('urnetwork.competition_job_authority_id',true) IS DISTINCT FROM target::text OR
	      current_setting('urnetwork.competition_job_authority_cycle',true) IS DISTINCT FROM generation::text OR
	      current_setting('urnetwork.competition_job_authority_attempt',true) IS DISTINCT FROM attempt::text) THEN
	   RAISE EXCEPTION 'competition recovery slot writer has stale execution authority';
	  END IF;
	 END LOOP;
	 RETURN NEW;
	END $guard$;
	CREATE TRIGGER competition_recovery_slot_authority BEFORE UPDATE ON competition_worker_slot
	 FOR EACH ROW EXECUTE FUNCTION competition_recovery_slot_guard();
`)

// The catalog version is discovered from the normal append-only migration list.
func CompetitionJobReevaluationMigrationVersion() int {
	return migrationVersion(competitionJobReevaluationMigration)
}
