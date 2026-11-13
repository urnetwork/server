// The execution trigger and readiness projection consume the same authenticated
// admission set, including separately approved retained failures.
package server

import "strings"

// Retains the published job/slot/fence/deadline and outcome guards verbatim;
// only the obsolete passing-receipt-only predicate gains current custody.
func CompetitionExecutorAdmissionClaimGuardBodySql() string {
	_, body, _, _, _ := CompetitionDistributedEvaluationBodiesSql()
	const previous = ` IF NEW.state='running' AND (NOT EXISTS(SELECT 1 FROM competition_round_baseline WHERE round_id=NEW.round_id) OR
    EXISTS(SELECT 1 FROM jsonb_array_elements(policy->'hosts') host WHERE NOT EXISTS(
     SELECT 1 FROM competition_executor_equivalence e JOIN competition_round_baseline b USING(round_id)
     WHERE e.round_id=NEW.round_id AND e.host_id=host->>'host_id' AND e.baseline_sha256=b.baseline_sha256
      AND e.receipt_json->>'policy_approval_sha256'=policy->>'approval_sha256' AND COALESCE((e.receipt_json->>'passed')::boolean,false)))) THEN`
	if strings.Count(body, previous) != 1 {
		panic("published executor job admission predicate changed")
	}
	return strings.Replace(body, previous, " IF NEW.state='running' AND NOT competition_executor_control_hosts_ready(NEW.round_id) THEN", 1)
}

// Additive migration-first correction: no receipt, round, deadline or trigger
// is removed, relabeled or rewritten, and historical migrations stay immutable.
func CompetitionExecutorAdmissionClaimMigrationSql() string {
	return "SET LOCAL lock_timeout='5s';\nCREATE OR REPLACE FUNCTION competition_executor_job_guard() RETURNS trigger LANGUAGE plpgsql AS $executor_admitted_job$" + CompetitionExecutorAdmissionClaimGuardBodySql() + "$executor_admitted_job$;\n"
}

// The monitor and operators use the actual append-only position.
func CompetitionExecutorAdmissionClaimMigrationVersion() int {
	return sqlMigrationVersion(CompetitionExecutorAdmissionClaimMigrationSql())
}
