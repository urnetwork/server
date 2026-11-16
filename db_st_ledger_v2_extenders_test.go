// The SN25 extender pool's migration (db_st_ledger_v2_extenders.go), read
// back from the migration list.
package server

import (
	"regexp"
	"strings"
	"testing"
)

// The migration is one plain SQL migration, after the settlement v2 schema
// whose epoch rows it adds six columns to, and its version is its position.
func TestStLedgerV2ExtenderMigrationIsAppended(t *testing.T) {
	version := sqlMigrationVersion(StLedgerV2ExtenderSchemaSql)
	if version < 1 || migrations[version-1] != stLedgerV2ExtenderMigration {
		t.Fatalf("extender pool migration version %d is not its position", version)
	}
	if v2 := sqlMigrationIndex(t, "CREATE TABLE st_ledger_v2_epoch_rows ("); version-1 <= v2 {
		t.Fatalf("extender pool migration %d is not after the v2 schema %d", version-1, v2)
	}
	if demand := StDemandSchemaMigrationVersion(); demand < 1 || version <= demand {
		t.Fatalf("extender pool migration %d is not after the demand schema %d, whose price sheet store it mirrors", version, demand)
	}
}

// The tables are created only when absent and the columns added only when
// absent, under a five-second lock bound set before anything else, with
// exactly the columns the model reads; nothing existing is rewritten.
func TestStLedgerV2ExtenderMigrationCreatesOnlyItsStateAndIsBounded(t *testing.T) {
	sql := StLedgerV2ExtenderSchemaSql
	for _, marker := range []string{
		`PERFORM set_config('lock_timeout', '5s', true);`,
		`IF to_regclass('public.st_amendments') IS NULL THEN`,
		`CREATE TABLE st_amendments (`,
		`raw bytea NOT NULL,`,
		`IF to_regclass('public.st_ledger_v2_extender_census') IS NULL THEN`,
		`CREATE TABLE st_ledger_v2_extender_census (`,
		`census bytea NOT NULL,`,
		`census_rows bigint NOT NULL CHECK (census_rows >= 0),`,
		`capture_time timestamp NOT NULL,`,
		`PRIMARY KEY (deployment_key, no_id, epoch)`,
		`IF to_regclass('public.st_ledger_v2_extender_shadow') IS NULL THEN`,
		`CREATE TABLE st_ledger_v2_extender_shadow (`,
		`report_ref varchar(71) NOT NULL,`,
		`report bytea NOT NULL,`,
		`IF to_regclass('public.st_platform_endpoint') IS NULL THEN`,
		`CREATE TABLE st_platform_endpoint (`,
		`kind varchar(16) NOT NULL CHECK (kind IN ('tlsedge', 'smtpedge', 'mxedge')),`,
		`PRIMARY KEY (client_id, register_time),`,
		`CHECK (revoke_time IS NULL OR register_time < revoke_time),`,
		`ADD COLUMN IF NOT EXISTS extender_bytes numeric(20,0) NULL,`,
		`ADD COLUMN IF NOT EXISTS extender_census_chunks text[] NULL,`,
		`ADD COLUMN IF NOT EXISTS extender_intra_platform_bytes numeric(20,0) NULL,`,
		`ADD COLUMN IF NOT EXISTS extender_intra_network_bytes numeric(20,0) NULL,`,
		`ADD COLUMN IF NOT EXISTS extender_contracts bigint NULL,`,
		`ADD COLUMN IF NOT EXISTS extender_payers bigint NULL;`,
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("extender pool migration lacks %q", marker)
		}
	}
	if strings.Index(sql, "lock_timeout") > strings.Index(sql, "CREATE TABLE") || strings.Index(sql, "lock_timeout") > strings.Index(sql, "ALTER TABLE") {
		t.Error("extender pool migration bounds its lock wait after it changes the catalog")
	}
	if regexp.MustCompile(`(?i)\b(UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|INDEX)\b`).MatchString(sql) {
		t.Error("extender pool migration rewrites or drops existing state")
	}
	if altered := regexp.MustCompile(`(?i)ALTER TABLE (\w+)`).FindAllStringSubmatch(sql, -1); len(altered) != 1 || altered[0][1] != "st_ledger_v2_epoch_rows" {
		t.Errorf("extender pool migration alters %v, want only st_ledger_v2_epoch_rows", altered)
	}
}
