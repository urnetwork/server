// The native fee schema must follow the complete already published prefix.
package server

import "testing"

// Integration must append the reserved fee fragment after the actual779 NUL
// correction. This root intentionally cannot pass on the standalone778 parent.
func TestStNativeFeeSettlementMigrationKeepsPublishedPrefix(t *testing.T) {
	index := sqlMigrationIndex(t, "CREATE TABLE st_operator_native_fee_owner")
	if index != 779 {
		t.Fatalf("native fee settlement migration index = %d, want 779 (migration780)", index)
	}
	current, ok := migrations[index].(*SqlMigration)
	if !ok || current.sql != stNativeFeeSettlementSchemaSql {
		t.Fatal("native fee settlement does not install its complete original schema")
	}
	prior, ok := migrations[index-2].(*SqlMigration)
	if !ok || prior.sql != providerWorkOpenSchemaSql {
		t.Fatal("native fee settlement reordered published provider-open migration778")
	}
}
