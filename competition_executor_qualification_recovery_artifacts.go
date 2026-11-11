// Migration 952's catalog identity, for the monitor's schema contract
// (monitor/migration_competition_executor_qualification_recovery.go). Reads the
// published SQL; changes nothing it publishes. Its position is
// CompetitionExecutorQualificationRecoveryMigrationVersion.
package server

import "strings"

// The exact function bodies migration 952 publishes: the recovery's own
// functions, and the equivalence guard it re-creates to read the recovered
// qualification clock and bind a receipt to its single replacement.
type CompetitionExecutorQualificationRecoveryBodies struct {
	RecoveryGuard    string
	RecoveryJobGuard string
	Clock            string
	EquivalenceGuard string
}

// Each body is the text between its dollar-quote tags, as the catalog stores it.
func CompetitionExecutorQualificationRecoveryBodiesSql() CompetitionExecutorQualificationRecoveryBodies {
	sql := CompetitionExecutorQualificationRecoveryMigrationSql()
	body := func(tag string) string {
		parts := strings.Split(sql, "$"+tag+"$")
		if len(parts) != 3 {
			panic("executor qualification recovery body " + tag + " is absent or ambiguous")
		}
		return parts[1]
	}
	return CompetitionExecutorQualificationRecoveryBodies{
		RecoveryGuard:    body("qualification_recovery"),
		RecoveryJobGuard: body("recovery_job"),
		Clock:            body("qualification_clock"),
		EquivalenceGuard: body("executor_qualification_recovery_equivalence"),
	}
}
