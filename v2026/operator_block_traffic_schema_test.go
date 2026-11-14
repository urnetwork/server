// The operator block traffic migration (operator_block_traffic_schema.go),
// read back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list after the
// gift refunds, and its version is its position.
func TestOperatorBlockTrafficMigrationIsAppended(t *testing.T) {
	version := OperatorBlockTrafficVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != operatorBlockTrafficMigration {
		t.Fatalf("operator block traffic migration version %d is not its position", version)
	}
	if version <= GiftRefundSchemaVersion() {
		t.Fatalf("operator block traffic migration %d is not after the gift refunds' %d", version, GiftRefundSchemaVersion())
	}
}

// The two tables are created only when absent, under a five-second lock
// bound set before them, with exactly the rollup's columns and primary keys;
// nothing existing is altered, rewritten, sampled or deleted, and no index is
// built on another table.
func TestOperatorBlockTrafficMigrationCreatesOnlyItsTablesAndIsBounded(t *testing.T) {
	sql := operatorBlockTrafficMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`IF to_regclass('public.operator_block_traffic') IS NULL THEN`,
		`CREATE TABLE operator_block_traffic (`,
		`block_number bigint NOT NULL,`,
		`read_through timestamp NOT NULL,`,
		`update_time timestamp NOT NULL,`,
		`PRIMARY KEY (block_number)`,
		`IF to_regclass('public.operator_block_network_traffic') IS NULL THEN`,
		`CREATE TABLE operator_block_network_traffic (`,
		`network_id uuid NOT NULL,`,
		`payout_byte_count bigint NOT NULL,`,
		`PRIMARY KEY (block_number, network_id)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("operator block traffic migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("operator block traffic migration bounds its lock wait after it creates a table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("operator block traffic migration touches more than its own new tables")
	}
}
