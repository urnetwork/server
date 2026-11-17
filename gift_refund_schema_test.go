// The gift refund migration (gift_refund_schema.go), read back from the
// migration list.
package server

import (
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended after the gift schema's,
// and its version is its position.
func TestGiftRefundSchemaMigrationIsAppended(t *testing.T) {
	version := GiftRefundSchemaVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != giftRefundSchemaMigration {
		t.Fatalf("gift refund migration version %d is not its position", version)
	}
	if version <= GiftSchemaVersion() {
		t.Fatalf("gift refund migration %d is not after the gift schema's %d", version, GiftSchemaVersion())
	}
}

// The tables are created only when absent, with exactly their columns, keys
// and indexes, under a five-second lock bound set first; nothing existing is
// altered, rewritten or deleted.
func TestGiftRefundSchemaMigrationIsBoundedAndAdditive(t *testing.T) {
	sql := giftRefundSchemaMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`IF to_regclass('public.gift_code_void') IS NULL THEN`,
		`CREATE TABLE gift_code_void (`,
		`balance_code_id uuid NOT NULL,`,
		`gift_order_id uuid NOT NULL,`,
		`cause varchar(16) NOT NULL,`,
		`source_id varchar(128) NOT NULL,`,
		`ended_year boolean NOT NULL,`,
		`void_time timestamp NOT NULL,`,
		`PRIMARY KEY (balance_code_id)`,
		`CREATE INDEX gift_code_void_order ON gift_code_void (gift_order_id, cause, source_id);`,
		`IF to_regclass('public.gift_order_refund') IS NULL THEN`,
		`CREATE TABLE gift_order_refund (`,
		`refund_id varchar(128) NOT NULL,`,
		`amount_cents bigint NOT NULL,`,
		`refund_time timestamp NOT NULL,`,
		`PRIMARY KEY (refund_id)`,
		`CREATE INDEX gift_order_refund_order ON gift_order_refund (gift_order_id);`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("the migration does not contain %q", marker)
		}
	}
	if strings.Index(sql, `set_config('lock_timeout'`) > strings.Index(sql, `CREATE TABLE`) {
		t.Error("the lock bound is not set first")
	}
	for _, refused := range []string{"ALTER TABLE", "DROP ", "DELETE ", "UPDATE ", "ENUM", "CONCURRENTLY"} {
		if strings.Contains(strings.ToUpper(sql), refused) {
			t.Errorf("the migration contains %q", refused)
		}
	}
}
