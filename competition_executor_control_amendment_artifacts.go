// Migration 950's catalog identity, for the monitor's schema contract
// (monitor/migration_competition_executor_control_amendment.go). Reads the
// published SQL; changes nothing it publishes. Its position is
// CompetitionExecutorControlAmendmentMigrationVersion.
package server

import "strings"

// The exact function bodies migration 950 publishes: the amendment's own
// functions, and the two it re-creates to read the amended host identity (the
// distributed executor's equivalence guard and the intake grading readiness).
type CompetitionExecutorControlAmendmentBodies struct {
	AmendmentGuard     string
	Host               string
	ReceiptGuard       string
	QualificationGuard string
	HostsReady         string
	EquivalenceGuard   string
	IntakeGradingReady string
}

// Each body is the text between its dollar-quote tags, as the catalog stores it.
func CompetitionExecutorControlAmendmentBodiesSql() CompetitionExecutorControlAmendmentBodies {
	sql := CompetitionExecutorControlAmendmentMigrationSql()
	body := func(tag string) string {
		parts := strings.Split(sql, "$"+tag+"$")
		if len(parts) != 3 {
			panic("executor control amendment body " + tag + " is absent or ambiguous")
		}
		return parts[1]
	}
	return CompetitionExecutorControlAmendmentBodies{
		AmendmentGuard:     body("executor_control_amendment"),
		Host:               body("executor_control_host"),
		ReceiptGuard:       body("executor_control_receipt"),
		QualificationGuard: body("executor_control_qualification"),
		HostsReady:         body("executor_control_hosts_ready"),
		EquivalenceGuard:   body("executor_control_equivalence"),
		IntakeGradingReady: body("executor_control_grading_ready"),
	}
}
