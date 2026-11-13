// Integration must append the exact composed intake SQL once, after coverage
// and every published predecessor, without assuming a positional ordinal.
package server

import "testing"

// This integration gate deliberately refuses an unregistered preparation tree.
// The separate renderer/unit tests remain usable before integration.
func TestCompetitionIntakeFirstIntegrationRegistersExactComposedSqlOnce(t *testing.T) {
	version := CompetitionIntakeFirstMigrationVersion()
	coverage := CompetitionWorkCoverageMigrationVersion()
	if version < 1 || coverage < 1 || version <= coverage || coverage <= CompetitionProductionBenefitMigrationVersion() || version <= CompetitionDistributedEvaluationMigrationVersion() {
		t.Fatalf("intake is not registered after its exact qualified predecessors: intake=%d coverage=%d", version, coverage)
	}
	count := 0
	for index, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == CompetitionIntakeFirstMigrationSql() {
			count++
			if index+1 != version {
				t.Fatal("intake version helper disagrees with its actual migration row")
			}
		}
	}
	if count != 1 {
		t.Fatalf("exact intake migration was registered %d times", count)
	}
	if len(CompetitionIntakeFirstFunctionsSql()) != 16 {
		t.Fatal("composed intake omitted its independent coverage-round successor")
	}
}
