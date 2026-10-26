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

// Successors through v892 republish the v855 guards; current-head faults belong to
// the last published successor, while historical prefixes retain their owner.
const competitionSourceGuardsHeadOwner = "reviewed repository patch source and winner guard custody@v892"

// Numeric head, matching names, old legitimate bodies and unchanged source
// functions cannot substitute for the exact published successor graph.
func TestMigrationsSourceGraphSchemaFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name string
		sql  string
		want string
	}{
		{name: "legacy source mode restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_mode(intent jsonb) RETURNS void LANGUAGE plpgsql AS $regression$` + competitionMigration847CompetitionV2SourceModeBody + `$regression$`, want: competitionSourceGuardsHeadOwner},
		{name: "legacy source authority restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text) RETURNS void LANGUAGE plpgsql AS $regression$` + competitionMigration847CompetitionV2SourceAuthorizedBody + `$regression$`, want: competitionSourceGuardsHeadOwner},
		{name: "graph validator always accepts", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid(commits jsonb, staging_mode boolean) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS 'BEGIN RETURN true; END'`, want: competitionSourceGuardsHeadOwner},
		{name: "graph security privilege changed", sql: `ALTER FUNCTION competition_v2_source_graph_valid(jsonb,boolean) SECURITY DEFINER`, want: competitionSourceGuardsHeadOwner},
		{name: "graph volatility changed", sql: `ALTER FUNCTION competition_v2_source_graph_valid(jsonb,boolean) VOLATILE`, want: competitionSourceGuardsHeadOwner},
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

// Exact-body custody transfers only when the real successor migration applies.
// Each published prefix must be healthy and still detect every retained guard.
func TestMigrationsSourceGuardPublishedOwnerTransfers(t *testing.T) {
	prefixes := []struct {
		head      int
		removedAt int
		owner     string
	}{
		{855, 858, "elected-winner review and successor source guards@v855"},
		{858, 859, "production and staging source authorization of the ten live repositories@v858"},
		{859, 891, "scheduled round opening bound to its intent@v859"},
		{891, 892, "provider-aware winner review and scheduled round guards@v891"},
		{892, 0, competitionSourceGuardsHeadOwner},
	}
	faults := []struct {
		name string
		sql  string
	}{
		{"source authority accepts everything", `CREATE OR REPLACE FUNCTION competition_v2_source_authorized(intent jsonb, env text, competition text, season text) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`},
		{"round insertion accepts everything", `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
		{"source graph accepts everything", `CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid(commits jsonb, staging_mode boolean) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS 'BEGIN RETURN true; END'`},
	}
	type observation struct {
		head   int
		name   string
		owner  string
		alerts []Alert
	}
	var observations []observation
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, prefix := range prefixes {
			contract := migrationCurrentSuffixTestContract(t, prefix.head)
			if contract.artifact.removedVersion != prefix.removedAt {
				t.Fatalf("published owner %d retires at %d, want %d", prefix.head, contract.artifact.removedVersion, prefix.removedAt)
			}
			server.ApplyDbMigrationsUpTo(t.Context(), prefix.head)
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("published owner prefix %d reports drift: %s", prefix.head, drift)
			}
			for _, fault := range faults {
				snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
					if _, err := tx.Exec(ctx, fault.sql); err != nil {
						t.Fatalf("apply %s at %d: %v", fault.name, prefix.head, err)
					}
					alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
					if err != nil {
						t.Fatalf("inspect %s at %d: %v", fault.name, prefix.head, err)
					}
					observations = append(observations, observation{prefix.head, fault.name, prefix.owner, alerts})
				})
			}
			if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
				t.Fatalf("rolled-back owner prefix %d reports drift: %s", prefix.head, drift)
			}
		}
	})
	if len(observations) != len(prefixes)*len(faults) {
		t.Fatalf("observed %d published-owner faults, want %d", len(observations), len(prefixes)*len(faults))
	}
	for _, observed := range observations {
		alert := requireAlertClass(t, observed.alerts, "migration-schema-drift")
		if !strings.Contains(alert.Markdown(), observed.owner) {
			t.Fatalf("%s at %d lost its published migration owner", observed.name, observed.head)
		}
	}
}
