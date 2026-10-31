// The operator score feed needs an indexed bounded walk, not an unindexed
// result limit. This additive migration changes no grading or historical data.
package server

import (
	"strings"
	"testing"
)

// Both scored success and scored failure use the exact query predicate.
func TestCompetitionSubmissionMetricsMigrationAddsOnlyTerminalScoreIndex(t *testing.T) {
	if !strings.Contains(competitionSubmissionMetricsMigrationSql, "ON competition_job (round_id,job_id)") ||
		!strings.Contains(competitionSubmissionMetricsMigrationSql, "WHERE state IN ('succeeded','failed') AND score_json IS NOT NULL;") ||
		strings.Count(competitionSubmissionMetricsMigrationSql, "CREATE INDEX ") != 1 {
		t.Fatal("operator metric migration lost its exact same-round scored-terminal index")
	}
	for _, forbidden := range []string{"ALTER TABLE", "CREATE TABLE", "UPDATE ", "DELETE ", "DROP ", "TRIGGER", "FUNCTION"} {
		if strings.Contains(competitionSubmissionMetricsMigrationSql, forbidden) {
			t.Fatal("operator metric migration changes more than the additive read index", forbidden)
		}
	}
	registered, version := 0, 0
	for index, migration := range migrations {
		if sqlMigration, ok := migration.(*SqlMigration); ok && sqlMigration.sql == competitionSubmissionMetricsMigrationSql {
			registered++
			version = index + 1
		}
	}
	if registered != 1 {
		t.Fatalf("operator metric index SQL is registered %d times, want exactly once", registered)
	}
	t.Logf("operator metric index migration version=%d catalog_count=%d", version, len(migrations))
}
