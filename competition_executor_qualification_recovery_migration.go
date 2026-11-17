// One approved replacement admission preserves the original append-only clock.
package server

import "strings"

// Keep the published source, host, archive and fencing predicates, changing
// only the clock resolver to include an explicitly authorized replacement.
func CompetitionExecutorQualificationRecoveryMigrationSql() string {
	previous := CompetitionExecutorControlAmendmentMigrationSql()
	parts := strings.Split(previous, "$executor_control_equivalence$")
	if len(parts) != 3 {
		panic("published amended equivalence guard changed")
	}
	body := parts[1]
	for _, replacement := range []struct{ before, after string }{
		{before: "competition_executor_qualification_run q WHERE q.round_id=NEW.round_id AND q.host_id=NEW.host_id", after: "competition_executor_qualification_clock(NEW.round_id,NEW.host_id) q WHERE true"},
		{before: "competition_executor_qualification_run WHERE round_id=NEW.round_id AND host_id=NEW.host_id", after: "competition_executor_qualification_clock(NEW.round_id,NEW.host_id) WHERE true"},
	} {
		if strings.Count(body, replacement.before) != 1 {
			panic("published qualification clock predicate changed")
		}
		body = strings.Replace(body, replacement.before, replacement.after, 1)
	}
	if strings.Count(body, " RETURN NEW;") != 1 {
		panic("published equivalence guard return boundary changed")
	}
	body = strings.Replace(body, " RETURN NEW;", ` IF COALESCE(NEW.receipt_json->>'recovery_sha256','') IS DISTINCT FROM
  COALESCE((SELECT authorization_sha256::text FROM competition_executor_qualification_recovery WHERE round_id=NEW.round_id AND host_id=NEW.host_id),'') THEN
  RAISE EXCEPTION 'executor receipt lacks its exact single replacement authority';
 END IF;
 RETURN NEW;`, 1)
	return competitionExecutorQualificationRecoverySql +
		"\nCREATE OR REPLACE FUNCTION competition_executor_equivalence_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_qualification_recovery_equivalence$" + body + "$executor_qualification_recovery_equivalence$;\n"
}

// Operators discover the actual appended version rather than guessing a head.
func CompetitionExecutorQualificationRecoveryMigrationVersion() int {
	for index, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == CompetitionExecutorQualificationRecoveryMigrationSql() {
			return index + 1
		}
	}
	return 0
}

const competitionExecutorQualificationRecoverySql = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE competition_executor_qualification_recovery (
 round_id uuid NOT NULL,
 host_id varchar(128) NOT NULL,
 authorization_sha256 char(64) NOT NULL CHECK(authorization_sha256 ~ '^[0-9a-f]{64}$'),
 authorization_json jsonb NOT NULL,
 authorization_canonical bytea NOT NULL CHECK(octet_length(authorization_canonical) BETWEEN 1 AND 32768),
 job_id uuid NOT NULL UNIQUE CHECK(job_id<>'00000000-0000-0000-0000-000000000000'::uuid),
 started_at timestamp NOT NULL DEFAULT (statement_timestamp() AT TIME ZONE 'UTC'),
 deadline_at timestamp NOT NULL DEFAULT ((statement_timestamp() AT TIME ZONE 'UTC')+interval '3 hours'),
 api_image_digest varchar(71) NOT NULL CHECK(api_image_digest ~ '^sha256:[0-9a-f]{64}$'),
 worker_image_digest varchar(71) NOT NULL CHECK(worker_image_digest ~ '^sha256:[0-9a-f]{64}$'),
 PRIMARY KEY(round_id,host_id),
 FOREIGN KEY(round_id,host_id) REFERENCES competition_executor_qualification_run(round_id,host_id),
 CHECK(convert_from(authorization_canonical,'UTF8')::jsonb=authorization_json),
 CHECK(encode(sha256(authorization_canonical),'hex')=authorization_sha256),
 CHECK(deadline_at=started_at+interval '3 hours')
);
CREATE TRIGGER competition_executor_qualification_recovery_append_only BEFORE UPDATE OR DELETE ON competition_executor_qualification_recovery
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_executor_qualification_recovery_no_truncate BEFORE TRUNCATE ON competition_executor_qualification_recovery
 FOR EACH STATEMENT EXECUTE FUNCTION competition_append_only_guard();

