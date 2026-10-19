// The real v850 transition transfers exact function custody while retaining
// v847 lifecycle and index guards. Every fault is rolled back independently.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Old bodies must still be enforced immediately before their true successor.
func TestMigrationsSourceGraphHistoricalAuthorityCustody(t *testing.T) {
	var observations [][]Alert
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.ApplyDbMigrationsUpTo(t.Context(), 849)
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published pre-successor source graph reports drift: %s", drift)
		}
		for _, statement := range []string{
			`CREATE OR REPLACE FUNCTION competition_v2_source_mode(intent jsonb) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`,
			`CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`,
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, statement); err != nil {
					t.Fatal(err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatal(err)
				}
				observations = append(observations, alerts)
			})
		}
	})
	if len(observations) != 2 {
		t.Fatalf("observed %d historical authority faults", len(observations))
	}
	for _, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), "isolated automation staging and current lifecycle guards@v847") {
			t.Fatal("pre-successor authority fault lost v847 custody")
		}
	}
}

// Numeric head, matching names, old legitimate bodies and unchanged source
// functions cannot substitute for the exact published successor graph.
func TestMigrationsSourceGraphSchemaFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name string
		sql  string
		want string
	}{
		{name: "legacy source mode restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_mode(intent jsonb) RETURNS void LANGUAGE plpgsql AS $regression$` + competitionMigration847CompetitionV2SourceModeBody + `$regression$`, want: "staging source graph of ten repositories@v850"},
		{name: "legacy source authority restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text) RETURNS void LANGUAGE plpgsql AS $regression$` + competitionMigration847CompetitionV2SourceAuthorizedBody + `$regression$`, want: "staging source graph of ten repositories@v850"},
		{name: "graph validator always accepts", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid(commits jsonb, staging_mode boolean) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS 'BEGIN RETURN true; END'`, want: "staging source graph of ten repositories@v850"},
		{name: "graph security privilege changed", sql: `ALTER FUNCTION competition_v2_source_graph_valid(jsonb,boolean) SECURITY DEFINER`, want: "staging source graph of ten repositories@v850"},
		{name: "graph volatility changed", sql: `ALTER FUNCTION competition_v2_source_graph_valid(jsonb,boolean) VOLATILE`, want: "staging source graph of ten repositories@v850"},
		{name: "retained seal guard changed", sql: `CREATE OR REPLACE FUNCTION competition_v2_seal_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`, want: "isolated automation staging and current lifecycle guards@v847"},
		{name: "retained staging index removed", sql: `DROP INDEX competition_round_v2_staging_epoch`, want: "isolated automation staging and current lifecycle guards@v847"},
	}
	var observations [][]Alert
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published source graph schema reports drift: %s", drift)
		}
		artifact := migrationCurrentSuffixTestContract(t, 850).artifact
		if artifact.rowColumn != 261 {
			t.Fatal("source graph lost its exact versioned artifact row")
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
				observations = append(observations, alerts)
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back source graph faults left drift: %s", drift)
		}
	})
	if len(observations) != len(faults) {
		t.Fatalf("observed %d source graph faults, want %d", len(observations), len(faults))
	}
	for i, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), faults[i].want) {
			t.Fatalf("%s lost its published migration owner", faults[i].name)
		}
	}
}
