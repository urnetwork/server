// The gift gamma migration (gift_gamma_schema.go), read back from the
// migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended after the gift refund
// schema's, and its version is its position.
func TestGiftGammaSchemaMigrationIsAppended(t *testing.T) {
	version := GiftGammaSchemaVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != giftGammaSchemaMigration {
		t.Fatalf("gift gamma migration version %d is not its position", version)
	}
	if version <= GiftRefundSchemaVersion() {
		t.Fatalf("gift gamma migration %d is not after the gift refund schema's %d", version, GiftRefundSchemaVersion())
	}
	if version <= GammaPaySchemaMigrationVersion() {
		t.Fatalf("gift gamma migration %d is not after the gamma schema's %d", version, GammaPaySchemaMigrationVersion())
	}
	t.Logf("gift gamma migration: version %d of %d", version, len(migrations))
}

// The column and its unique constraint are each added only when absent, the
// column nullable with no default, under a five-second lock bound set first;
// nothing else is altered, and nothing is rewritten, updated or deleted.
func TestGiftGammaSchemaMigrationIsBoundedAndAdditive(t *testing.T) {
	sql := giftGammaSchemaMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`ALTER TABLE gift_order`,
		`ADD COLUMN IF NOT EXISTS gamma_intent_id uuid NULL;`,
		`conname = 'gift_order_gamma_intent_id_key'`,
		`ADD CONSTRAINT gift_order_gamma_intent_id_key UNIQUE (gamma_intent_id);`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("the migration does not contain %q", marker)
		}
	}
	if strings.Index(sql, `set_config('lock_timeout'`) > strings.Index(sql, `ALTER TABLE`) {
		t.Error("the lock bound is not set first")
	}
	if strings.Count(sql, "ALTER TABLE gift_order") != 2 {
		t.Error("the migration alters more than gift_order's new column and its constraint")
	}
	if regexp.MustCompile(`(?i)\b(UPDATE|INSERT|DELETE|TRUNCATE|DROP|VACUUM|ANALYZE|CLUSTER|ENUM|DEFAULT|NOT NULL|CONCURRENTLY|CREATE TABLE|CREATE INDEX)\b`).MatchString(sql) {
		t.Error("the migration does more than add the nullable column and its unique constraint")
	}
}
