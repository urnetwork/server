// One explicitly approved job may receive a second audited recovery. The old
// audit and every default cycle-zero/one guard remain intact.
package server

import "strings"

// This immutable approval scope is not a settings knob or a general retry policy.
func CompetitionOperatorSecondRecoveryJobId() Id {
	return RequireParseId("01a1cc73-0d5d-0bd4-3f0b-5a2155fbde43")
}

// The successor owns its new bodies and complete predecessor wrappers.
type CompetitionSecondRecoverySqlBodies struct {
	Transition   string
	Insert       string
	InsertGuard  string
	Audit        string
	Commit       string
	Immutable    string
	Executor     string
	Intake       string
	LegacyInsert string
}

// Exact predecessor replacements fail closed when any inherited authority changes.
func CompetitionSecondRecoveryBodiesSql() CompetitionSecondRecoverySqlBodies {
	old := CompetitionOperatorRequestedPatchRetryBodiesSql()
	bodies := CompetitionSecondRecoverySqlBodies{}
	bodies.Transition = strings.ReplaceAll(old.Transition, "competition_job_reevaluation", "competition_job_second_reevaluation")
	bodies.Transition = competitionPatchRetryReplace(bodies.Transition, "previous_job.evaluation_cycle<>0 OR next_job.evaluation_cycle<>1", "previous_job.evaluation_cycle<>1 OR next_job.evaluation_cycle<>2")
	bodies.Transition = competitionPatchRetryReplace(bodies.Transition, "audit.authorization_json->>'schema' IS DISTINCT FROM '2'", "audit.authorization_json->>'schema' IS DISTINCT FROM '3'")
	bodies.Transition = competitionPatchRetryReplace(bodies.Transition, "previous_job.cycle_started_at IS NOT NULL OR previous_job.cycle_attempt_offset<>0", "previous_job.cycle_started_at IS NULL OR previous_job.cycle_attempt_offset<=0")
	bodies.Transition = competitionPatchRetryReplace(bodies.Transition, " IF previous_job.evaluation_cycle", " IF NOT competition_second_recovery_job_allowed(previous_job.job_id) THEN RETURN false; END IF;\n IF previous_job.evaluation_cycle")
	bodies.Transition = competitionPatchRetryReplace(bodies.Transition, "  OR previous_job.executor_host_id IS NULL", "  OR next_job.second_recovery_receipt_json IS DISTINCT FROM (audit.receipt_json-'distributed_proof')\n  OR previous_job.executor_host_id IS NULL")
	mutable := "'executor_host_id','executor_slot_id','executor_fencing_token','execution_deadline_at']"
	if strings.Count(bodies.Transition, mutable) != 2 {
		panic("second recovery predecessor mutable fields changed")
	}
	bodies.Transition = strings.ReplaceAll(bodies.Transition, mutable, "'executor_host_id','executor_slot_id','executor_fencing_token','execution_deadline_at','second_recovery_receipt_json']")

	bodies.Insert = competitionPatchRetryReplace(old.InsertValidation, "j.evaluation_cycle=0", "j.evaluation_cycle=1")
	bodies.Insert = competitionPatchRetryReplace(bodies.Insert, "next_audit.authorization_json->>'schema' IS DISTINCT FROM '2'", "next_audit.authorization_json->>'schema' IS DISTINCT FROM '3'")
	bodies.Audit = old.AuditValidation
	for _, replacement := range [][2]string{
		{"next_audit.authorization_json->>'schema' IS DISTINCT FROM '2'", "next_audit.authorization_json->>'schema' IS DISTINCT FROM '3'"},
		{"next_audit.authorization_json->'expected_cycle' IS DISTINCT FROM '0'::jsonb", "next_audit.authorization_json->'expected_cycle' IS DISTINCT FROM '1'::jsonb"},
		{"next_audit.receipt_json->>'schema' IS DISTINCT FROM '2'", "next_audit.receipt_json->>'schema' IS DISTINCT FROM '3'"},
		{"next_audit.receipt_json->>'cycle' IS DISTINCT FROM '1'", "next_audit.receipt_json->>'cycle' IS DISTINCT FROM '2'"},
		{"native->'identity'->>'cycle' IS DISTINCT FROM '0'", "native->'identity'->>'cycle' IS DISTINCT FROM '1'"},
	} {
		bodies.Audit = competitionPatchRetryReplace(bodies.Audit, replacement[0], replacement[1])
	}
	bodies.Audit = strings.ReplaceAll(bodies.Audit, "original_terminal_json->>'started_at'", "original_terminal_json->>'cycle_started_at'")
	bodies.Audit = competitionPatchRetryReplace(bodies.Audit, "idle_count integer;", "idle_count integer; previous competition_job_reevaluation%ROWTYPE; second jsonb; prior jsonb;")
	bodies.Audit = competitionPatchRetryReplace(bodies.Audit, "\nBEGIN", `
BEGIN
 SELECT * INTO previous FROM competition_job_reevaluation WHERE job_id=next_audit.job_id;
 second:=next_audit.authorization_json->'second_recovery'; prior:=next_audit.receipt_json->'second_recovery'->'previous_recovery';
 IF NOT FOUND OR NOT competition_second_recovery_job_allowed(next_audit.job_id)
  OR previous.receipt_json->>'schema' IS DISTINCT FROM '2' OR previous.receipt_json->>'cycle' IS DISTINCT FROM '1'
  OR previous.receipt_json->>'incident_class' IS DISTINCT FROM 'operator_requested_patch_retry'
  OR previous.round_id IS DISTINCT FROM next_audit.round_id OR previous.environment IS DISTINCT FROM next_audit.environment
  OR previous.competition_id IS DISTINCT FROM next_audit.competition_id OR previous.season_id IS DISTINCT FROM next_audit.season_id
  OR next_audit.previous_receipt_sha256 IS DISTINCT FROM previous.receipt_json->>'receipt_sha256'
  OR jsonb_typeof(second) IS DISTINCT FROM 'object'
  OR second-ARRAY['schema','job_id','previous_receipt_sha256','approval_evidence_sha256']<>'{}'::jsonb
  OR second->>'schema' IS DISTINCT FROM '1' OR second->>'job_id' IS DISTINCT FROM next_audit.job_id::text
  OR second->>'previous_receipt_sha256' IS DISTINCT FROM next_audit.previous_receipt_sha256
  OR COALESCE(second->>'approval_evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR ((next_audit.receipt_json->'second_recovery')-'previous_recovery') IS DISTINCT FROM second
  OR (prior-'started_at') IS DISTINCT FROM (previous.receipt_json-ARRAY['request_sha256','attempt_offset','distributed_proof','started_at'])
  OR (prior->>'started_at')::timestamptz AT TIME ZONE 'UTC' IS DISTINCT FROM (next_audit.original_terminal_json->>'cycle_started_at')::timestamp
  OR next_audit.original_terminal_json->>'evaluation_cycle' IS DISTINCT FROM '1'
  OR next_audit.original_terminal_json->>'cycle_started_at' IS NULL THEN
  RAISE EXCEPTION 'second recovery requires the exact immutable first receipt and new scoped approval';
 END IF;`)
	_, _, bodies.Commit, _, _, _ = CompetitionDistributedReevaluationBodiesSql()
	bodies.Commit = competitionPatchRetryReplace(bodies.Commit, "NEW.authorization_json->>'schema' IS DISTINCT FROM '2'", "NEW.authorization_json->>'schema' IS DISTINCT FROM '3'")
	bodies.Commit = competitionPatchRetryReplace(bodies.Commit, "j.evaluation_cycle=1", "j.evaluation_cycle=2")
	bodies.Commit = competitionPatchRetryReplace(bodies.Commit, "AND j.started_at=", `AND j.cycle_attempt_offset=(NEW.original_terminal_json->>'attempt_count')::integer
  AND j.attempt_count=j.cycle_attempt_offset AND j.available_at=NEW.authorized_at
  AND j.second_recovery_receipt_json=(NEW.receipt_json-'distributed_proof')
  AND j.started_at=`)
	bodies.Immutable = competitionPatchRetryReplace(old.Immutable, "\n\tBEGIN", `
	BEGIN
 IF competition_second_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;
 IF OLD.second_recovery_receipt_json IS DISTINCT FROM NEW.second_recovery_receipt_json THEN
  RAISE EXCEPTION 'second recovery owner projection is immutable';
 END IF;`)
	bodies.Executor = competitionPatchRetryReplace(old.Executor, "\nBEGIN", "\nBEGIN\n IF TG_OP='UPDATE' AND competition_second_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;")
	bodies.Intake = competitionPatchRetryReplace(old.Intake, "\nBEGIN", "\nBEGIN\n IF TG_OP='UPDATE' AND competition_second_recovery_transition_allows(OLD,NEW) THEN RETURN NEW; END IF;")
	bodies.LegacyInsert = competitionPatchRetryReplace(old.Insert, "\n\tBEGIN", `
	BEGIN
 PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
 IF EXISTS(SELECT 1 FROM competition_job_second_reevaluation s WHERE s.environment=NEW.environment
  AND s.competition_id=NEW.competition_id AND s.season_id=NEW.season_id AND s.idempotency_key=NEW.idempotency_key) THEN
  RAISE EXCEPTION 'reevaluation request identity already belongs to the second recovery';
 END IF;`)
	bodies.InsertGuard = `
BEGIN
 PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
 IF EXISTS(SELECT 1 FROM competition_job_reevaluation first_recovery WHERE first_recovery.environment=NEW.environment
  AND first_recovery.competition_id=NEW.competition_id AND first_recovery.season_id=NEW.season_id
  AND first_recovery.idempotency_key=NEW.idempotency_key) THEN
  RAISE EXCEPTION 'second recovery requires a new request identity';
 END IF;
 NEW:=competition_second_recovery_insert_validate(NEW);
 RETURN competition_second_recovery_audit_validate(NEW);
END;`
	return bodies
}