CREATE FUNCTION competition_executor_qualification_recovery_guard() RETURNS trigger LANGUAGE plpgsql AS $qualification_recovery$
DECLARE epoch_round competition_round%ROWTYPE; original competition_executor_qualification_run%ROWTYPE; value jsonb; host jsonb; baseline text;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'qualification recovery requires its existing round'; END IF;
 PERFORM competition_v2_scope_lock(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 PERFORM pg_advisory_xact_lock(hashtextextended('competition-qualification-job:'||NEW.job_id::text,0));
 SELECT * INTO original FROM competition_executor_qualification_run WHERE round_id=NEW.round_id AND host_id=NEW.host_id FOR SHARE;
 IF NOT FOUND OR clock_timestamp() AT TIME ZONE 'UTC'<=original.deadline_at THEN
  RAISE EXCEPTION 'qualification recovery requires an expired original immutable clock';
 END IF;
 IF epoch_round.epoch_number<2 OR epoch_round.staging OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR epoch_round.automation_sealed_at IS NOT NULL
  OR NEW.started_at IS DISTINCT FROM statement_timestamp() AT TIME ZONE 'UTC'
  OR EXISTS(SELECT 1 FROM competition_executor_equivalence WHERE round_id=NEW.round_id AND host_id=NEW.host_id)
  OR EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND (attempt_count<>0 OR started_at IS NOT NULL OR execution_deadline_at IS NOT NULL))
  OR EXISTS(SELECT 1 FROM competition_execution_slot WHERE round_id=NEW.round_id AND job_id IS NOT NULL)
  OR EXISTS(SELECT 1 FROM competition_job WHERE job_id=NEW.job_id)
  OR EXISTS(SELECT 1 FROM competition_executor_qualification_run WHERE job_id=NEW.job_id)
  OR current_setting('urnetwork.competition_executor_qualification_recovery',true) IS DISTINCT FROM NEW.authorization_sha256::text
  OR NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=epoch_round.policy_json->'automation_policy'->>'environment'
   AND competition_id=epoch_round.competition_id AND season_id=epoch_round.policy_json->'automation_policy'->>'season_id'
   AND owner_id=current_setting('urnetwork.competition_v2_owner',true)
   AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'qualification recovery lacks its idle live operator fence';
 END IF;
 value:=NEW.authorization_json;
 host:=competition_executor_control_host(NEW.round_id,NEW.host_id);
 SELECT baseline_sha256 INTO baseline FROM competition_round_baseline WHERE round_id=NEW.round_id;
 IF host IS NULL OR baseline IS NULL OR jsonb_typeof(value) IS DISTINCT FROM 'object'
  OR (SELECT count(*) FROM jsonb_object_keys(value))<>17
  OR value->'schema' IS DISTINCT FROM '1'::jsonb OR value->>'kind' IS DISTINCT FROM 'executor-qualification-single-recovery/v1'
  OR value->>'round_id' IS DISTINCT FROM NEW.round_id::text OR value->>'host_id' IS DISTINCT FROM NEW.host_id
  OR value->>'original_job_id' IS DISTINCT FROM original.job_id::text
  OR value->>'replacement_job_id' IS DISTINCT FROM NEW.job_id::text OR NEW.job_id=original.job_id
  OR value->>'baseline_sha256' IS DISTINCT FROM baseline
  OR value->>'qualification_sha256' IS DISTINCT FROM host->>'qualification_sha256'
  OR COALESCE(value->>'control_amendment_sha256','') IS DISTINCT FROM COALESCE((SELECT amendment_sha256::text FROM competition_executor_control_amendment WHERE round_id=NEW.round_id),'')
  OR value->>'policy_approval_sha256' IS DISTINCT FROM epoch_round.policy_json->'distributed_evaluation_policy'->>'approval_sha256'
  OR value->>'source_lock_sha256' IS DISTINCT FROM epoch_round.policy_json->'automation_policy'->>'source_lock_sha256'
  OR value->>'evaluator_image_digest' IS DISTINCT FROM epoch_round.policy_json->>'evaluator_image_digest'
  OR value->>'providers_sha256' IS DISTINCT FROM epoch_round.providers_sha256
  OR value->>'config_local_sha256' IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->>'config_local_sha256'
  OR value->>'vault_local_sha256' IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->>'vault_local_sha256'
  OR COALESCE(value->>'original_score_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(value->>'evidence_sha256','') !~ '^[0-9a-f]{64}$' THEN
  RAISE EXCEPTION 'qualification recovery changed its original failure or frozen scope';
 END IF;
 RETURN NEW;
END $qualification_recovery$;
CREATE TRIGGER competition_executor_qualification_recovery_identity BEFORE INSERT ON competition_executor_qualification_recovery
 FOR EACH ROW EXECUTE FUNCTION competition_executor_qualification_recovery_guard();

CREATE FUNCTION competition_executor_recovery_job_guard() RETURNS trigger LANGUAGE plpgsql AS $recovery_job$
BEGIN
 PERFORM pg_advisory_xact_lock(hashtextextended('competition-qualification-job:'||NEW.job_id::text,0));
 IF EXISTS(SELECT 1 FROM competition_executor_qualification_recovery WHERE job_id=NEW.job_id) THEN
  RAISE EXCEPTION 'replacement qualification cannot become a public submission';
 END IF;
 RETURN NEW;
END $recovery_job$;
-- Existing job guards acquire their round scope first; preserve that ordering.
CREATE TRIGGER zz_competition_executor_recovery_job_identity BEFORE INSERT ON competition_job
 FOR EACH ROW EXECUTE FUNCTION competition_executor_recovery_job_guard();

CREATE FUNCTION competition_executor_qualification_clock(target uuid, identity text)
RETURNS TABLE(job_id uuid,started_at timestamp,deadline_at timestamp,api_image_digest varchar,worker_image_digest varchar)
LANGUAGE sql STABLE AS $qualification_clock$
 SELECT r.job_id,r.started_at,r.deadline_at,r.api_image_digest,r.worker_image_digest
 FROM competition_executor_qualification_recovery r WHERE r.round_id=target AND r.host_id=identity
 UNION ALL
 SELECT q.job_id,q.started_at,q.deadline_at,q.api_image_digest,q.worker_image_digest
 FROM competition_executor_qualification_run q WHERE q.round_id=target AND q.host_id=identity
 AND NOT EXISTS(SELECT 1 FROM competition_executor_qualification_recovery r WHERE r.round_id=target AND r.host_id=identity)
$qualification_clock$;
`
