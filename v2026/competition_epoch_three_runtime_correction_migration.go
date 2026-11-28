// The pre-grading journal records a replacement identity without updating the
// original source, grant, scoring policy, or already recorded readiness.
package server

import "strings"

// The catalog follows the unique position of the published correction SQL.
func CompetitionEpochThreeRuntimeCorrectionMigrationVersion() int {
	return migrationVersionOf(competitionEpochThreeRuntimeCorrectionSql())
}

// The five executable bodies installed by the runtime correction.
type CompetitionEpochThreeRuntimeCorrectionBodies struct {
	CorrectionGuard      string
	ReceiptValid         string
	SourceReadinessGuard string
	IntakeRuntimeReady   string
	IntakeBaselineValid  string
}

// PostgreSQL stores the bytes between the migration's dollar-quote delimiters.
func competitionEpochThreeCorrectionBody(sql, tag string) string {
	parts := strings.Split(sql, "$"+tag+"$")
	if len(parts) != 3 {
		panic("epoch-three correction function body absent or ambiguous: " + tag)
	}
	return parts[1]
}

// Read every body from the same composed SQL installed by the migration.
func CompetitionEpochThreeRuntimeCorrectionBodiesSql() CompetitionEpochThreeRuntimeCorrectionBodies {
	sql := competitionEpochThreeRuntimeCorrectionSql()
	return CompetitionEpochThreeRuntimeCorrectionBodies{
		CorrectionGuard:      competitionEpochThreeCorrectionBody(sql, "runtime_correction"),
		ReceiptValid:         competitionEpochThreeCorrectionBody(sql, "runtime_receipt"),
		SourceReadinessGuard: competitionEpochThreeCorrectionBody(sql, "guard"),
		IntakeRuntimeReady:   competitionEpochThreeCorrectionBody(sql, "intake_runtime_ready"),
		IntakeBaselineValid:  competitionEpochThreeCorrectionBody(sql, "intake_baseline_valid"),
	}
}

// Derive only the three targeted guards. Grading readiness itself stays
// unchanged so its nine-run and two-host requirements cannot be weakened.
func competitionEpochThreeRuntimeCorrectionSql() string {
	return competitionEpochThreeRuntimeCorrectionJournalSql + competitionEpochThreeRuntimeCorrectionReadSql +
		rewriteEpochThreeRuntimeGuard(competitionAutomationGuardMigration, "competition_v2_source_readiness_guard(", "$guard$",
			"NEW.receipt_json->>'qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'",
			"NOT competition_epoch_three_runtime_receipt_valid(intent,NEW.role,NEW.receipt_json)") +
		rewriteEpochThreeRuntimeGuard(competitionIntakeFirstRuntimeSql, "competition_v2_intake_runtime_ready(", "$intake_runtime_ready$",
			"r.receipt_json->>'qualification_sha256'=intent->>'host_qualification_sha256'",
			"competition_epoch_three_runtime_receipt_valid(intent,r.role,r.receipt_json)") +
		rewriteEpochThreeRuntimeGuard(competitionIntakeFirstRuntimeSql, "competition_v2_intake_baseline_valid(", "$intake_baseline_valid$",
			"qualification IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->>'host_qualification_sha256'",
			"qualification IS DISTINCT FROM COALESCE((SELECT correction_json->>'worker_qualification_sha256' FROM competition_epoch_three_runtime_correction WHERE round_id=target), epoch_round.policy_json->'evaluation_policy'->>'host_qualification_sha256')")
}

// A changed published migration fails assembly rather than introducing a
// second fallback or quietly replacing another predicate.
func rewriteEpochThreeRuntimeGuard(sql, name, delimiter, old, replacement string) string {
	_, function, found := strings.Cut(sql, "CREATE FUNCTION "+name)
	declaration, tail, hasOpening := strings.Cut(function, delimiter)
	body, _, hasClosing := strings.Cut(tail, delimiter)
	if !found || !hasOpening || !hasClosing || strings.Count(body, old) != 1 {
		panic("epoch-three runtime guard source changed: " + name)
	}
	return "\nCREATE OR REPLACE FUNCTION " + name + declaration + delimiter +
		strings.Replace(body, old, replacement, 1) + delimiter + ";\n"
}

