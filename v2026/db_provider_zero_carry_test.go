// The zero-carry verdict migration (db_provider_zero_carry.go), read back from
// the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, and its
// version is its position.
func TestProviderZeroCarryMigrationIsAppended(t *testing.T) {
	version := ProviderZeroCarryVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != providerZeroCarryMigration {
		t.Fatalf("zero-carry migration version %d is not its position", version)
	}
	if ProviderEgressTlsVerdictVersion() >= version {
		t.Fatalf("zero-carry migration %d is not after the TLS verdict migration %d", version, ProviderEgressTlsVerdictVersion())
	}
	// the judge's URL condition reads the outcome columns the URL source
	// migration adds (url_probation, url_source)
	if source := ProviderEgressUrlSourceVersion(); source < 1 || version <= source {
		t.Fatalf("zero-carry migration %d is not after the URL source migration %d", version, source)
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly the verdict's columns; nothing existing is altered,
// rewritten, sampled or deleted.
func TestProviderZeroCarryMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := providerZeroCarryMigration.sql
	for _, marker := range []string{
		`IF to_regclass('public.provider_zero_carry') IS NULL THEN`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`CREATE TABLE provider_zero_carry (`,
		`client_id uuid NOT NULL,`,
		`create_time timestamp NOT NULL,`,
		`expire_time timestamp NOT NULL,`,
		`client_version varchar(64) NULL,`,
		`contract_count integer NOT NULL,`,
		`network_count integer NOT NULL,`,
		`carried_byte_count bigint NOT NULL,`,
		`PRIMARY KEY (client_id)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("zero-carry migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("zero-carry migration bounds its lock wait after it creates the table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("zero-carry migration touches more than its own new table")
	}
}
