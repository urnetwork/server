// Actual published prefixes and rolled-back faults distinguish replacement
// custody from a gap that would silently drop the retained source guard.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// New columns and ownership never hide a missing successor artifact. Old
// bodies remain required through the last head before the real replacement.
func TestMigrationsSnowSuccessorContractCustody(t *testing.T) {
	contract := migrationCurrentSuffixTestContract(t, 893)
	if contract.artifact.rowColumn != 304 || contract.artifact.removedVersion != 0 {
		t.Fatal("Snow artifact column or lifetime is wrong")
	}
	if migrationCurrentSuffixTestContract(t, 892).artifact.removedVersion != 893 || migrationCurrentSuffixTestContract(t, 844).artifact.removedVersion != 0 {
		t.Fatal("replacement retired retained custody")
	}
	for _, pin := range []string{"competition_v2_snow_review_findings_valid(jsonb)", "competition_v2_review_identity_guard()", "competition_v2_source_authorized(jsonb,text,text,text)", "p.proparallel='s'", "p.prosrc=", "competition_v2_winner_checkpoint_before_insert"} {
		if !strings.Contains(contract.query, pin) {
			t.Fatal("Snow predicate omitted full findings or retained custody")
		}
	}
	for _, test := range []struct {
		head, badVersion int
		drift            bool
	}{{892, 892, true}, {893, 892, false}, {893, 893, true}} {
		row := syntheticMigrationArtifactRow(test.head)
		for len(row) <= test.badVersion-589 {
			row = append(row, "t")
		}
		row[test.badVersion-589] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(test.head), nil
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if got := requireAlertClassCount(alerts, "migration-schema-drift") != 0; got != test.drift {
			t.Fatalf("head %d artifact %d has wrong custody", test.head, test.badVersion)
		}
	}
}

// The native published DDL, not look-alike names, is the healthy control.
// Each fault rolls back before the next: no schema or shared fixture cleanup.
func TestMigrationsSnowSuccessorPublishedSchemaAndFaults(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{892, 893} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("coherent prefix %d reports drift: %s", head, drift)
			}
			faults := []struct {
				name, sql string
				owner     int
			}{
				{"historical identity body", `CREATE OR REPLACE FUNCTION competition_v2_review_identity_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`, 844},
				{"historical retained source", `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb,env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS $$ BEGIN RETURN; END $$`, 892},
			}
			if head == 893 {
				faults = []struct {
					name, sql string
					owner     int
				}{
					{"findings missing", `DROP FUNCTION competition_v2_snow_review_findings_valid(jsonb)`, 893},
					{"findings accepts everything", `CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$ BEGIN RETURN true; END $$`, 893},
					{"findings parallel metadata", `ALTER FUNCTION competition_v2_snow_review_findings_valid(jsonb) PARALLEL UNSAFE`, 893},
					{"identity body", `CREATE OR REPLACE FUNCTION competition_v2_review_identity_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`, 893},
					{"identity trigger", `ALTER TABLE competition_integrity_review DISABLE TRIGGER competition_v2_review_identity`, 893},
					{"provider body", `CREATE OR REPLACE FUNCTION competition_v2_winner_review_provider(policy_name text,model_name text,reasoning_name text) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT false $$`, 893},
					{"round body", `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`, 893},
					{"retained mono-root source", `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb,env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS $$ BEGIN RETURN; END $$`, 893},
					{"retained scope lock", `ALTER FUNCTION competition_v2_scope_lock(text,text,text) STABLE`, 844},
				}
			}
			for _, fault := range faults {
				snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
					if _, err := tx.Exec(ctx, fault.sql); err != nil {
						t.Fatalf("apply %s: %v", fault.name, err)
					}
					alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
					if err != nil {
						t.Fatalf("inspect %s: %v", fault.name, err)
					}
					artifact := migrationCurrentSuffixTestContract(t, fault.owner).artifact
					want := fmt.Sprintf("%s@v%d", artifact.name, fault.owner)
					matched := false
					for _, alert := range alerts {
						if alert.Class == "migration-schema-drift" && strings.Contains(alert.Markdown(), want) {
							matched = true
						}
					}
					if !matched {
						t.Fatalf("prefix %d lost owning %s fault", head, fault.name)
					}
				})
			}
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("rolled-back prefix %d faults left drift: %s", head, drift)
			}
		}
	})
}
