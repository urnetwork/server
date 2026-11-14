// The per-cycle Pro grant migration (db_pro_grant_cycle.go), read back from the
// migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, after the Gamma
// schema whose credit path writes paid_end_time, and its version is its
// position.
func TestProGrantCycleMigrationIsAppended(t *testing.T) {
	version := ProGrantCycleVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != proGrantCycleMigration {
		t.Fatalf("Pro grant cycle migration version %d is not its position", version)
	}
	if ProviderZeroCarryVersion() >= version {
		t.Fatalf("Pro grant cycle migration %d is not after the zero-carry migration %d", version, ProviderZeroCarryVersion())
	}
}

// The column is nullable with no default (a hold has no paid end, and a row from
// before the column is read by its market's grace), the table is created only
// when absent, and both run under a five-second lock bound set first. Nothing
// existing is rewritten, sampled or deleted.
func TestProGrantCycleMigrationIsBoundedAndAdditive(t *testing.T) {
	sql := proGrantCycleMigration.sql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`ALTER TABLE subscription_renewal ADD COLUMN IF NOT EXISTS paid_end_time timestamp NULL;`,
		`IF to_regclass('public.pro_grant_cycle') IS NULL THEN`,
		`CREATE TABLE pro_grant_cycle (`,
		`network_id uuid NOT NULL,`,
		`cycle_start_time timestamp NOT NULL,`,
		`cycle_end_time timestamp NOT NULL,`,
		`balance_id uuid NULL,`,
		`ended_time timestamp NULL,`,
		`create_time timestamp NOT NULL DEFAULT now(),`,
		`PRIMARY KEY (network_id, cycle_start_time)`,
		`CREATE INDEX pro_grant_cycle_cycle_end_time ON pro_grant_cycle (cycle_end_time);`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("Pro grant cycle migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "ALTER TABLE") {
		t.Error("Pro grant cycle migration bounds its lock wait after it alters the renewal table")
	}
	if regexp.MustCompile(`(?i)\b(UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER)\b`).MatchString(sql) {
		t.Error("Pro grant cycle migration rewrites or removes data")
	}
	if regexp.MustCompile(`(?i)paid_end_time timestamp[^;]*DEFAULT`).MatchString(sql) {
		t.Error("the paid end has a default; a hold would read as paid")
	}
}
