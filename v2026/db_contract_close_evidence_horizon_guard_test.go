// The close evidence guard at the shortest contract horizon: its published
// migration. No database is read.
package server

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The migration replaces the move guard's body with exactly one change, its
// retention age, which is the floor; it follows the move guard in the list, and
// it bounds its lock wait.
func TestContractCloseEvidenceHorizonGuardMigration(t *testing.T) {
	version := ContractCloseEvidenceHorizonGuardMigrationVersion()
	move := ContractCloseEvidenceMoveGuardMigrationVersion()
	if version == 0 || version <= move {
		t.Fatalf("horizon guard version %d, move guard version %d", version, move)
	}
	if count := strings.Count(contractCloseReportEvidenceHorizonGuardSql, "$close_report_guard$"); count != 2 {
		t.Fatalf("the horizon guard delimiter occurs %d times", count)
	}
	if !strings.HasPrefix(strings.TrimSpace(contractCloseReportEvidenceHorizonGuardSql), "SET LOCAL lock_timeout = '5s';") {
		t.Error("the horizon guard does not bound its lock wait")
	}
	if strings.Replace(contractCloseReportEvidenceMoveGuardSql, "interval '91 days'", "interval '35 days'", 1) != contractCloseReportEvidenceHorizonGuardSql {
		t.Fatal("the horizon guard is not the move guard with only its retention age changed")
	}
	if floor := fmt.Sprintf("interval '%d days'", int64(CloseEvidenceRetentionFloor/(24*time.Hour))); !strings.Contains(ContractCloseEvidenceHorizonGuardBodySql(), floor) {
		t.Fatalf("the horizon guard's age is not the floor, %s", floor)
	}
	// the published bodies before it are unchanged
	if !strings.Contains(ContractCloseEvidenceMoveGuardBodySql(), "interval '91 days'") || !strings.Contains(ContractCloseEvidenceRetentionGuardBodySql(), "interval '91 days'") {
		t.Fatal("an earlier evidence guard body changed")
	}
}
