//go:build sidecar_pending

package server

import (
	"regexp"
	"testing"
)

// CODESTYLE's migration rules for §12's parts (DNS-M5-09): each entry creates one of §12.14's tables, or an
// index on one, and touches nothing the running release reads or writes; kinds and states are varchar,
// never ENUM, and nothing references another table.
func TestSidecarDnsPartsAreAdditive(t *testing.T) {
	refused := regexp.MustCompile(`(?i)\b(DROP|RENAME|ALTER|NOT VALID|TRUNCATE|UPDATE|DELETE|REFERENCES|ENUM|transaction_timeout)\b`)
	parts := []struct {
		name       string
		migrations []any
		statement  *regexp.Regexp
	}{
		{
			name:       "MG2's §12 part",
			migrations: sidecarMg2DnsMigrations(),
			statement:  regexp.MustCompile(`(?is)^\s*(CREATE TABLE dns_label\b|CREATE INDEX dns_label_\w+ ON dns_label\b)`),
		},
		{
			name:       "MG5's §12 part",
			migrations: sidecarMg5DnsMigrations(),
			statement: regexp.MustCompile(`(?is)^\s*(CREATE TABLE (dns_root|network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map|dns_custom_host_map_applied)\b|` +
				`CREATE INDEX (network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map)_\w+ ON (network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map)\b)`),
		},
	}
	for _, part := range parts {
		if len(part.migrations) == 0 {
			t.Fatalf("%s is empty", part.name)
		}
		for index, migration := range part.migrations {
			sqlMigration, ok := migration.(*SqlMigration)
			if !ok {
				t.Fatalf("%s entry %d is a %T", part.name, index, migration)
			}
			if !part.statement.MatchString(sqlMigration.sql) {
				t.Errorf("%s entry %d is not an addition: %s", part.name, index, sqlMigration.sql)
			}
			if refused.MatchString(sqlMigration.sql) {
				t.Errorf("%s entry %d changes what the running release uses: %s", part.name, index, sqlMigration.sql)
			}
		}
	}
}
