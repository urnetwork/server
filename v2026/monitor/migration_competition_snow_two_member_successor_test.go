// Published successor custody is checked through the public signal, including
// healthy prefixes and rolled-back faults; no historical body is waived early.
package monitor

import (
	"context"
	"fmt"
	"github.com/urnetwork/server/v2026"
	"strings"
	"testing"
)

// The exact lifetime boundary must reject a missing successor without rejecting
// a deliberately superseded predecessor after the successor applies.
func TestMigrationsSnowTwoMemberSuccessorContractCustody(t *testing.T) {
	settlement := migrationCurrentSuffixTestContract(t, 896)
	if settlement.artifact.rowColumn != 307 || settlement.artifact.removedVersion != 0 {
		t.Fatal("settlement checkpoint has wrong column or lifetime")
	}
	contract := migrationCurrentSuffixTestContract(t, 897)
	if contract.artifact.rowColumn != 308 || contract.artifact.removedVersion != 0 || migrationCurrentSuffixTestContract(t, 893).artifact.removedVersion != 897 {
		t.Fatal("two-member successor has wrong lifetime or column")
	}
	for _, pin := range []string{"public.competition_v2_snow_review_findings_valid(jsonb)", "public.competition_v2_snow_review_findings_valid_v2(jsonb)", "public.competition_v2_snow_review_findings_valid_for_policy(jsonb,text)", "competition_v2_source_authorized", "competition_v2_winner_checkpoint_before_insert", "p.prosrc=", "p.proparallel='s'"} {
		if !strings.Contains(contract.query, pin) {
			t.Fatal("successor omitted exact findings, metadata or retained custody")
		}
	}
	for _, test := range []struct {
		head, bad int
		drift     bool
	}{{896, 893, true}, {897, 893, false}, {897, 897, true}} {
		row := syntheticMigrationArtifactRow(test.head)
		for len(row) <= test.bad-589 {
			row = append(row, "t")
		}
		row[test.bad-589] = "f"
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
			t.Fatal("successor lifetime misclassified artifact")
		}
	}
}

// Actual migrated DDL is the healthy control. Each fault rolls back, retaining
// the findings census, selector, exact metadata and inherited source authority.
func TestMigrationsSnowTwoMemberSuccessorPublishedSchemaAndFaults(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{893, 896, 897} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("coherent prefix %d reports drift: %s", head, drift)
			}
			if head != 897 {
				continue
			}
			oldProvider, _, _, _, _, _ := server.CompetitionSnowWinnerReviewBodiesSql()
			faults := []struct{ name, sql string }{
				{"legitimate historical provider restored", `CREATE OR REPLACE FUNCTION competition_v2_winner_review_provider(policy_name text,model_name text,reasoning_name text) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $historical$` + oldProvider + `$historical$`},
				{"v2 findings absent with same-name overload", `DROP FUNCTION competition_v2_snow_review_findings_valid_v2(jsonb); CREATE FUNCTION competition_v2_snow_review_findings_valid_v2(request text) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT true $$`},
				{"policy dispatcher absent", `DROP FUNCTION competition_v2_snow_review_findings_valid_for_policy(jsonb,text)`},
				{"v1 findings weakened", `CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$ BEGIN RETURN true; END $$`},
				{"v2 findings weakened", `CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid_v2(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$ BEGIN RETURN true; END $$`},
				{"unknown selector accepts", `CREATE OR REPLACE FUNCTION competition_v2_snow_review_findings_valid_for_policy(request jsonb,policy_name text) RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT true $$`},
				{"selector metadata", `ALTER FUNCTION competition_v2_snow_review_findings_valid_for_policy(jsonb,text) PARALLEL UNSAFE`},
				{"identity trigger", `ALTER TABLE competition_integrity_review DISABLE TRIGGER competition_v2_review_identity`},
				{"retained source", `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb,env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS $$ BEGIN RETURN; END $$`},
			}
			for _, fault := range faults {
				snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
					if _, err := tx.Exec(ctx, fault.sql); err != nil {
						t.Fatalf("apply %s: %v", fault.name, err)
					}
					alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
					if err != nil {
						t.Fatal(err)
					}
					want := fmt.Sprintf("%s@v897", migrationCurrentSuffixTestContract(t, 897).artifact.name)
					matched := false
					for _, alert := range alerts {
						if alert.Class == "migration-schema-drift" && strings.Contains(alert.Markdown(), want) {
							matched = true
						}
					}
					if !matched {
						t.Fatalf("%s lost successor custody", fault.name)
					}
				})
			}
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("rolled-back successor faults left drift: %s", drift)
			}
		}
	})
}

// The omitted predecessor column would shift every later emitted artifact.
// Both exact column shapes must survive the actual successor and every fault.
func TestMigrationsSettlementCompletionPublishedSchemaAndFaults(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{896, 897} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("settlement prefix %d reports drift: %s", head, drift)
			}
			faults := []struct{ name, sql string }{
				{"completion absent", `ALTER TABLE contract_close DROP COLUMN settlement_completion`},
				{"completion wrong type", `ALTER TABLE contract_close ALTER COLUMN settlement_completion TYPE bigint`},
				{"completion nullable", `ALTER TABLE contract_close ALTER COLUMN settlement_completion DROP NOT NULL`},
				{"completion default absent", `ALTER TABLE contract_close ALTER COLUMN settlement_completion DROP DEFAULT`},
				{"completion default wrong", `ALTER TABLE contract_close ALTER COLUMN settlement_completion SET DEFAULT 1`},
				{"checkpoint absent", `ALTER TABLE contract_close DROP COLUMN settlement_completion_checkpoint_byte_count`},
				{"checkpoint wrong type", `ALTER TABLE contract_close ALTER COLUMN settlement_completion_checkpoint_byte_count TYPE integer`},
				{"checkpoint required", `ALTER TABLE contract_close ALTER COLUMN settlement_completion_checkpoint_byte_count SET NOT NULL`},
				{"checkpoint default invented", `ALTER TABLE contract_close ALTER COLUMN settlement_completion_checkpoint_byte_count SET DEFAULT 0`},
			}
			for _, fault := range faults {
				snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
					if _, err := tx.Exec(ctx, fault.sql); err != nil {
						t.Fatalf("apply %s: %v", fault.name, err)
					}
					alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
					if err != nil {
						t.Fatal(err)
					}
					want := fmt.Sprintf("%s@v896", migrationCurrentSuffixTestContract(t, 896).artifact.name)
					matched := false
					for _, alert := range alerts {
						if alert.Class == "migration-schema-drift" && strings.Contains(alert.Markdown(), want) {
							matched = true
						}
					}
					if !matched {
						t.Fatalf("%s lost settlement column custody", fault.name)
					}
				})
			}
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("rolled-back settlement faults left drift: %s", drift)
			}
		}
	})
}