// A corrected worker receipt binds the actual worker release as well as the
// actual host fingerprint. The historical API receipt remains unchanged.
const competitionEpochThreeRuntimeCorrectionReadSql = `
CREATE FUNCTION competition_epoch_three_runtime_receipt_valid(intent jsonb, role_name text, receipt jsonb)
 RETURNS boolean LANGUAGE sql STABLE AS $runtime_receipt$
 SELECT COALESCE(CASE WHEN role_name='worker' AND c IS NOT NULL THEN
  receipt->>'qualification_sha256'=c->>'worker_qualification_sha256'
  AND receipt->>'runtime_image_digest'=c->>'worker_image_digest'
 ELSE receipt->>'qualification_sha256'=intent->>'host_qualification_sha256' END,false)
 FROM (SELECT (SELECT correction_json FROM competition_epoch_three_runtime_correction
   WHERE round_id=(intent->>'target_round_id')::uuid
   AND correction_json->>'source_activation_generation'=intent->>'generation'
   AND correction_json->>'source_intent_sha256'=intent->>'intent_sha256') AS c) selected
$runtime_receipt$;
`

// Journal registration does not itself authorize or activate a correction.
const competitionEpochThreeRuntimeCorrectionJournalSql = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE competition_epoch_three_runtime_correction (
 round_id uuid PRIMARY KEY REFERENCES competition_round(round_id),
 correction_sha256 char(64) NOT NULL CHECK(correction_sha256 ~ '^[0-9a-f]{64}$'),
 correction_canonical bytea NOT NULL CHECK(octet_length(correction_canonical) BETWEEN 1 AND 8192),
 correction_json jsonb NOT NULL CHECK(jsonb_typeof(correction_json)='object'),
 created_at timestamp NOT NULL,
 CHECK(convert_from(correction_canonical,'UTF8')::jsonb=correction_json),
 CHECK(encode(sha256(correction_canonical),'hex')=correction_sha256)
);
CREATE TRIGGER competition_epoch_three_runtime_correction_append_only
 BEFORE UPDATE OR DELETE ON competition_epoch_three_runtime_correction
 FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_epoch_three_runtime_correction_no_truncate
 BEFORE TRUNCATE ON competition_epoch_three_runtime_correction
 FOR EACH STATEMENT EXECUTE FUNCTION competition_append_only_guard();

CREATE FUNCTION competition_epoch_three_runtime_correction_guard() RETURNS trigger LANGUAGE plpgsql
AS $runtime_correction$
DECLARE
 grant_row competition_source_intake_activation%ROWTYPE;
 epoch_round competition_round%ROWTYPE;
 source_row competition_source_activation%ROWTYPE;
 value jsonb:=NEW.correction_json;
