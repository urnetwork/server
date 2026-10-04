package server

import "testing"

// Existing participant and gas writers must both receive their exact additive
// schema; a gas-only child cannot occupy the participant's published index.
func TestStOperatorGasMigrationAppendsAfterParticipantSchema(t *testing.T) {
	index := sqlMigrationIndex(t, "CREATE TABLE st_operator_gas_budget")
	if index != 776 {
		t.Fatalf("operator gas migration index = %d, want 776 (migration 777)", index)
	}
	prior, ok := migrations[index-1].(*SqlMigration)
	if !ok || prior.sql != providerWorkSessionSchemaSql {
		t.Fatal("operator gas migration replaced or reordered participant schema 776")
	}
	current, ok := migrations[index].(*SqlMigration)
	if !ok || current.sql != stOperatorGasSchemaSql {
		t.Fatal("operator gas migration does not install its original complete schema")
	}
}
