// Keeps the complete upstream migration prefix immutable when signed client-key
// history is appended, and refuses the conflicting pre-promotion draft catalog.
package server

import (
	"strings"
	"testing"
)

const clientKeyHistoryUpstreamMigrationCount = 635

// The independent identity covers every upstream entry, including all four
// planner repairs; the older published-prefix guard stops at entry 592.
func TestStClientKeyHistoryMigrationKeepsUpstreamPrefix(t *testing.T) {
	if len(migrations) <= clientKeyHistoryUpstreamMigrationCount {
		t.Fatal("signed client-key history did not follow the upstream migration prefix")
	}
	identity, err := migrationPrefixIdentity(clientKeyHistoryUpstreamMigrationCount)
	if err != nil {
		t.Fatal(err)
	}
	const expectedIdentity = "35e1073c0067c2843ca5d1e8b3054a9d39b673f6aea85f6e85cfba730bc94c8e"
	if identity != expectedIdentity {
		t.Fatalf("upstream migration prefix identity = %s, want %s", identity, expectedIdentity)
	}
}

// Existing planner-order checks are relative; these fixed indices also prevent
// inserting client-key history ahead of the already published repair sequence.
func TestStClientKeyHistoryMigrationFollowsPlannerRepair(t *testing.T) {
	expected := []struct {
		marker string
		index  int
	}{
		{marker: "transfer_contract_unresolved_source_pair_create_time", index: 631},
		{marker: "transfer_contract_unresolved_destination_pair_create_time", index: 632},
		{marker: "transfer_contract_unresolved_payer_transfer_byte_count", index: 633},
		{marker: "ALTER COLUMN open SET STATISTICS 300", index: 634},
		{marker: "CREATE TABLE st_client_key_history (", index: clientKeyHistoryUpstreamMigrationCount},
	}
	for _, entry := range expected {
		if index := migrationIndex(t, entry.marker); index != entry.index {
			t.Fatalf("migration %q index = %d, want %d", entry.marker, index, entry.index)
		}
	}
	migration, ok := migrations[clientKeyHistoryUpstreamMigrationCount].(*SqlMigration)
	if !ok || migration.sql != clientKeyHistorySchemaSQL {
		t.Fatal("appended client-key migration differs from its complete signed-history schema")
	}
	count := 0
	for _, candidate := range migrations {
		if sql, ok := candidate.(*SqlMigration); ok && strings.Contains(sql.sql, "CREATE TABLE st_client_key_history (") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("signed client-key schema appears %d times, want one append", count)
	}
}

// A retained draft database applied client-key history at index 631. It cannot
// be relabeled as the new upstream planner migration or silently skipped.
func TestStClientKeyHistoryMigrationRefusesOldDraftCatalog(t *testing.T) {
	if len(migrations) <= clientKeyHistoryUpstreamMigrationCount {
		t.Fatal("signed client-key migration is missing")
	}
	const oldDraftVersion = 632
	entries := testMigrationCatalogEntries(t, oldDraftVersion)
	identity, err := migrationIdentity(migrations[clientKeyHistoryUpstreamMigrationCount])
	if err != nil {
		t.Fatal(err)
	}
	entries[oldDraftVersion-1].Identity = identity
	err = validateMigrationCatalog(oldDraftVersion, entries)
	if err == nil || !strings.Contains(err.Error(), "migration 631 identity differs from durable catalog") {
		t.Fatalf("conflicting retained draft catalog was not refused at its original index: %v", err)
	}
}
