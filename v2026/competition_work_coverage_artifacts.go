// Exact coverage bodies are shared with the read-only migration catalog probe.
package server

import "strings"

// The integration owner appends this migration after the published origin
// prefix; zero means it is not registered and is never a valid monitor version.
func CompetitionWorkCoverageMigrationVersion() int {
	return sqlMigrationVersion(competitionWorkCoverageMigration)
}

// Delimiter names are part of this single authored migration, not a second
// hand-maintained copy of PostgreSQL function behavior.
func CompetitionWorkCoverageBodiesSql() map[string]string {
	bodies := map[string]string{}
	for _, name := range []string{"policy", "score", "eligible", "job", "baseline", "seal", "source", "round", "equivalence"} {
		parts := strings.Split(competitionWorkCoverageMigration, "$coverage_"+name+"$")
		if len(parts) != 3 {
			panic("coverage migration body delimiter is ambiguous")
		}
		bodies[name] = parts[1]
	}
	return bodies
}

// The version-aware wrapper still delegates statistical/legacy gate semantics
// to these published functions, whose exact bodies retain catalog custody.
func CompetitionWorkCoverageLegacyEligibilityBodiesSql() (production, staging string) {
	return strings.Split(competitionAutomationMigration, "$eligible$")[1], strings.Split(competitionAutomationStagingMigration, "$eligible$")[1]
}
