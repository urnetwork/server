package server

// The relay chunks migration (db_st_ledger_v2_extender_relay.go) is safe for
// the nightly's db migrate on Main: one nullable column without a default,
// added only when absent under a bounded lock wait, appended after the
// extender pool's columns. Pure: it reads the migration list and its SQL.

import (
	"strings"
	"testing"
)

func TestStLedgerV2ExtenderRelayMigrationShape(t *testing.T) {
	version := StLedgerV2ExtenderRelayMigrationVersion()
	if !(StLedgerV2ExtenderMigrationVersion() < version && version <= MigrationCount()) {
		t.Fatalf("the relay chunks migration is at version %d of %d, the extender pool's at %d", version, MigrationCount(), StLedgerV2ExtenderMigrationVersion())
	}
	sql := strings.Join(strings.Fields(StLedgerV2ExtenderRelaySchemaSql), " ")
	for _, required := range []string{
		"PERFORM set_config('lock_timeout', '5s', true);",
		"ALTER TABLE st_ledger_v2_epoch_rows ADD COLUMN IF NOT EXISTS extender_relay_chunks text[] NULL;",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("the migration lacks %q", required)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "ALTER TABLE") {
		t.Fatal("the migration bounds its lock wait only after it alters the table")
	}
	// nothing else changes shape, rows or a default
	for _, statement := range []string{"DEFAULT", "NOT NULL", "DROP ", "UPDATE ", "DELETE ", "INSERT ", "CREATE ", "USING ", "TYPE "} {
		if strings.Contains(sql, statement) {
			t.Fatalf("the migration runs %q", statement)
		}
	}
	if strings.Count(sql, "ALTER TABLE") != 1 || strings.Count(sql, "ADD COLUMN") != 1 {
		t.Fatal("the migration alters more than the one column")
	}
}
