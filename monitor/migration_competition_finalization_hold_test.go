// Prospective finalization has its own artifact; publishing it must not turn
// the original four-kind hold journal into historical schema drift.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Observe the artifact row returned by the complete production query on the
// existing fault transaction. Its catalog CTEs are never copied or bypassed.
type competitionFinalizationArtifactSource struct {
	*snMainnetMigrationTxSource
	artifactRow Row
}

// Delegate execution and value conversion to the established native source;
// retain only the actual coherence row, not the independent identity rows.
func (self *competitionFinalizationArtifactSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	rows, err := self.snMainnetMigrationTxSource.PostgreSQL(ctx, query)
	if err == nil && strings.Contains(query, "WITH version AS") && strings.Contains(query, "FROM migration_audit") && len(rows) == 1 {
		self.artifactRow = rows[0]
	}
	return rows, err
}

// Both the previous published prefix and the additive head retain all old
// artifacts. The new contract becomes mandatory only at its exact DDL version.
func TestMigrationsCompetitionFinalizationHoldPublishedPrefixes(t *testing.T) {
	version := server.CompetitionFinalizationHoldMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	legacy := migrationCurrentSuffixTestContract(t, 843)
	if version != 927 || contract.artifact.removedVersion != 0 {
		t.Fatalf("finalization migration must append after published origin926, version=%d head=%d", version, server.MigrationCount())
	}
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			row, drift := migrationPingDatabaseCheck(t, t.Context())
			want := "f"
			if head == version {
				want = "t"
			}
			if drift != "" || row[contract.artifact.rowColumn] != want || row[legacy.artifact.rowColumn] != "t" {
				t.Fatalf("prefix%d new=%q legacy=%q drift=%s", head, row[contract.artifact.rowColumn], row[legacy.artifact.rowColumn], drift)
			}
		}
	})
}

// Exact operative guards, immutable journal and physical snapshot fence are
// independently inspected through the complete production catalog signal.
func TestMigrationsCompetitionFinalizationHoldNativeFaultCustody(t *testing.T) {
	version := server.CompetitionFinalizationHoldMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
	legacy := migrationCurrentSuffixTestContract(t, 843).artifact
	faults := []struct{ name, sql string }{
		{"mutable finalization history", `ALTER TABLE competition_finalization_hold DISABLE TRIGGER competition_finalization_hold_immutable`},
		{"truncate bypass", `ALTER TABLE competition_finalization_hold DISABLE TRIGGER competition_finalization_hold_no_truncate`},
		{"missing oldwriter control fence", `ALTER TABLE competition_automation_hold DISABLE TRIGGER competition_00_finalization_hold_scope`},
		{"missing finalization control fence", `ALTER TABLE competition_finalization_hold DISABLE TRIGGER competition_00_finalization_hold_scope`},
		{"missing seal authority", `ALTER TABLE competition_evaluation_seal DISABLE TRIGGER competition_00_finalization_seal`},
		{"missing first election authority", `ALTER TABLE competition_reward_decision DISABLE TRIGGER competition_00_finalization_decision`},
		{"missing terminal authority", `ALTER TABLE competition_round DISABLE TRIGGER competition_00_finalization_round`},
		{"advisory-only stale snapshot fence", `CREATE OR REPLACE FUNCTION competition_finalization_touch_scope(env text,competition text,season text) RETURNS void LANGUAGE plpgsql AS 'BEGIN PERFORM competition_v2_scope_lock(env,competition,season); END'`},
		{"mutable scope authority", `CREATE OR REPLACE FUNCTION competition_finalization_require_round_unheld(target_round uuid) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`},
		{"first election body bypass", `CREATE OR REPLACE FUNCTION competition_finalization_decision_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
		{"lost positive generation", snMainnetMigrationDropConstraint("competition_finalization_scope_fence", "c", "CHECK ((generation > 0))")},
		{"wrong generation width", `ALTER TABLE competition_finalization_scope_fence ALTER COLUMN generation TYPE integer`},
		{"weakened capability check", snMainnetMigrationDropConstraint("competition_finalization_hold", "c", "CHECK (((kind)::text = 'finalization'::text))")},
		{"lost global actor key", snMainnetMigrationDropConstraint("competition_finalization_hold", "u", "UNIQUE (environment, competition_id, season_id, actor_id, idempotency_key)")},
	}
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatal("published finalization schema drift", drift)
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				source := &competitionFinalizationArtifactSource{snMainnetMigrationTxSource: &snMainnetMigrationTxSource{tx: tx}}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				alert := requireAlertClass(t, alerts, "migration-schema-drift")
				if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, owner) {
					t.Fatalf("%s lost owning finalization artifact: %s", fault.name, alert.Observed)
				}
				// Inspect the original four-kind journal's own artifact column
				// from that same full query. A standalone predicate would omit
				// its required index/function/trigger catalog CTEs.
				if len(source.artifactRow) <= legacy.rowColumn || source.artifactRow[legacy.rowColumn] != "t" {
					t.Fatalf("%s unexpectedly changed original hold authority", fault.name)
				}
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatal("rolled-back finalization faults left schema drift", drift)
		}
	})
}
