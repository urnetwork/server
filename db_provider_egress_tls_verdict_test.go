// The TLS verdict migration (db_provider_egress_tls_verdict.go), read back
// from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended after the security table
// it extends, and its version is its position.
func TestProviderEgressTlsVerdictMigrationIsAppendedAfterTheSecurityTable(t *testing.T) {
	version := ProviderEgressTlsVerdictVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != providerEgressTlsVerdictMigration {
		t.Fatalf("TLS verdict migration version %d is not its position", version)
	}
	created := -1
	for i, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && strings.Contains(sql.sql, "CREATE TABLE provider_egress_url_security") {
			created = i
		}
	}
	if created < 0 || version-1 <= created {
		t.Fatalf("TLS verdict migration %d is not after the security table at %d", version, created+1)
	}
}

// The column is a constant default added under a five-second lock bound, at
// most once; every unresolved finding starts excluding as before; the
// projection is recomputed through its partial index; and nothing rewrites,
// samples or deletes.
func TestProviderEgressTlsVerdictMigrationKeepsExclusionsAndIsBounded(t *testing.T) {
	sql := providerEgressTlsVerdictMigration.sql
	for _, marker := range []string{
		`attname = 'tls_verdict' AND NOT attisdropped`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`ADD COLUMN tls_verdict smallint NOT NULL DEFAULT 0,`,
		`ADD COLUMN tls_verdict_measured_at timestamp NULL;`,
		`UPDATE provider_egress_url_security SET tls_verdict = 1 WHERE tls_failure;`,
		`WHERE health.tls_authentication_failure = true;`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("TLS verdict migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "ALTER TABLE") {
		t.Error("TLS verdict migration bounds its lock wait after it alters the table")
	}
	if regexp.MustCompile(`(?i)\b(ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER)\b`).MatchString(sql) {
		t.Error("TLS verdict migration rewrites, samples or deletes")
	}
}
