// The Solana receiver sweep cursor migration (solana_receiver_sweep_schema.go),
// read back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, and its
// version is its position.
func TestSolanaReceiverSweepCursorMigrationIsAppended(t *testing.T) {
	version := SolanaReceiverSweepCursorVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != solanaReceiverSweepCursorMigration {
		t.Fatalf("receiver sweep cursor migration version %d is not its position", version)
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly the cursor's columns; nothing existing is altered,
// rewritten, sampled or deleted.
func TestSolanaReceiverSweepCursorMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := solanaReceiverSweepCursorMigration.sql
	for _, marker := range []string{
		`IF to_regclass('public.solana_receiver_sweep_cursor') IS NULL THEN`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`CREATE TABLE solana_receiver_sweep_cursor (`,
		`token_account varchar(64) NOT NULL,`,
		`receiver varchar(64) NOT NULL,`,
		`signature varchar(128) NOT NULL,`,
		`slot bigint NOT NULL,`,
		`block_time timestamp NULL,`,
		`update_time timestamp NOT NULL,`,
		`PRIMARY KEY (token_account)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("receiver sweep cursor migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("receiver sweep cursor migration bounds its lock wait after it creates the table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("receiver sweep cursor migration touches more than its own new table")
	}
}
