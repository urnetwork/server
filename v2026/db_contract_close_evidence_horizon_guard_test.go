// The close evidence guard at the shortest contract horizon, then at the
// evidence's own window: their published migrations. No database is read.
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
	if floor := fmt.Sprintf("interval '%d days'", int64(closeEvidenceHorizonGuardFloor/(24*time.Hour))); !strings.Contains(ContractCloseEvidenceHorizonGuardBodySql(), floor) {
		t.Fatalf("the horizon guard's age is not its floor, %s", floor)
	}
	// the published bodies before it are unchanged
	if !strings.Contains(ContractCloseEvidenceMoveGuardBodySql(), "interval '91 days'") || !strings.Contains(ContractCloseEvidenceRetentionGuardBodySql(), "interval '91 days'") {
		t.Fatal("an earlier evidence guard body changed")
	}
}

// The window guard replaces the horizon guard's body with exactly one change,
// its retention age, which is the floor, 7 days (owner, 2026-11-23); it
// follows the horizon guard in the list, it is the list's newest evidence
// guard, and it bounds its lock wait. The horizon guard's published body still
// holds 35 days, which a database keeps until this applies.
func TestContractCloseEvidenceWindowGuardMigration(t *testing.T) {
	version := ContractCloseEvidenceWindowGuardMigrationVersion()
	horizon := ContractCloseEvidenceHorizonGuardMigrationVersion()
	if version == 0 || version <= horizon || horizon <= ContractCloseEvidenceMoveGuardMigrationVersion() {
		t.Fatalf("window guard version %d, horizon guard version %d", version, horizon)
	}
	if count := strings.Count(contractCloseReportEvidenceWindowGuardSql, "$close_report_guard$"); count != 2 {
		t.Fatalf("the window guard delimiter occurs %d times", count)
	}
	if !strings.HasPrefix(strings.TrimSpace(contractCloseReportEvidenceWindowGuardSql), "SET LOCAL lock_timeout = '5s';") {
		t.Error("the window guard does not bound its lock wait")
	}
	if strings.Replace(contractCloseReportEvidenceHorizonGuardSql, "interval '35 days'", "interval '7 days'", 1) != contractCloseReportEvidenceWindowGuardSql {
		t.Fatal("the window guard is not the horizon guard with only its retention age changed")
	}
	if floor := fmt.Sprintf("interval '%d days'", int64(CloseEvidenceRetentionFloor/(24*time.Hour))); floor != "interval '7 days'" || !strings.Contains(ContractCloseEvidenceWindowGuardBodySql(), floor) {
		t.Fatalf("the window guard's age is not the floor, %s", floor)
	}
	if CloseEvidenceRetentionWindow < CloseEvidenceRetentionFloor {
		t.Fatalf("the window %s is below the guard's floor %s", CloseEvidenceRetentionWindow, CloseEvidenceRetentionFloor)
	}
	// the published bodies before it are unchanged
	if !strings.Contains(ContractCloseEvidenceHorizonGuardBodySql(), "interval '35 days'") ||
		!strings.Contains(ContractCloseEvidenceMoveGuardBodySql(), "interval '91 days'") ||
		!strings.Contains(ContractCloseEvidenceRetentionGuardBodySql(), "interval '91 days'") {
		t.Fatal("an earlier evidence guard body changed")
	}
	// no later migration replaces the guard again
	for _, migration := range migrations[version:] {
		if sqlMigration, ok := migration.(*SqlMigration); ok && strings.Contains(sqlMigration.sql, "contract_close_report_evidence_guard()") {
			t.Fatal("a later migration replaces the evidence guard; the window guard's floor reads would miss it")
		}
	}
}
