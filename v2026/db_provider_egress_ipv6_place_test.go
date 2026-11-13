// The IPv6 place evidence migration (db_provider_egress_ipv6_place.go), read
// back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, after the
// HTTP/3 place evidence it is judged beside, and its version is its position.
func TestProviderEgressIpv6PlaceMigrationIsAppended(t *testing.T) {
	version := ProviderEgressIpv6PlaceVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != providerEgressIpv6PlaceMigration {
		t.Fatalf("IPv6 place migration version %d is not its position", version)
	}
	if h3 := ProviderEgressH3PlaceVersion(); h3 < 1 || version <= h3 {
		t.Fatalf("IPv6 place migration %d is not after the HTTP/3 place migration %d", version, h3)
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly the evidence's columns; nothing existing is
// altered, rewritten, sampled or deleted.
func TestProviderEgressIpv6PlaceMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := providerEgressIpv6PlaceMigration.sql
	for _, marker := range []string{
		`IF to_regclass('public.provider_egress_ipv6_place') IS NULL THEN`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`CREATE TABLE provider_egress_ipv6_place (`,
		`name varchar(256) NOT NULL,`,
		`country_code varchar(2) NOT NULL,`,
		`tally_day date NOT NULL,`,
		`transport varchar(3) NOT NULL,`,
		`network_ids uuid[] NOT NULL DEFAULT '{}',`,
		`update_time timestamp NOT NULL,`,
		`PRIMARY KEY (name, country_code, tally_day, transport)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("IPv6 place migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("IPv6 place migration bounds its lock wait after it creates the table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("IPv6 place migration touches more than its own new table")
	}
}
