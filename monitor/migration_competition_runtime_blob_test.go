// The real runtime-blob migration transfers only the round-insert function's
// custody. Historical guards and the replacement remain independently checked.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The copied successor cannot silently omit unrelated mode, fee or source
// fences while teaching the monitor its additional explicit capability.
func TestMigrationsRuntimeBlobBodyChangesOnlyCapability(t *testing.T) {
	previous := "policy->>'required_api_capability' IS DISTINCT FROM 'competition.reward.v2'"
	replacement := "COALESCE(policy->>'required_api_capability','') NOT IN ('competition.reward.v2','competition.reward.v2.runtime-blob.v1')"
	if strings.Count(competitionMigration847CompetitionV2RoundInsertGuardBody, previous) != 1 {
		t.Fatal("historical capability guard is not uniquely identified")
	}
	if expected := strings.Replace(competitionMigration847CompetitionV2RoundInsertGuardBody, previous, replacement, 1); expected != competitionMigration851CompetitionV2RoundInsertGuardBody {
		t.Fatal("runtime-blob monitor body changed more than the exact capability guard")
	}
	contract := migrationCurrentSuffixTestContract(t, 851)
	if contract.artifact.rowColumn != 262 || contract.query != competitionMigration851ArtifactQuery {
		t.Fatal("runtime-blob guard lost its ordered artifact and emitted query")
	}
}

// Both actual schema prefixes must be healthy; only the successor artifact
// becomes present at851, while the retained v847 and v850 artifacts stay true.
func TestMigrationsRuntimeBlobPublishedTransition(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{850, 851} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			row, drift := migrationPingDatabaseCheck(t, t.Context())
			if drift != "" {
				t.Fatalf("published runtime-blob transition at %d reports schema drift: %s", head, drift)
			}
			for _, version := range []int{847, 850, 851} {
				artifact := migrationCurrentSuffixTestContract(t, version).artifact
				if len(row) <= artifact.rowColumn || migrationBool(row[artifact.rowColumn]) != (version <= head) {
					t.Fatalf("prefix %d lost exact artifact presence for %d", head, version)
				}
			}
		}
	})
}

// Even a legitimate future body is drift until its actual migration applies;
// the old function cannot be abandoned based on the compiled binary's head.
func TestMigrationsRuntimeBlobHistoricalGuardCustody(t *testing.T) {
	faults := []struct {
		name string
		body string
	}{
		{name: "guard accepts everything", body: "BEGIN RETURN NEW; END"},
		{name: "successor installed before its migration", body: competitionMigration851CompetitionV2RoundInsertGuardBody},
	}
	var observations [][]Alert
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.ApplyDbMigrationsUpTo(t.Context(), 850)
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published pre-successor runtime schema reports drift: %s", drift)
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				statement := `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $regression$` + fault.body + `$regression$`
				if _, err := tx.Exec(ctx, statement); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observations = append(observations, alerts)
			})
		}
	})
	if len(observations) != len(faults) {
		t.Fatalf("observed %d historical guard faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), "isolated automation staging and current lifecycle guards@v847") {
			t.Fatalf("%s lost historical v847 custody", faults[index].name)
		}
	}
}

// Matching names, numeric migration heads and previous legitimate bodies do
// not excuse an inoperative successor or any retained source/lifecycle fence.
func TestMigrationsRuntimeBlobSchemaFaultsReachSignal(t *testing.T) {
	weakenedCapability := strings.Replace(competitionMigration851CompetitionV2RoundInsertGuardBody,
		"'competition.reward.v2.runtime-blob.v1')", "'competition.reward.v2.runtime-blob.v1','synthetic.future-capability')", 1)
	weakenedFee := strings.Replace(competitionMigration851CompetitionV2RoundInsertGuardBody,
		"(NEW.policy_json->'season_policy'->>'submission_fee_usd')::double precision IS DISTINCT FROM 0::double precision", "false", 1)
	if weakenedCapability == competitionMigration851CompetitionV2RoundInsertGuardBody || weakenedFee == competitionMigration851CompetitionV2RoundInsertGuardBody {
		t.Fatal("runtime-blob fault must change the guarded body")
	}
	faults := []struct {
		name string
		sql  string
		want string
	}{
		{name: "previous runtime capability body restored", sql: `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $regression$` + competitionMigration847CompetitionV2RoundInsertGuardBody + `$regression$`, want: competitionSourceGuardsHeadOwner},
		{name: "unrecognized runtime capability admitted", sql: `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $regression$` + weakenedCapability + `$regression$`, want: competitionSourceGuardsHeadOwner},
		{name: "zero-fee stage condition removed", sql: `CREATE OR REPLACE FUNCTION competition_v2_round_insert_guard() RETURNS trigger LANGUAGE plpgsql AS $regression$` + weakenedFee + `$regression$`, want: competitionSourceGuardsHeadOwner},
		{name: "runtime guard privilege changed", sql: `ALTER FUNCTION competition_v2_round_insert_guard() SECURITY DEFINER`, want: competitionSourceGuardsHeadOwner},
		{name: "runtime guard volatility changed", sql: `ALTER FUNCTION competition_v2_round_insert_guard() STABLE`, want: competitionSourceGuardsHeadOwner},
		{name: "runtime guard trigger disabled", sql: `ALTER TABLE competition_round DISABLE TRIGGER competition_v2_round_source`, want: "competition source authority and atomic outbox custody@v844"},
		{name: "retained source graph guard changed", sql: `CREATE OR REPLACE FUNCTION competition_v2_source_graph_valid(commits jsonb, staging_mode boolean) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS 'BEGIN RETURN true; END'`, want: competitionSourceGuardsHeadOwner},
		{name: "retained seal guard changed", sql: `CREATE OR REPLACE FUNCTION competition_v2_seal_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`, want: "isolated automation staging and current lifecycle guards@v847"},
	}
	var observations [][]Alert
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("published runtime-blob schema reports drift: %s", drift)
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
				observations = append(observations, alerts)
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back runtime-blob faults left schema drift: %s", drift)
		}
	})
	if len(observations) != len(faults) {
		t.Fatalf("observed %d runtime-blob faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), faults[index].want) {
			t.Fatalf("%s lost its published migration owner", faults[index].name)
		}
	}
}
