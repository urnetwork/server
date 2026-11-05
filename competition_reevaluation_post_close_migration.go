// A new recovery grant after intake closes needs explicit finite authority
// and both claim and finalization holds. Published migration bytes stay intact.
package server

import "strings"

const competitionReevaluationPostCloseWindowSql = `
CREATE FUNCTION competition_job_reevaluation_post_close_allows(authorization jsonb, closes timestamp, granted timestamp)
RETURNS boolean LANGUAGE plpgsql STABLE AS $recovery_window$
DECLARE approval jsonb;
BEGIN
 approval := authorization->'post_close_approval';
 IF jsonb_typeof(approval) IS DISTINCT FROM 'object' THEN RETURN false; END IF;
 IF (SELECT count(*) FROM jsonb_object_keys(approval))<>4
  OR approval->>'schema' IS DISTINCT FROM '1'
  OR approval->>'evidence_sha256' !~ '^[0-9a-f]{64}$'
  OR approval->>'evidence_sha256' IS NULL THEN RETURN false; END IF;
 RETURN COALESCE((approval->>'closes_at')::timestamptz AT TIME ZONE 'UTC'=closes
  AND closes<=granted
  AND (authorization->>'authorized_at')::timestamptz AT TIME ZONE 'UTC'<=granted
  AND granted<(authorization->>'expires_at')::timestamptz AT TIME ZONE 'UTC'
  AND (authorization->>'expires_at')::timestamptz<=(approval->>'expires_at')::timestamptz
  AND (approval->>'closes_at')::timestamptz<(approval->>'expires_at')::timestamptz,false);
EXCEPTION WHEN invalid_datetime_format OR datetime_field_overflow OR invalid_parameter_value THEN
 RETURN false;
END $recovery_window$;
`

// Derivation changes one explicit window clause, not the predecessor's job,
// terminal snapshot, principal scope, lease or immutable publication checks.
var competitionReevaluationPostCloseMigration = func() *SqlMigration {
	insertGuard, _, _, _ := CompetitionJobReevaluationBodiesSql()
	oldTerminal := "AND j.eval_error_json->>'kind'='infrastructure'\n\t      AND j.eval_error_json->>'code' IN ('artifact_archive_failed','evaluation_time_budget_exhausted')"
	if strings.Count(insertGuard, oldTerminal) != 1 {
		panic("incident recovery predecessor terminal changed")
	}
	insertGuard = strings.Replace(insertGuard, oldTerminal, `AND (
       (`+strings.TrimPrefix(oldTerminal, "AND ")+` AND NEW.authorization_json->>'incident_class' IS DISTINCT FROM 'operator_authorized_backing_service_v1') OR
       (j.eval_error_json->>'kind'='submission' AND j.eval_error_json->>'code'='run_unstable'
        AND NEW.authorization_json->>'incident_class'='operator_authorized_backing_service_v1'))`, 1)
	oldWindow := `AND r.opens_at<=NEW.authorized_at AND NEW.authorized_at<r.closes_at`
	newWindow := `AND r.opens_at<=NEW.authorized_at AND (
       (NEW.authorized_at<r.closes_at AND NOT (NEW.authorization_json ? 'post_close_approval')) OR
       (NEW.authorized_at>=r.closes_at
        AND competition_job_reevaluation_post_close_allows(NEW.authorization_json,r.closes_at,NEW.authorized_at)
        AND (SELECT active FROM competition_automation_hold h WHERE h.environment=NEW.environment
         AND h.competition_id=NEW.competition_id AND h.season_id=NEW.season_id AND h.kind='evaluation'
         ORDER BY revision DESC LIMIT 1) IS TRUE
        AND (SELECT active FROM competition_finalization_hold h WHERE h.environment=NEW.environment
         AND h.competition_id=NEW.competition_id AND h.season_id=NEW.season_id AND h.kind='finalization'
         ORDER BY revision DESC LIMIT 1) IS TRUE))`
	if strings.Count(insertGuard, oldWindow) != 1 || strings.Count(insertGuard, "BEGIN") != 1 {
		panic("post-close recovery predecessor shape changed")
	}
	insertGuard = strings.Replace(insertGuard, oldWindow, newWindow, 1)
	insertGuard = strings.Replace(insertGuard, "BEGIN", `BEGIN
 IF NEW.authorization_json->>'incident_class' IN ('operator_authorized_heartbeat_budget_v1','operator_authorized_backing_service_v1') THEN
  IF NEW.receipt_json->>'incident_class' IS DISTINCT FROM NEW.authorization_json->>'incident_class'
   OR NEW.receipt_json->'previous_failure'->>'kind' IS DISTINCT FROM NEW.original_terminal_json->'eval_error_json'->>'kind'
   OR NEW.receipt_json->'previous_failure'->>'code' IS DISTINCT FROM NEW.original_terminal_json->'eval_error_json'->>'code' THEN
   RAISE EXCEPTION 'incident recovery must retain its original failure and typed approval';
  END IF;
  IF NEW.authorization_json->>'incident_class'='operator_authorized_heartbeat_budget_v1'
   AND (NEW.original_terminal_json->'eval_error_json'->>'kind' IS DISTINCT FROM 'infrastructure'
    OR NEW.original_terminal_json->'eval_error_json'->>'code' IS DISTINCT FROM 'evaluation_time_budget_exhausted') THEN
   RAISE EXCEPTION 'heartbeat recovery requires an exhausted infrastructure budget';
  END IF;
  IF NEW.authorization_json->>'incident_class'='operator_authorized_backing_service_v1'
   AND (NEW.authorization_json->'backing_service_evidence'->>'schema' IS DISTINCT FROM '1'
    OR NEW.authorization_json->'backing_service_evidence'->>'service' IS DISTINCT FROM 'postgresql'
    OR COALESCE(NEW.authorization_json->'backing_service_evidence'->>'artifact_sha256' !~ '^[0-9a-f]{64}$',true)
    OR COALESCE(NEW.authorization_json->'backing_service_evidence'->>'artifact_bytes' !~ '^[1-9][0-9]{0,9}$',true)
    OR COALESCE(NEW.authorization_json->'backing_service_evidence'->>'artifact_path' !~
      ('^evidence/scorer-input/candidate-[0-9]{2}-'||left(NEW.job_id::text,8)||'/stderr[.]log$'),true)) THEN
   RAISE EXCEPTION 'backing service recovery requires exact retained log evidence';
  END IF;
 END IF;
 IF NEW.authorization_json ? 'post_close_approval' THEN
  PERFORM competition_finalization_touch_scope(NEW.environment,NEW.competition_id,NEW.season_id);
 END IF;`, 1)
	return newSqlMigration(competitionReevaluationPostCloseWindowSql +
		"CREATE OR REPLACE FUNCTION competition_job_reevaluation_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$" + insertGuard + "$guard$;")
}()

// The append-only list, not a hard-coded ordinal, owns the successor version.
func CompetitionReevaluationPostCloseMigrationVersion() int {
	return migrationVersion(competitionReevaluationPostCloseMigration)
}

// Catalog monitors independently own the new window predicate and the exact
// successor insert guard while retaining all other historical recovery checks.
func CompetitionReevaluationPostCloseBodiesSql() (window, insertGuard string) {
	return strings.Split(competitionReevaluationPostCloseWindowSql, "$recovery_window$")[1],
		strings.Split(competitionReevaluationPostCloseMigration.sql, "$guard$")[1]
}
