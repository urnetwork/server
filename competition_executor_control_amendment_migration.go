// Operational executor corrections are append-only overlays. No frozen round,
// source grant, job, deadline or historical admission evidence is rewritten.
package server

import "strings"

// The existing equivalence guard keeps all its workload/archive/lease/deadline
// checks; only its host identity lookup observes the explicit round amendment.
func CompetitionExecutorControlAmendmentMigrationSql() string {
	start := strings.Index(competitionDistributedEvaluationMigrationSql, "CREATE FUNCTION competition_executor_equivalence_guard()")
	if start < 0 {
		panic("published executor equivalence guard absent")
	}
	parts := strings.Split(competitionDistributedEvaluationMigrationSql[start:], "$executor_equivalence$")
	if len(parts) < 3 {
		panic("published executor equivalence guard unterminated")
	}
	body := parts[1]
	old := " SELECT entry INTO host FROM jsonb_array_elements(epoch_round.policy_json->'distributed_evaluation_policy'->'hosts') entry WHERE entry->>'host_id'=NEW.host_id;\n IF NOT FOUND OR"
	replacement := " SELECT competition_executor_control_host(NEW.round_id,NEW.host_id) INTO host;\n IF host IS NULL OR"
	if strings.Count(body, old) != 1 {
		panic("published executor host identity lookup changed")
	}
	body = strings.Replace(body, old, replacement, 1)
	gradingStart := strings.Index(competitionIntakeFirstRuntimeSql, "CREATE FUNCTION competition_v2_intake_grading_ready(")
	if gradingStart < 0 {
		panic("published intake grading guard absent")
	}
	gradingParts := strings.Split(competitionIntakeFirstRuntimeSql[gradingStart:], "$intake_grading_ready$")
	if len(gradingParts) < 3 {
		panic("published intake grading guard unterminated")
	}
	grading := gradingParts[1]
	predicateStart := strings.Index(grading, "  IF EXISTS(SELECT 1 FROM jsonb_array_elements(policy->'hosts') host WHERE NOT EXISTS(")
	if predicateStart < 0 {
		panic("published whole-host admission predicate absent")
	}
	const endMarker = " THEN RETURN false; END IF;"
	end := strings.Index(grading[predicateStart:], endMarker)
	if end < 0 {
		panic("published whole-host admission predicate unterminated")
	}
	condition := grading[predicateStart : predicateStart+end+len(endMarker)]
	const previousFingerprint = "e.receipt_json->>'qualification_sha256'=host->>'qualification_sha256'"
	if strings.Count(condition, previousFingerprint) != 1 {
		panic("published whole-host fingerprint predicate changed")
	}
	amendedCondition := strings.Replace(condition, previousFingerprint, "e.receipt_json->>'qualification_sha256'=competition_executor_control_host(target,host->>'host_id')->>'qualification_sha256'\n    AND COALESCE(e.receipt_json->>'control_amendment_sha256','')=COALESCE((SELECT amendment_sha256::text FROM competition_executor_control_amendment WHERE round_id=target),'')", 1)
	grading = strings.Replace(grading, condition, "  IF NOT competition_executor_control_hosts_ready(target) THEN RETURN false; END IF;", 1)
	return competitionExecutorControlAmendmentSql + competitionExecutorControlHostReadyPrefixSql + amendedCondition + "\n RETURN true;\nEND $executor_control_hosts_ready$;\n" +
		"\nCREATE OR REPLACE FUNCTION competition_executor_equivalence_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_control_equivalence$" + body + "$executor_control_equivalence$;\n" +
		"\nCREATE OR REPLACE FUNCTION competition_v2_intake_grading_ready(target uuid) RETURNS boolean LANGUAGE plpgsql AS $executor_control_grading_ready$" + grading + "$executor_control_grading_ready$;\n"
}

// The exact published whole-host predicate is shared with the ordinary claim
// path, so neither intake readiness nor later epochs can accept a stale host.
const competitionExecutorControlHostReadyPrefixSql = `
CREATE FUNCTION competition_executor_control_hosts_ready(target uuid) RETURNS boolean LANGUAGE plpgsql AS $executor_control_hosts_ready$
DECLARE epoch_round competition_round%ROWTYPE; baseline competition_round_baseline%ROWTYPE; policy jsonb;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=target;
 IF NOT FOUND THEN RETURN false; END IF;
 SELECT * INTO baseline FROM competition_round_baseline WHERE round_id=target;
 IF NOT FOUND THEN RETURN false; END IF;
 policy:=epoch_round.policy_json->'distributed_evaluation_policy';
 IF jsonb_typeof(policy->'hosts') IS DISTINCT FROM 'array' OR jsonb_array_length(policy->'hosts')<2 THEN RETURN false; END IF;
`

