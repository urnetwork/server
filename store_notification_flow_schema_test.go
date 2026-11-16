// The store notification flow migration (store_notification_flow_schema.go),
// read back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended to the list, and its
// version is its position.
func TestStoreNotificationFlowMigrationIsAppended(t *testing.T) {
	version := StoreNotificationFlowVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != storeNotificationFlowMigration {
		t.Fatalf("store notification flow migration version %d is not its position", version)
	}
}

// The table is created only when absent, under a five-second lock bound set
// before it, with exactly its columns and one row per store whose clock
// starts at the migration; nothing existing is altered, rewritten or deleted.
func TestStoreNotificationFlowMigrationCreatesOnlyItsTableAndIsBounded(t *testing.T) {
	sql := storeNotificationFlowMigration.sql
	for _, marker := range []string{
		`IF to_regclass('public.store_notification_flow') IS NULL THEN`,
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`CREATE TABLE store_notification_flow (`,
		`store varchar(32) NOT NULL,`,
		`last_accepted_at timestamp NOT NULL,`,
		`update_time timestamp NOT NULL,`,
		`PRIMARY KEY (store)`,
		`INSERT INTO store_notification_flow (store, last_accepted_at, update_time)`,
		`('apple', timezone('UTC', now()), timezone('UTC', now())),`,
		`('google', timezone('UTC', now()), timezone('UTC', now()));`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("store notification flow migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") {
		t.Error("store notification flow migration bounds its lock wait after it creates the table")
	}
	if strings.Index(sql, "INSERT INTO") < strings.Index(sql, "CREATE TABLE") {
		t.Error("store notification flow migration writes before it creates its table")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("store notification flow migration touches more than its own new table")
	}
	if strings.Count(sql, "INSERT INTO") != 1 || !strings.Contains(sql, "INSERT INTO store_notification_flow") {
		t.Error("store notification flow migration writes another table")
	}
}