// Catalog custody pins the exact job predicate, not only the function name.
func CompetitionSecondRecoveryScopeBodySql() string {
	return "\n SELECT target='" + CompetitionOperatorSecondRecoveryJobId().String() + "'::uuid\n"
}

// Migration-first permissive guards leave all existing jobs and receipts unchanged.
func CompetitionSecondRecoveryMigrationSql() string {
	bodies := CompetitionSecondRecoveryBodiesSql()
	sql := `SET LOCAL lock_timeout='2s';
CREATE FUNCTION competition_second_recovery_job_allowed(target uuid) RETURNS boolean LANGUAGE sql IMMUTABLE AS $scope$` + CompetitionSecondRecoveryScopeBodySql() + `$scope$;
ALTER TABLE competition_job ADD COLUMN second_recovery_receipt_json jsonb;
ALTER TABLE competition_job DROP CONSTRAINT competition_job_evaluation_cycle_check;
ALTER TABLE competition_job ADD CONSTRAINT competition_job_evaluation_cycle_check CHECK (
 evaluation_cycle IN (0,1) OR (evaluation_cycle=2 AND competition_second_recovery_job_allowed(job_id)));
ALTER TABLE competition_job DROP CONSTRAINT competition_job_cycle_shape;
ALTER TABLE competition_job ADD CONSTRAINT competition_job_cycle_shape CHECK (
 (evaluation_cycle=0 AND cycle_attempt_offset=0 AND cycle_started_at IS NULL AND second_recovery_receipt_json IS NULL) OR
 (evaluation_cycle=1 AND cycle_attempt_offset>0 AND cycle_attempt_offset<=attempt_count AND second_recovery_receipt_json IS NULL) OR
 (evaluation_cycle=2 AND competition_second_recovery_job_allowed(job_id) AND cycle_attempt_offset>0 AND cycle_attempt_offset<=attempt_count
  AND second_recovery_receipt_json IS NOT NULL AND jsonb_typeof(second_recovery_receipt_json)='object'
  AND second_recovery_receipt_json->>'schema' IS NOT DISTINCT FROM '3'
  AND second_recovery_receipt_json->>'cycle' IS NOT DISTINCT FROM '2'));
CREATE TABLE competition_job_second_reevaluation (
 job_id uuid PRIMARY KEY REFERENCES competition_job_reevaluation(job_id) CHECK (competition_second_recovery_job_allowed(job_id)),
 round_id uuid NOT NULL REFERENCES competition_round(round_id), environment varchar(128) NOT NULL,
 competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL, operator_id varchar(128) NOT NULL,
 idempotency_key varchar(128) NOT NULL, request_sha256 char(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
 authorization_sha256 char(64) NOT NULL CHECK (authorization_sha256 ~ '^[0-9a-f]{64}$'),
 previous_receipt_sha256 char(64) NOT NULL CHECK (previous_receipt_sha256 ~ '^[0-9a-f]{64}$'),
 authorized_at timestamp NOT NULL, original_terminal_json jsonb NOT NULL, authorization_json jsonb NOT NULL, receipt_json jsonb NOT NULL,
 transaction_id xid8 NOT NULL DEFAULT pg_current_xact_id(), UNIQUE(environment,competition_id,season_id,idempotency_key));
CREATE TRIGGER competition_job_second_reevaluation_append_only BEFORE UPDATE OR DELETE ON competition_job_second_reevaluation
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
`
	create := func(name, arguments, result, body string) string {
		return "CREATE OR REPLACE FUNCTION " + name + "(" + arguments + ") RETURNS " + result + " LANGUAGE plpgsql AS $second_recovery$" + body + "$second_recovery$;\n"
	}
	sql += create("competition_second_recovery_transition_allows", "previous_job competition_job,next_job competition_job", "boolean", bodies.Transition)
	sql += create("competition_second_recovery_insert_validate", "next_audit competition_job_second_reevaluation", "competition_job_second_reevaluation", bodies.Insert)
	sql += create("competition_second_recovery_audit_validate", "next_audit competition_job_second_reevaluation", "competition_job_second_reevaluation", bodies.Audit)
	sql += create("competition_second_recovery_insert_guard", "", "trigger", bodies.InsertGuard)
	sql += create("competition_second_recovery_commit_guard", "", "trigger", bodies.Commit)
	sql += `CREATE TRIGGER competition_job_second_reevaluation_insert BEFORE INSERT ON competition_job_second_reevaluation
 FOR EACH ROW EXECUTE FUNCTION competition_second_recovery_insert_guard();
CREATE CONSTRAINT TRIGGER competition_job_second_reevaluation_atomic AFTER INSERT ON competition_job_second_reevaluation
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION competition_second_recovery_commit_guard();
`
	sql += create("competition_job_immutable_guard", "", "trigger", bodies.Immutable)
	sql += create("competition_executor_job_guard", "", "trigger", bodies.Executor)
	sql += create("competition_v2_intake_execution_guard", "", "trigger", bodies.Intake)
	sql += create("competition_job_reevaluation_insert_guard", "", "trigger", bodies.LegacyInsert)
	return sql
}

// The normal append-only migration list owns readiness and deployment ordering.
func CompetitionSecondRecoveryMigrationVersion() int {
	return sqlMigrationVersion(CompetitionSecondRecoveryMigrationSql())
}