BEGIN
 IF NEW.round_id<>'01a1f4d0-47c9-ac2d-ce45-55ef159a9b41'::uuid THEN
  RAISE EXCEPTION 'runtime correction is not scoped to the exact epoch-three round';
 END IF;
 SELECT * INTO STRICT grant_row FROM competition_source_intake_activation WHERE target_round_id=NEW.round_id;
 PERFORM competition_v2_scope_lock(grant_row.environment,grant_row.competition_id,grant_row.season_id);
 SELECT * INTO STRICT epoch_round FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT * INTO STRICT source_row FROM competition_source_activation
  WHERE (environment,competition_id,season_id,generation)=
        (grant_row.environment,grant_row.competition_id,grant_row.season_id,grant_row.generation) FOR UPDATE;
 IF grant_row.environment<>'main' OR grant_row.generation<>3 OR grant_row.target_epoch<>3
  OR epoch_round.epoch_number<>3 OR epoch_round.staging OR epoch_round.canceled
  OR epoch_round.finalized_at IS NOT NULL OR epoch_round.automation_sealed_at IS NOT NULL
  OR clock_timestamp()>=epoch_round.closes_at
  OR epoch_round.policy_json IS DISTINCT FROM grant_row.round_policy_json
  OR source_row.target_epoch<>3 OR source_row.abandoned_at IS NOT NULL OR source_row.activated_at IS NOT NULL
  OR source_row.intent_json->>'target_round_id' IS DISTINCT FROM NEW.round_id::text
  OR grant_row.grant_json->>'intent_sha256' IS DISTINCT FROM source_row.intent_sha256::text
  OR current_setting('urnetwork.competition_epoch_three_runtime_correction',true) IS DISTINCT FROM NEW.correction_sha256::text
  OR NOT EXISTS(SELECT 1 FROM competition_supervisor_lease l WHERE
   (l.environment,l.competition_id,l.season_id)=(grant_row.environment,grant_row.competition_id,grant_row.season_id)
   AND l.owner_id=current_setting('urnetwork.competition_v2_owner',true)
   AND l.fencing_token::text=current_setting('urnetwork.competition_v2_fence',true)
   AND clock_timestamp()<l.lease_expires_at) THEN
  RAISE EXCEPTION 'runtime correction lacks its approved pre-grading coordinator fence';
 END IF;
 IF EXISTS(SELECT 1 FROM competition_source_readiness r WHERE
      (r.environment,r.competition_id,r.season_id,r.generation)=
      (grant_row.environment,grant_row.competition_id,grant_row.season_id,grant_row.generation) AND r.role='worker')
  OR EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND
     (attempt_count<>0 OR started_at IS NOT NULL OR execution_deadline_at IS NOT NULL))
  OR EXISTS(SELECT 1 FROM competition_round_baseline WHERE round_id=NEW.round_id)
  OR EXISTS(SELECT 1 FROM competition_worker_slot WHERE slot_id=1 AND worker_id IS NOT NULL AND clock_timestamp()<lease_expires_at)
  OR EXISTS(SELECT 1 FROM competition_executor_qualification_run WHERE round_id=NEW.round_id)
  OR EXISTS(SELECT 1 FROM competition_executor_equivalence WHERE round_id=NEW.round_id)
  OR EXISTS(SELECT 1 FROM competition_execution_slot WHERE round_id=NEW.round_id AND job_id IS NOT NULL) THEN
  RAISE EXCEPTION 'runtime correction cannot replace started readiness or evaluation evidence';
 END IF;
 IF (SELECT count(*) FROM jsonb_object_keys(value))<>11
  OR value->'schema' IS DISTINCT FROM '1'::jsonb
  OR value->>'kind' IS DISTINCT FROM 'epoch-three-runtime-identity-correction/v1'
  OR value->>'round_id' IS DISTINCT FROM NEW.round_id::text
  OR value->'source_activation_generation' IS DISTINCT FROM '3'::jsonb
  OR value->>'source_intent_sha256' IS DISTINCT FROM source_row.intent_sha256::text
  OR value->>'source_lock_sha256' IS DISTINCT FROM source_row.intent_json->>'source_lock_sha256'
  OR value->>'evaluator_image_digest' IS DISTINCT FROM source_row.intent_json->>'evaluator_image_digest'
  OR value->>'previous_qualification_sha256' IS DISTINCT FROM source_row.intent_json->>'host_qualification_sha256'
  OR value->>'previous_qualification_sha256' IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->>'host_qualification_sha256'
  OR value->>'worker_qualification_sha256' IS NOT DISTINCT FROM value->>'previous_qualification_sha256'
  OR COALESCE(value->>'worker_qualification_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(value->>'worker_image_digest','') !~ '^sha256:[0-9a-f]{64}$'
  OR COALESCE(value->>'host_containment_receipt_sha256','') !~ '^[0-9a-f]{64}$' THEN
  RAISE EXCEPTION 'runtime correction differs from the frozen source or verified evidence';
 END IF;
 NEW.created_at:=transaction_timestamp();
 RETURN NEW;
END $runtime_correction$;
CREATE TRIGGER competition_epoch_three_runtime_correction_identity
 BEFORE INSERT ON competition_epoch_three_runtime_correction
 FOR EACH ROW EXECUTE FUNCTION competition_epoch_three_runtime_correction_guard();
`
