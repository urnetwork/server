// New optional custody appends without changing the report or financial prefix.
package server

import "testing"

// Later source can consume both original and legacy reports under one schema.
func TestContractCloseOriginalMigrationPreservesReportPrefix(t *testing.T) {
	prior := sqlMigrationIndex(t, "CREATE TABLE contract_close_report_evidence")
	current := sqlMigrationIndex(t, "ADD COLUMN original_report")
	if current != prior+1 || migrations[current].(*SqlMigration).sql != contractCloseOriginalSchemaSql {
		t.Fatal("original signature migration rewrote the existing financial prefix")
	}
}
