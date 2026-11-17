// The new migration bounds CHECK validation as well as lock acquisition;
// both limits belong only to its transaction, not a deployed worker's sessions.
package server

import (
	"strings"
	"testing"
)

// The healthy statement budget is separate from the shorter lock wait. Neither
// can be omitted, set after DDL, repeated or leaked as a session/role default.
func TestCompetitionSecondRecoveryMigrationBoundsValidationBeforeDdl(t *testing.T) {
	sql := CompetitionSecondRecoveryMigrationSql()
	const bounds = "SET LOCAL lock_timeout='2s';\nSET LOCAL statement_timeout='30s';\n"
	if !strings.HasPrefix(sql, bounds+"CREATE FUNCTION ") ||
		strings.Count(sql, "lock_timeout") != 1 || strings.Count(sql, "statement_timeout") != 1 {
		t.Fatal("second recovery must bound lock acquisition and validation before any DDL")
	}
	for _, forbidden := range []string{"SET SESSION", "ALTER ROLE", "ALTER DATABASE", "transaction_timeout"} {
		if strings.Contains(sql, forbidden) {
			t.Fatal("migration timeout escaped its transaction", forbidden)
		}
	}
}
