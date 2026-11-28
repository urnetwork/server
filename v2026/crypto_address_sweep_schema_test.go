// The issued address sweep migration (crypto_address_sweep_schema.go), read
// back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, and its
// version is its position.
func TestCryptoAddressSweepMigrationIsAppended(t *testing.T) {
	version := CryptoAddressSweepVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != cryptoAddressSweepMigration {
		t.Fatalf("issued address sweep migration version %d is not its position", version)
	}
}

// The two tables are created only when absent, under a five-second lock
// bound set before them, with exactly the sweep's columns; nothing existing is
// altered, rewritten, sampled or deleted.
func TestCryptoAddressSweepMigrationCreatesOnlyItsTablesAndIsBounded(t *testing.T) {
	sql := cryptoAddressSweepMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`IF to_regclass('public.crypto_address_sweep') IS NULL THEN`,
		`CREATE TABLE crypto_address_sweep (`,
		`asset varchar(16) NOT NULL,`,
		`address text NOT NULL,`,
		`height bigint NULL,`,
		`signature varchar(128) NULL,`,
		`read_at timestamp NULL,`,
		`last_error text NULL,`,
		`update_time timestamp NOT NULL,`,
		`PRIMARY KEY (asset, address)`,
		`IF to_regclass('public.crypto_address_sweep_position') IS NULL THEN`,
		`CREATE TABLE crypto_address_sweep_position (`,
		`after_address_index bigint NOT NULL,`,
		`PRIMARY KEY (asset)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("issued address sweep migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("issued address sweep migration bounds its lock wait after it creates a table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("issued address sweep migration touches more than its own new tables")
	}
}
