// The Bitcoin block scanner's kept-chain migration (db_crypto_scan_chain.go),
// read back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list after the
// crypto payment rail's tables, and its version is its position.
func TestCryptoScanChainMigrationIsAppended(t *testing.T) {
	version := CryptoScanChainVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != cryptoScanChainMigration {
		t.Fatalf("kept-chain migration version %d is not its position", version)
	}
	created := 0
	for i, migration := range migrations {
		if sqlMigration, ok := migration.(*SqlMigration); ok && strings.Contains(sqlMigration.sql, "CREATE TABLE crypto_scan_cursor") {
			created = i + 1
		}
	}
	if created < 1 || version <= created {
		t.Fatalf("kept-chain migration %d is not after the migration %d that creates crypto_scan_cursor", version, created)
	}
}

// It only adds the nullable column, when absent, under a five-second lock
// bound set before it: no row is rewritten, nothing else is altered.
func TestCryptoScanChainMigrationAddsOnlyItsColumnAndIsBounded(t *testing.T) {
	sql := cryptoScanChainMigration.sql
	for _, marker := range []string{
		`SET LOCAL lock_timeout = '5s';`,
		`ALTER TABLE crypto_scan_cursor ADD COLUMN IF NOT EXISTS recent_blocks jsonb NULL;`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("kept-chain migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "ALTER TABLE") {
		t.Error("kept-chain migration bounds its lock wait after it alters the table")
	}
	if strings.Count(sql, "ALTER") != 1 {
		t.Error("kept-chain migration alters more than the one column")
	}
	if regexp.MustCompile(`(?i)\b(UPDATE|INSERT|DELETE|TRUNCATE|DROP|DEFAULT|NOT NULL|INDEX|VACUUM|ANALYZE|CLUSTER|CREATE)\b`).MatchString(sql) {
		t.Error("kept-chain migration does more than add one nullable column")
	}
}
