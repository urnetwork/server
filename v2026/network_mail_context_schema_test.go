// The mail context migration (network_mail_context_schema.go), read back from
// the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, after the
// Shopify order payment's, and its version is its position.
func TestNetworkMailContextMigrationIsAppended(t *testing.T) {
	version := NetworkMailContextVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != networkMailContextMigration {
		t.Fatalf("mail context migration version %d is not its position", version)
	}
	if version <= SolanaShopifyOrderPaymentVersion() {
		t.Fatalf("mail context migration %d is not after the Shopify order payment's %d", version, SolanaShopifyOrderPaymentVersion())
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly its columns and key; nothing existing is altered,
// rewritten, sampled or deleted.
func TestNetworkMailContextMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := networkMailContextMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`IF to_regclass('public.network_mail_context') IS NULL THEN`,
		`CREATE TABLE network_mail_context (`,
		`network_id uuid NOT NULL,`,
		`time_zone varchar(64) NOT NULL DEFAULT '',`,
		`time_zone_time timestamp NULL,`,
		`languages text[] NOT NULL DEFAULT '{}'::text[],`,
		`languages_time timestamp NULL,`,
		`clients jsonb NOT NULL DEFAULT '[]'::jsonb,`,
		`update_time timestamp NOT NULL,`,
		`PRIMARY KEY (network_id)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("mail context migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("mail context migration bounds its lock wait after it creates the table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("mail context migration touches more than its own new table")
	}
}
