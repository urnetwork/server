//go:build sidecar_pending

package server

import (
	"regexp"
	"testing"
)

// CODESTYLE's migration rules for §12's parts (DNS-M5-09, DNS-M6-01, DNS-M6-02): each entry creates one of
// §12.14's tables, or an index on one, or adds one nullable column with no default to a table an earlier
// part creates, and touches nothing the running release reads or writes; kinds and states are varchar,
// never ENUM, and nothing references another table.
func TestSidecarDnsPartsAreAdditive(t *testing.T) {
	refused := regexp.MustCompile(`(?i)\b(DROP|RENAME|ALTER|NOT VALID|TRUNCATE|UPDATE|DELETE|REFERENCES|ENUM|transaction_timeout)\b`)
	refusedButAlter := regexp.MustCompile(`(?i)\b(DROP|RENAME|NOT VALID|TRUNCATE|UPDATE|DELETE|REFERENCES|ENUM|transaction_timeout|SET NOT NULL|DEFAULT)\b`)
	parts := []struct {
		name       string
		migrations []any
		statement  *regexp.Regexp
		// the part adds nullable columns, which ALTER names
		allowAlter bool
	}{
		{
			name:       "MG2's §12 part",
			migrations: sidecarMg2DnsMigrations(),
			statement:  regexp.MustCompile(`(?is)^\s*(CREATE TABLE dns_label\b|CREATE INDEX dns_label_\w+ ON dns_label\b)`),
		},
		{
			name:       "MG5's §12 part",
			migrations: sidecarMg5DnsMigrations(),
			statement: regexp.MustCompile(`(?is)^\s*(CREATE TABLE (dns_root|network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map|dns_custom_host_map_applied|dns_restore_incident)\b|` +
				`CREATE INDEX (network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map|dns_restore_incident)_\w+ ON (network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map|dns_restore_incident)\b)`),
		},
		{
			// nullable columns on tables earlier parts create, and an index on MG6's own network_domain
			name:       "MG6's §12 part",
			migrations: sidecarMg6DnsMigrations(),
			statement: regexp.MustCompile(`(?is)^\s*(ALTER TABLE dns_label ADD COLUMN certified_time timestamp NULL\s*$|` +
				`ALTER TABLE network_domain ADD COLUMN acme_token varchar\(26\) NULL\s*$|` +
				`CREATE UNIQUE INDEX network_domain_acme_token ON network_domain \(acme_token\) WHERE acme_token IS NOT NULL\s*$)`),
			allowAlter: true,
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
			partRefused := refused
			if part.allowAlter {
				partRefused = refusedButAlter
			}
			if partRefused.MatchString(sqlMigration.sql) {
				t.Errorf("%s entry %d changes what the running release uses: %s", part.name, index, sqlMigration.sql)
			}
		}
	}
}
