// Migration 953's catalog identity, for the monitor's schema contract
// (monitor/migration_competition_executor_admission_exception.go). Reads the
// published SQL; changes nothing it publishes.
package server

import "strings"

// The position of the retained host admission exception migration, wherever
// merges place it; 0 when it is not registered.
func CompetitionExecutorAdmissionExceptionMigrationVersion() int {
	sql := CompetitionExecutorAdmissionExceptionMigrationSql()
	version := 0
	for index, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == sql {
			if version != 0 {
				panic("admission exception migration is registered more than once")
			}
			version = index + 1
		}
	}
	return version
}

// The exact function bodies migration 953 publishes: the exception's own
// insert guard, and the whole-host readiness it re-creates to admit a host
// through an exact retained exception and its selected qualification clock.
type CompetitionExecutorAdmissionExceptionBodies struct {
	ExceptionGuard string
	HostsReady     string
}

// Each body is the text between its dollar-quote tags, as the catalog stores it.
func CompetitionExecutorAdmissionExceptionBodiesSql() CompetitionExecutorAdmissionExceptionBodies {
	sql := CompetitionExecutorAdmissionExceptionMigrationSql()
	body := func(tag string) string {
		parts := strings.Split(sql, "$"+tag+"$")
		if len(parts) != 3 {
			panic("admission exception body " + tag + " is absent or ambiguous")
		}
		return parts[1]
	}
	return CompetitionExecutorAdmissionExceptionBodies{
		ExceptionGuard: body("admission_exception"),
		HostsReady:     body("admission_exception_hosts"),
	}
}
