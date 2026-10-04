// Prospective report custody appends after the entire existing financial migration history.
package server

import "testing"

// No deployed prefix or old writer's empty-id behavior is rewritten by schema preparation.
func TestContractCloseReportMigrationAppendsToFinancialHistory(t *testing.T) {
	prior := sqlMigrationIndex(t, "CREATE TABLE circle_transfer_request")
	current := sqlMigrationIndex(t, "CREATE TABLE contract_close_report")
	if current != prior+1 {
		t.Fatalf("report custody moved the financial migration prefix: %d after %d", current, prior)
	}
	if migrations[current].(*SqlMigration).sql != contractCloseReportSchemaSql {
		t.Fatal("report custody migration is not exact")
	}
}
