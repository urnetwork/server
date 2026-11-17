// Catalog custody follows the actual appended migration, never an assumed
// positional version or a reconstructed approximation of its function bodies.
package server

import "strings"

// Dynamic positioning remains correct when independently authored migrations
// are integrated after the published origin prefix.
func CompetitionProductionBenefitMigrationVersion() int {
	return sqlMigrationVersion(competitionProductionBenefitReviewMigration())
}

// Returns exact installed bodies; no database access or schema mutation occurs.
func CompetitionProductionBenefitBodiesSql() (valid, guard, findingsV1, findingsV2 string) {
	migration := competitionProductionBenefitReviewMigration()
	findings := strings.Split(migration, "$snow_findings$")
	return strings.Split(migration, "$production_benefit$")[1], strings.Split(migration, "$production_benefit_guard$")[1], findings[1], findings[3]
}