// Registration is independently discoverable without a guessed positional head.
func CompetitionExecutorControlAmendmentMigrationVersion() int {
	for index, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == CompetitionExecutorControlAmendmentMigrationSql() {
			return index + 1
		}
	}
	return 0
}

const competitionExecutorControlAmendmentSql = `
SET LOCAL lock_timeout = '5s';
CREATE INDEX competition_executor_control_started_job ON competition_job(round_id)
 WHERE attempt_count<>0 OR started_at IS NOT NULL OR execution_deadline_at IS NOT NULL;
CREATE TABLE competition_executor_control_amendment (
 round_id uuid PRIMARY KEY REFERENCES competition_round(round_id),
 amendment_sha256 char(64) NOT NULL CHECK(amendment_sha256 ~ '^[0-9a-f]{64}$'),
 amendment_json jsonb NOT NULL,
 amendment_canonical bytea NOT NULL CHECK(octet_length(amendment_canonical) BETWEEN 1 AND 1048576),
 created_at timestamp NOT NULL,
 CHECK(convert_from(amendment_canonical,'UTF8')::jsonb=amendment_json)
);
CREATE TRIGGER competition_executor_control_amendment_append_only BEFORE UPDATE OR DELETE ON competition_executor_control_amendment
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_executor_control_amendment_no_truncate BEFORE TRUNCATE ON competition_executor_control_amendment
 FOR EACH STATEMENT EXECUTE FUNCTION competition_append_only_guard();

CREATE FUNCTION competition_executor_control_amendment_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_control_amendment$
DECLARE epoch_round competition_round%ROWTYPE; value jsonb; policy jsonb; host jsonb; previous jsonb; host_index integer;
BEGIN
 value:=NEW.amendment_json;
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'executor control amendment requires its exact existing round'; END IF;
 PERFORM competition_v2_scope_lock(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 policy:=epoch_round.policy_json->'distributed_evaluation_policy';
 IF epoch_round.epoch_number<2 OR epoch_round.staging OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR epoch_round.automation_sealed_at IS NOT NULL
  OR policy IS NULL OR policy='null'::jsonb
  OR current_setting('urnetwork.competition_executor_control_amendment',true) IS DISTINCT FROM NEW.amendment_sha256::text
  OR NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=epoch_round.policy_json->'automation_policy'->>'environment'
   AND competition_id=epoch_round.competition_id AND season_id=epoch_round.policy_json->'automation_policy'->>'season_id'
   AND owner_id=current_setting('urnetwork.competition_v2_owner',true)
   AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'executor control amendment lacks its live approved coordinator fence';
 END IF;
 IF EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND (attempt_count<>0 OR started_at IS NOT NULL OR execution_deadline_at IS NOT NULL))
  OR EXISTS(SELECT 1 FROM competition_executor_qualification_run WHERE round_id=NEW.round_id)
  OR EXISTS(SELECT 1 FROM competition_executor_equivalence WHERE round_id=NEW.round_id)
  OR EXISTS(SELECT 1 FROM competition_execution_slot WHERE round_id=NEW.round_id AND job_id IS NOT NULL) THEN
  RAISE EXCEPTION 'executor control amendment cannot reset started customer or host-admission clocks';
 END IF;
 IF (SELECT count(*) FROM jsonb_object_keys(value))<>13
  OR value->'schema' IS DISTINCT FROM '1'::jsonb OR value->>'kind' IS DISTINCT FROM 'executor-control-correctness/v1'
  OR value->>'round_id' IS DISTINCT FROM NEW.round_id::text
  OR value->>'environment' IS DISTINCT FROM epoch_round.policy_json->'automation_policy'->>'environment'
  OR value->>'competition_id' IS DISTINCT FROM epoch_round.competition_id
  OR value->>'season_id' IS DISTINCT FROM epoch_round.policy_json->'automation_policy'->>'season_id'
  OR value->>'policy_approval_sha256' IS DISTINCT FROM policy->>'approval_sha256'
  OR value->>'source_lock_sha256' IS DISTINCT FROM epoch_round.policy_json->'automation_policy'->>'source_lock_sha256'
  OR value->>'evaluator_image_digest' IS DISTINCT FROM epoch_round.policy_json->>'evaluator_image_digest'
  OR value->>'config_local_sha256' IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->>'config_local_sha256'
  OR value->>'vault_local_sha256' IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->>'vault_local_sha256'
  OR COALESCE(value->>'evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR jsonb_typeof(value->'hosts') IS DISTINCT FROM 'array'
  OR jsonb_array_length(value->'hosts') IS DISTINCT FROM jsonb_array_length(policy->'hosts') THEN
  RAISE EXCEPTION 'executor control amendment changed its frozen source or policy scope';
 END IF;
 FOR host_index IN 0..jsonb_array_length(policy->'hosts')-1 LOOP
  host:=value->'hosts'->host_index; previous:=policy->'hosts'->host_index;
  IF (SELECT count(*) FROM jsonb_object_keys(host))<>7
   OR host->>'host_id' IS DISTINCT FROM previous->>'host_id'
   OR host->>'previous_qualification_sha256' IS DISTINCT FROM previous->>'qualification_sha256'
   OR host->>'qualification_sha256' IS NOT DISTINCT FROM previous->>'qualification_sha256'
   OR host->>'agent_sha256' IS NOT DISTINCT FROM host->>'previous_agent_sha256'
   OR EXISTS(SELECT 1 FROM unnest(ARRAY['previous_qualification_sha256','qualification_sha256','previous_agent_sha256','agent_sha256','previous_builder_sha256','builder_sha256']) name
    WHERE COALESCE(host->>name,'') !~ '^[0-9a-f]{64}$') THEN
   RAISE EXCEPTION 'executor control amendment omitted a real bound host replacement';
  END IF;
 END LOOP;
 RETURN NEW;
END $executor_control_amendment$;
CREATE TRIGGER competition_executor_control_amendment_identity BEFORE INSERT ON competition_executor_control_amendment
 FOR EACH ROW EXECUTE FUNCTION competition_executor_control_amendment_guard();

CREATE FUNCTION competition_executor_control_host(target uuid, identity text) RETURNS jsonb LANGUAGE sql STABLE AS $executor_control_host$
 SELECT CASE WHEN amendment.round_id IS NULL THEN original
  ELSE jsonb_set(original,'{qualification_sha256}',replacement->'qualification_sha256',false) END
 FROM competition_round epoch_round
 CROSS JOIN LATERAL jsonb_array_elements(epoch_round.policy_json->'distributed_evaluation_policy'->'hosts') original
 LEFT JOIN competition_executor_control_amendment amendment ON amendment.round_id=epoch_round.round_id
 LEFT JOIN LATERAL (SELECT host FROM jsonb_array_elements(amendment.amendment_json->'hosts') host WHERE host->>'host_id'=identity) amended ON true
 CROSS JOIN LATERAL (SELECT amended.host AS replacement) selected
 WHERE epoch_round.round_id=target AND original->>'host_id'=identity
$executor_control_host$;

CREATE FUNCTION competition_executor_control_receipt_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_control_receipt$
DECLARE expected text;
BEGIN
 SELECT amendment_sha256 INTO expected FROM competition_executor_control_amendment WHERE round_id=NEW.round_id;
 IF COALESCE(NEW.receipt_json->>'control_amendment_sha256','') IS DISTINCT FROM COALESCE(expected,'') THEN
  RAISE EXCEPTION 'executor no-op receipt does not bind the current operational amendment';
 END IF;
 RETURN NEW;
END $executor_control_receipt$;
CREATE TRIGGER competition_executor_control_receipt_identity BEFORE INSERT ON competition_executor_equivalence
 FOR EACH ROW EXECUTE FUNCTION competition_executor_control_receipt_guard();

CREATE FUNCTION competition_executor_control_qualification_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_control_qualification$
DECLARE expected text;
BEGIN
 SELECT amendment_sha256 INTO expected FROM competition_executor_control_amendment WHERE round_id=NEW.round_id;
 IF COALESCE(current_setting('urnetwork.competition_executor_control_amendment',true),'') IS DISTINCT FROM COALESCE(expected,'') THEN
  RAISE EXCEPTION 'stale coordinator cannot start a host-admission clock after control amendment';
 END IF;
 RETURN NEW;
END $executor_control_qualification$;
CREATE TRIGGER competition_executor_control_qualification_identity BEFORE INSERT ON competition_executor_qualification_run
 FOR EACH ROW EXECUTE FUNCTION competition_executor_control_qualification_guard();
`
