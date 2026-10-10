// New original evidence extends the complete published receipt/purchase prefix.
// It cannot replace v764 or make an incomplete old report look authenticated.
package server

import (
	"strings"
	"testing"
)

// Existing reports keep their original migration and cascade behavior. Only
// newly admitted complete evidence has independent immutable lifetime.
func TestContractCloseEvidenceMigrationPreservesPublishedReceiptAndPurchasePrefix(t *testing.T) {
	legacy := sqlMigrationIndex(t, "CREATE TABLE contract_close_report (")
	purchase := sqlMigrationIndex(t, "CREATE TABLE play_purchase_binding")
	evidence := sqlMigrationIndex(t, "CREATE TABLE contract_close_report_evidence")
	if legacy != 763 || purchase != 765 || evidence != purchase+1 || migrations[legacy].(*SqlMigration).sql != contractCloseReportSchemaSql || migrations[evidence].(*SqlMigration).sql != contractCloseReportEvidenceSchemaSql {
		t.Fatal("original evidence changed the published receipt or purchase migration prefix")
	}
	if strings.Contains(contractCloseReportEvidenceSchemaSql, "REFERENCES") || !strings.Contains(contractCloseReportEvidenceSchemaSql, "PRIMARY KEY (client_id, report_id)") || !strings.Contains(contractCloseReportEvidenceSchemaSql, "BEFORE UPDATE OR DELETE") || !strings.Contains(contractCloseReportEvidenceSchemaSql, "BEFORE TRUNCATE") {
		t.Fatal("original report evidence lost independent immutable client/report custody")
	}
}
