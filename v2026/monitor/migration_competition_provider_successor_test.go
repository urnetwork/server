// Exercises exact published prefixes and rolled-back catalog faults through
// the public migration signal, independently of unrelated schema fixtures.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A retired body no longer reports drift, but its historical prefix and each
// replacement still require their own exact artifact in the public adapter.
func TestMigrationsCompetitionSuccessorContractLifetimes(t *testing.T) {
	for _, test := range []struct {
		head, badVersion int
		drift            bool
	}{
		{890, 859, true}, {891, 859, false}, {892, 859, false},
		{891, 891, true}, {892, 891, false}, {892, 892, true},
	} {
		row := syntheticMigrationArtifactRow(test.head)
		// Fixed published positional columns work on the old source as well:
		// before the extension, only the two new columns need padding.
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
			t.Fatalf("published head %d artifact %d drift=%t, want %t", test.head, test.badVersion, got, test.drift)
		}
	}
	for version, removed := range map[int]int{859: 891, 891: 892, 892: 893} {
		artifact := migrationCurrentSuffixTestContract(t, version).artifact
		if artifact.rowColumn != version-589 || artifact.removedVersion != removed {
			t.Fatalf("version %d has wrong column or lifetime", version)
		}
	}
	for _, version := range []int{891, 892} {
		query := migrationCurrentSuffixTestContract(t, version).query
		for _, pin := range []string{"competition_v2_winner_review_provider", "p.proparallel='s'", "p.prosrc=", "competition_v2_winner_review_before_insert", "competition_v2_winner_checkpoint_before_insert"} {
			if !strings.Contains(query, pin) {
				t.Fatalf("version %d omitted exact provider or guard metadata", version)
			}
		}
	}
}

// The actual migration DDL is healthy before and after each replacement;
// missing bodies, weakened metadata and disabled custody stay observable.
func TestMigrationsCompetitionSuccessorPublishedSchemaAndFaults(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, head := range []int{890, 891, 892} {
			server.ApplyDbMigrationsUpTo(ctx, head)
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatalf("coherent published prefix %d reports drift: %s", head, drift)
			}
			faults := []struct{ name, sql string }{
				{"provider body", `CREATE OR REPLACE FUNCTION competition_v2_winner_review_provider(policy_name text,model_name text,reasoning_name text) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT false $$`},
				{"provider parallel metadata", `ALTER FUNCTION competition_v2_winner_review_provider(text,text,text) PARALLEL UNSAFE`},
				{"winner review body", `CREATE OR REPLACE FUNCTION competition_v2_winner_review_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`},
				{"round body", `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`},
				{"source body", `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb,env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS $$ BEGIN RETURN; END $$`},
				{"winner trigger", `ALTER TABLE competition_integrity_review DISABLE TRIGGER competition_v2_winner_review_before_insert`},
			}
			ownerVersion := head
			if head == 890 {
				// The old guard's custody remains required before its successor.
				ownerVersion = 859
				faults = []struct{ name, sql string }{
					{"historical winner trigger", `ALTER TABLE competition_integrity_review DISABLE TRIGGER competition_v2_winner_review_before_insert`},
					{"historical checkpoint trigger", `ALTER TABLE competition_supervisor_transition DISABLE TRIGGER competition_v2_winner_checkpoint_before_insert`},
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
					artifact := migrationCurrentSuffixTestContract(t, ownerVersion).artifact
					matched := false
					for _, alert := range alerts {
						if alert.Class == "migration-schema-drift" && strings.Contains(alert.Markdown(), fmt.Sprintf("%s@v%d", artifact.name, ownerVersion)) {
							matched = true
						}
					}
					if !matched {
						t.Fatalf("head %d lost owning %s fault", head, fault.name)
					}
				})
			}
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatalf("rolled-back faults left drift at %d", head)
			}
		}
	})
}

// Source errors and malformed row counts never become a healthy response;
// private source error text does not become a public alert.
func TestMigrationsCompetitionSuccessorSourceFailureControls(t *testing.T) {
	for _, kind := range []string{"source-error", "no-row", "duplicate-row"} {
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(892), nil
			}
			switch kind {
			case "source-error":
				return nil, errors.New("private-migration-source-canary")
			case "no-row":
				return nil, nil
			default:
				return []Row{syntheticMigrationArtifactRow(892), syntheticMigrationArtifactRow(892)}, nil
			}
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err == nil {
			t.Fatalf("%s was accepted as an observation", kind)
		}
		for _, alert := range alerts {
			if strings.Contains(alert.Markdown(), "private-migration-source-canary") {
				t.Fatal("private source error entered public alert")
			}
		}
	}
}
