// The gift code migration (gift_schema.go), read back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, after the mail
// context's, and its version is its position.
func TestGiftSchemaMigrationIsAppended(t *testing.T) {
	version := GiftSchemaVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != giftSchemaMigration {
		t.Fatalf("gift schema migration version %d is not its position", version)
	}
	if version <= NetworkMailContextVersion() {
		t.Fatalf("gift schema migration %d is not after the mail context's %d", version, NetworkMailContextVersion())
	}
}

// The code columns are added only when absent, kind with the constant default
// every existing code reads and the rest nullable without one; the tables are
// created only when absent, with exactly their columns and keys; all under a
// five-second lock bound set first. Nothing existing is rewritten, sampled or
// deleted, no index is built on the codes, and no kind is an ENUM.
func TestGiftSchemaMigrationIsBoundedAndAdditive(t *testing.T) {
	sql := giftSchemaMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`ALTER TABLE transfer_balance_code`,
		`ADD COLUMN IF NOT EXISTS kind varchar(16) NOT NULL DEFAULT 'data',`,
		`ADD COLUMN IF NOT EXISTS pro_duration_seconds bigint NULL,`,
		`ADD COLUMN IF NOT EXISTS gift_order_id uuid NULL,`,
		`ADD COLUMN IF NOT EXISTS referrer_network_id uuid NULL;`,

		`IF to_regclass('public.gift_order') IS NULL THEN`,
		`CREATE TABLE gift_order (`,
		`gift_order_id uuid NOT NULL,`,
		`order_token_hash bytea NOT NULL,`,
		`create_time timestamp NOT NULL,`,
		`status varchar(16) NOT NULL,`,
		`paid_time timestamp NULL,`,
		`refunded_time timestamp NULL,`,
		`quantity integer NOT NULL,`,
		`unit_price_cents bigint NOT NULL,`,
		`total_cents bigint NOT NULL,`,
		`rail varchar(16) NOT NULL,`,
		`stripe_session_id varchar(128) NULL,`,
		`crypto_intent_id uuid NULL,`,
		`purchase_event_base varchar(128) NULL,`,
		`redeem_by timestamp NULL,`,
		`delivery varchar(16) NOT NULL,`,
		`buyer_email varchar(256) NOT NULL,`,
		`send_on date NULL,`,
		`send_timezone varchar(64) NOT NULL,`,
		`message text NOT NULL,`,
		`signature text NOT NULL,`,
		`language varchar(16) NOT NULL,`,
		`season varchar(64) NOT NULL,`,
		`bittensor_footer boolean NOT NULL,`,
		`referrer_network_id uuid NULL,`,
		`PRIMARY KEY (gift_order_id),`,
		`UNIQUE (stripe_session_id),`,
		`UNIQUE (crypto_intent_id)`,

		`IF to_regclass('public.gift_order_recipient') IS NULL THEN`,
		`CREATE TABLE gift_order_recipient (`,
		`recipient_index integer NOT NULL,`,
		`email varchar(256) NOT NULL,`,
		`balance_code_id uuid NULL,`,
		`PRIMARY KEY (gift_order_id, recipient_index)`,

		`IF to_regclass('public.gift_order_pdf') IS NULL THEN`,
		`CREATE TABLE gift_order_pdf (`,
		`part varchar(16) NOT NULL,`,
		`pdf bytea NOT NULL,`,
		`PRIMARY KEY (gift_order_id, part)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("gift schema migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "ALTER TABLE") {
		t.Error("gift schema migration bounds its lock wait after it alters the codes")
	}
	if regexp.MustCompile(`(?i)\b(UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX|ENUM)\b`).MatchString(sql) {
		t.Error("gift schema migration rewrites, scans or removes data, or declares an ENUM")
	}
	if strings.Count(sql, "CREATE TABLE") != 3 || strings.Count(sql, "ADD COLUMN IF NOT EXISTS") != 4 {
		t.Error("gift schema migration creates or adds more than its three tables and four columns")
	}
	if regexp.MustCompile(`(?i)(pro_duration_seconds|gift_order_id|referrer_network_id) [a-z]+ NULL[^,;]*DEFAULT`).MatchString(sql) {
		t.Error("a nullable gift code column has a default")
	}
}
