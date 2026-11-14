// The Shopify order payment migration (solana_shopify_order_schema.go), read
// back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, after the
// issued address sweep's, and its version is its position.
func TestSolanaShopifyOrderPaymentMigrationIsAppended(t *testing.T) {
	version := SolanaShopifyOrderPaymentVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != solanaShopifyOrderPaymentMigration {
		t.Fatalf("Shopify order payment migration version %d is not its position", version)
	}
	if version <= CryptoAddressSweepVersion() {
		t.Fatalf("Shopify order payment migration %d is not after the issued address sweep's %d", version, CryptoAddressSweepVersion())
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly its columns and keys; nothing existing is altered,
// rewritten, sampled or deleted.
func TestSolanaShopifyOrderPaymentMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := solanaShopifyOrderPaymentMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`IF to_regclass('public.solana_shopify_order_payment') IS NULL THEN`,
		`CREATE TABLE solana_shopify_order_payment (`,
		`tx_signature varchar(128) NOT NULL,`,
		`order_id varchar(128) NOT NULL,`,
		`order_name varchar(64) NOT NULL,`,
		`payment_id varchar(128) NOT NULL,`,
		`received_usd double precision NOT NULL,`,
		`order_total_usd double precision NOT NULL,`,
		`transaction_time timestamp NULL,`,
		`leg varchar(16) NOT NULL,`,
		`record_time timestamp NOT NULL,`,
		`PRIMARY KEY (tx_signature),`,
		`UNIQUE (order_id)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("Shopify order payment migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("Shopify order payment migration bounds its lock wait after it creates the table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("Shopify order payment migration touches more than its own new table")
	}
}
