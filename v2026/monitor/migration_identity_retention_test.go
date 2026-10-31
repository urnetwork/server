package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Pure: each contract sits at the version of its migration's content and
// pins the published shapes, not only their names.
func TestIdentityRetentionMigrationContracts(t *testing.T) {
	for _, test := range []struct {
		contract snMainnetMigrationContract
		version  int
		pins     []string
	}{
		{
			contract: retiredChildNetworkClientIndexMigrationContract,
			version:  server.RetiredChildNetworkClientIndexMigrationVersion(),
			pins: []string{
				"CREATE INDEX network_client_retired_child_deactivate_time ON public.network_client USING btree (deactivate_time, client_id) WHERE ((NOT active) AND (source_client_id IS NOT NULL))",
			},
		},
		{
			contract: clientKeyHistoryRetentionMigrationContract,
			version:  server.ClientKeyHistoryRetentionMigrationVersion(),
			pins: []string{
				"st_client_key_history_truncate_guard",
				"st_client_key_gone_gone_time",
				strings.ReplaceAll(server.ClientKeyHistoryGuardBodySql(), "'", "''"),
				strings.ReplaceAll(server.ClientKeyRetireDeletedClientBodySql(), "'", "''"),
			},
		},
	} {
		artifact := test.contract.artifact
		if artifact.requiredVersion != test.version || artifact.rowColumn != test.version-589 {
			t.Fatalf("%s at version %d column %d, want version %d", artifact.name, artifact.requiredVersion, artifact.rowColumn, test.version)
		}
		if migrationCurrentSuffixTestContract(t, test.version).query != test.contract.query {
			t.Fatalf("%s is not the suffix contract of its version", artifact.name)
		}
		for _, pin := range test.pins {
			if !strings.Contains(test.contract.query, pin) {
				t.Fatalf("%s does not pin %q", artifact.name, pin)
			}
		}
	}
}

// Pure: contracts merged out of version order still emit their queries in
// version order, the order of their row columns, and the merged list itself
// is left as it was.
func TestMigrationCurrentSuffixContractsEmitInVersionOrder(t *testing.T) {
	merged := []snMainnetMigrationContract{
		migrationCurrentSuffix(902, "later", "(902)"),
		migrationCurrentSuffix(900, "earlier", "(900)"),
		migrationCurrentSuffix(901, "middle", "(901)"),
	}
	ordered := migrationCurrentSuffixContractsInVersionOrder(merged)
	for index, want := range []int{900, 901, 902} {
		if contract := ordered[index]; contract.artifact.requiredVersion != want || contract.artifact.rowColumn != want-589 || contract.query != fmt.Sprintf("(%d)", want) {
			t.Fatalf("position %d holds %+v, want version %d", index, contract.artifact, want)
		}
	}
	if merged[0].artifact.requiredVersion != 902 {
		t.Fatal("ordering rewrote the merged list")
	}
	// a gap would shift every later row column as surely as a swap
	artifacts := migrationCurrentSuffixArtifacts()
	if len(artifacts) == 0 || artifacts[0].requiredVersion != 794 || artifacts[len(artifacts)-1].requiredVersion != server.MigrationCount() {
		t.Fatalf("suffix artifacts do not run from 794 to the head %d", server.MigrationCount())
	}
	for index := 1; index < len(artifacts); index++ {
		if artifacts[index].requiredVersion != artifacts[index-1].requiredVersion+1 {
			t.Fatalf("suffix artifacts are not contiguous at %d: %d after %d", index, artifacts[index].requiredVersion, artifacts[index-1].requiredVersion)
		}
	}
}

// DB-backed: under the owner rule this runs only after the branch, with its
// migrations, is merged to main.
//
// A rolled-back fault in each published shape reaches the signal as drift of
// its own migration.
func TestIdentityRetentionSchemaFaultsReachSignal(t *testing.T) {
	indexVersion := server.RetiredChildNetworkClientIndexMigrationVersion()
	historyVersion := server.ClientKeyHistoryRetentionMigrationVersion()
	faults := []struct {
		name    string
		version int
		sql     string
	}{
		{"retired child index dropped", indexVersion, `DROP INDEX network_client_retired_child_deactivate_time`},
		{"retired child index covers every client", indexVersion, `DROP INDEX network_client_retired_child_deactivate_time;
			CREATE INDEX network_client_retired_child_deactivate_time ON network_client (deactivate_time, client_id)`},
		{"history truncate guard dropped", historyVersion, `DROP TRIGGER st_client_key_history_truncate_guard ON st_client_key_history`},
		{"history guard refuses retention again", historyVersion, `CREATE OR REPLACE FUNCTION st_client_key_history_immutable_guard() RETURNS trigger
			LANGUAGE plpgsql AS 'BEGIN RAISE EXCEPTION ''signed client-key history is immutable''; END'`},
		{"client delete records no gone time", historyVersion, `CREATE OR REPLACE FUNCTION st_client_key_retire_deleted_client() RETURNS trigger
			LANGUAGE plpgsql AS 'BEGIN UPDATE st_client_key_head SET retired = true WHERE client_id = OLD.client_id; RETURN OLD; END'`},
		{"gone time made optional", historyVersion, `ALTER TABLE st_client_key_gone ALTER COLUMN gone_time DROP NOT NULL`},
		{"gone time lookup dropped", historyVersion, `DROP INDEX st_client_key_gone_gone_time`},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("healthy identity retention reports drift: %s", drift)
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
				artifact := migrationCurrentSuffixTestContract(t, fault.version).artifact
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", artifact.name, fault.version),
				})
			})
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d schema faults, want %d", len(observed), len(faults))
	}
	for index, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}
