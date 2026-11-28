package server

// The predecessor keeps the existing hold, fencing, timing and signature
// custody checks. This successor admits one additional exact closure tuple.
func CompetitionSilleStorageRecoveryAuditBodySql() string {
	body := CompetitionOperatorRequestedPatchRetryBodiesSql().Audit
	return competitionPatchRetryReplace(body,
		"  OR COALESCE(authority->>'reason','') NOT IN ('retention_budget_exhausted','closed_legacy_prelaunch')",
		`  OR (COALESCE(authority->>'reason','') NOT IN ('retention_budget_exhausted','closed_legacy_prelaunch') AND (
   authority->>'reason' IS DISTINCT FROM 'sille_storage_interruption_20261117'
   OR NEW.environment IS DISTINCT FROM 'main'
   OR NEW.competition_id IS DISTINCT FROM 'sim-latency-season-1'
   OR NEW.season_id IS DISTINCT FROM 'sim-latency-production-20261028-r2'
   OR NEW.round_id::text IS DISTINCT FROM '01a1c110-585b-ce38-4f29-ca4860a1c058'
   OR NEW.job_id::text IS DISTINCT FROM '01a1e738-6c64-357a-8d4f-143e4b3364d3'
   OR jsonb_typeof(NEW.authorization_json->'post_close_approval') IS DISTINCT FROM 'object'
   OR NEW.original_terminal_json->>'attempt_count' IS DISTINCT FROM '1'
   OR authority->>'original_host_id' IS DISTINCT FROM 'sille'
   OR authority->>'original_run_id' IS DISTINCT FROM '35e95bdc8f4db995113463b989323b19be5f5f4681b375ca7d8f731a35eece9b'
   OR authority->>'closure_evidence_sha256' IS DISTINCT FROM '3fc5cbab1e3e2ad6c7c85929f94d67ba599be415c5521ceb9e54bb8c59ef15e0'
   OR NEW.receipt_json->'distributed'->>'reason' IS DISTINCT FROM authority->>'reason'
   OR native->>'state' IS DISTINCT FROM 'expired'
   OR native->>'exit_code' IS DISTINCT FROM '1'
   OR native->>'error_code' IS DISTINCT FROM 'original_deadline_elapsed'))`)
}

func CompetitionSilleStorageRecoveryMigrationSql() string {
	return "SET LOCAL lock_timeout='2s';\n" +
		"CREATE OR REPLACE FUNCTION competition_distributed_recovery_audit_guard() RETURNS trigger LANGUAGE plpgsql AS $storage_recovery$" +
		CompetitionSilleStorageRecoveryAuditBodySql() + "$storage_recovery$;\n"
}

func CompetitionSilleStorageRecoveryMigrationVersion() int {
	return sqlMigrationVersion(CompetitionSilleStorageRecoveryMigrationSql())
}
