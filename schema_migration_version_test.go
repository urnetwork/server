package server

import "testing"

// The catalog monitor follows each of these DDL texts to its version. Each is
// registered exactly once, so the first match is the only one, and its version
// names that migration.
func TestSchemaMigrationVersionsNameTheirMigrations(t *testing.T) {
	for _, test := range []struct {
		name    string
		sql     string
		version func() int
	}{
		{"SN chain census", snChainSchemaSql, SnChainSchemaMigrationVersion},
		{"SN top board", snTopSchemaSql, SnTopSchemaMigrationVersion},
		{"SN top replica provenance", snTopReplicaSchemaSql, SnTopReplicaSchemaMigrationVersion},
		{"verify cohort census", verifyCohortCensusSchemaSql, VerifyCohortCensusSchemaMigrationVersion},
	} {
		registered := 0
		for _, migration := range migrations {
			if sqlMigration, ok := migration.(*SqlMigration); ok && sqlMigration.sql == test.sql {
				registered++
			}
		}
		version := test.version()
		if registered != 1 || version < 1 || len(migrations) < version {
			t.Fatalf("%s DDL is registered %d times, at version %d", test.name, registered, version)
		}
		if sqlMigration, ok := migrations[version-1].(*SqlMigration); !ok || sqlMigration.sql != test.sql {
			t.Fatalf("%s version %d does not name its migration", test.name, version)
		}
	}
}
