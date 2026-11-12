// The HTTP/3 place evidence migration (db_provider_egress_h3_place.go), read
// back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, after the
// URL-outcome migrations its judgement reads beside, and its version is its
// position.
func TestProviderEgressH3PlaceMigrationIsAppended(t *testing.T) {
	version := ProviderEgressH3PlaceVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != providerEgressH3PlaceMigration {
		t.Fatalf("HTTP/3 place migration version %d is not its position", version)
	}
	if zeroCarry := ProviderZeroCarryVersion(); zeroCarry < 1 || version <= zeroCarry {
		t.Fatalf("HTTP/3 place migration %d is not after the zero-carry migration %d", version, zeroCarry)
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly the evidence's columns; nothing existing is
// altered, rewritten, sampled or deleted.
func TestProviderEgressH3PlaceMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := providerEgressH3PlaceMigration.sql
	for _, marker := range []string{
		`IF to_regclass('public.provider_egress_h3_place') IS NULL THEN`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`CREATE TABLE provider_egress_h3_place (`,
		`name varchar(256) NOT NULL,`,
		`country_code varchar(2) NOT NULL,`,
		`tally_day date NOT NULL,`,
		`network_ids uuid[] NOT NULL DEFAULT '{}',`,
		`update_time timestamp NOT NULL,`,
		`PRIMARY KEY (name, country_code, tally_day)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("HTTP/3 place migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("HTTP/3 place migration bounds its lock wait after it creates the table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("HTTP/3 place migration touches more than its own new table")
	}
}
