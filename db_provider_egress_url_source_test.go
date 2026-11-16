// The URL outcome source migration (db_provider_egress_url_source.go), read
// back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended after the history columns
// it sits beside, and its version is its position.
func TestProviderEgressUrlSourceMigrationIsAppendedAfterTheHistoryColumns(t *testing.T) {
	version := ProviderEgressUrlSourceVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != providerEgressUrlSourceMigration {
		t.Fatalf("URL source migration version %d is not its position", version)
	}
	policy := -1
	for i, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && strings.Contains(sql.sql, "ADD COLUMN url_probe_policy_version") {
			policy = i
		}
	}
	if policy < 0 || version-1 <= policy {
		t.Fatalf("URL source migration %d is not after the policy version column at %d", version, policy+1)
	}
	if verdict := ProviderEgressTlsVerdictVersion(); verdict < 1 || version <= verdict {
		t.Fatalf("URL source migration %d is not after the TLS verdict migration %d", version, verdict)
	}
}

// The columns are constant defaults that read as a scored general outcome,
// added under a five-second lock bound, at most once, after the two new
// country-site tables are created; and nothing rewrites, samples, backfills or
// deletes.
func TestProviderEgressUrlSourceMigrationIsAdditiveAndBounded(t *testing.T) {
	sql := providerEgressUrlSourceMigration.sql
	for _, marker := range []string{
		`attrelid = 'public.provider_egress_health_history'::regclass`,
		`attname = 'url_source' AND NOT attisdropped`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`ADD COLUMN url_source smallint NOT NULL DEFAULT 0,`,
		`ADD COLUMN url_probation boolean NOT NULL DEFAULT false;`,
		`CREATE TABLE IF NOT EXISTS provider_egress_country_site (`,
		`PRIMARY KEY (name, country_code)`,
		`CREATE TABLE IF NOT EXISTS provider_egress_country_site_tally (`,
		// the refresh sums the tally by the network stored with each row
		`network_id uuid NOT NULL,`,
		`PRIMARY KEY (tally_day, name, country_code, client_id)`,
		// six updates a row a day stay on their page
		`) WITH (fillfactor = 70);`,
		// a day made like the history but not attached takes the columns
		`relation.relkind = 'r' AND NOT relation.relispartition`,
		`relation.relname LIKE 'provider\_egress\_health\_history\_p%'`,
		`ADD COLUMN IF NOT EXISTS url_source smallint NOT NULL DEFAULT 0, ADD COLUMN IF NOT EXISTS url_probation boolean NOT NULL DEFAULT false`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("URL source migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("URL source migration bounds its lock wait after it creates its tables")
	}
	if strings.Index(sql, "ALTER TABLE") < strings.LastIndex(sql, "CREATE TABLE") {
		t.Error("URL source migration holds the history's lock while it creates its tables")
	}
	if regexp.MustCompile(`(?i)\b(ANALYZE|VACUUM|UPDATE|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("URL source migration rewrites, samples, backfills, indexes or deletes")
	}
}
