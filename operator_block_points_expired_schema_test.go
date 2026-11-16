// The operator block points expiry migration
// (operator_block_points_expired_schema.go), read back from the migration
// list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, after the
// operator block traffic's, which GET /account/epochs reads it with, and its
// version is its position.
func TestOperatorBlockPointsExpiredMigrationIsAppended(t *testing.T) {
	version := OperatorBlockPointsExpiredVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != operatorBlockPointsExpiredMigration {
		t.Fatalf("operator block points expiry migration version %d is not its position", version)
	}
	if version <= OperatorBlockTrafficVersion() {
		t.Fatalf("operator block points expiry migration %d is not after the operator block traffic's %d", version, OperatorBlockTrafficVersion())
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly its columns and key; nothing existing is altered,
// rewritten, sampled or deleted.
func TestOperatorBlockPointsExpiredMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := operatorBlockPointsExpiredMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`IF to_regclass('public.operator_block_points_expired') IS NULL THEN`,
		`CREATE TABLE operator_block_points_expired (`,
		`block_number bigint NOT NULL,`,
		`window_start timestamp NOT NULL,`,
		`window_end timestamp NOT NULL,`,
		`reliability_min_block_number bigint NOT NULL,`,
		`retention_min_block_number bigint NOT NULL,`,
		`expire_time timestamp NOT NULL,`,
		`create_time timestamp NOT NULL,`,
		`PRIMARY KEY (block_number)`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("operator block points expiry migration lacks %q", marker)
		}
	}
	if strings.Count(sql, "CREATE TABLE") != 1 {
		t.Error("operator block points expiry migration creates more than its table")
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("operator block points expiry migration bounds its lock wait after it creates the table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("operator block points expiry migration touches more than its own new table")
	}
}
