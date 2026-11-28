package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Pure: the contract sits at the version of the side table's migration and
// pins the published shapes: the trigger's rendered definition, the capture
// body, the prune time index and the storage options.
func TestContractOriginLingerMigrationContract(t *testing.T) {
	version := server.ContractOriginLingerMigrationVersion()
	artifact := contractOriginLingerMigrationContract.artifact
	if artifact.requiredVersion != version || artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
		t.Fatalf("%s at version %d column %d removed %d, want version %d", artifact.name, artifact.requiredVersion, artifact.rowColumn, artifact.removedVersion, version)
	}
	if migrationCurrentSuffixTestContract(t, version).query != contractOriginLingerMigrationContract.query {
		t.Fatalf("%s is not the suffix contract of its version", artifact.name)
	}
	for _, pin := range []string{
		strings.ReplaceAll(contractOriginLingerTriggerDefinition, "'", "''"),
		strings.ReplaceAll(server.ContractOriginLingerCaptureBodySql(), "'", "''"),
		"CREATE INDEX contract_origin_linger_prune_time ON public.contract_origin_linger USING btree (prune_time)",
		"'vacuum_truncate=false'",
		"PRIMARY KEY (source_id, destination_id, contract_id)",
		"CHECK (singleton)",
	} {
		if !strings.Contains(contractOriginLingerMigrationContract.query, pin) {
			t.Fatalf("the side table contract does not pin %q", pin)
		}
	}
	// the operator's trigger may be absent at this version, never different
	if !strings.Contains(contractOriginLingerMigrationContract.query, "("+contractOriginLingerTriggerAbsent+" OR "+contractOriginLingerTriggerPresent+")") {
		t.Fatal("the side table contract does not admit exactly an absent or the published trigger")
	}
}

// DB-backed: under the owner rule this runs only after the branch, with its
// migrations, is merged to main.
//
// The migrated database is healthy, and so is one whose trigger the operator
// has not created yet; a changed trigger, function, index, storage option or
// state table reaches the signal as drift of the side table's migration.
func TestContractOriginLingerSchemaFaultsReachSignal(t *testing.T) {
	version := server.ContractOriginLingerMigrationVersion()
	faults := []struct {
		name string
		sql  string
	}{
		{"trigger without its WHEN", `DROP TRIGGER transfer_contract_origin_linger ON transfer_contract;
			CREATE TRIGGER transfer_contract_origin_linger AFTER INSERT OR UPDATE OF outcome, dispute, close_time, expiration_time
			ON transfer_contract FOR EACH ROW EXECUTE FUNCTION contract_origin_linger_capture()`},
		{"trigger disabled", `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_origin_linger`},
		{"trigger on another function", `CREATE FUNCTION contract_origin_linger_other() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END';
			DROP TRIGGER transfer_contract_origin_linger ON transfer_contract;
			` + strings.Replace(server.ContractOriginLingerTriggerSql, "contract_origin_linger_capture()", "contract_origin_linger_other()", 1)},
		{"capture writes nothing", `CREATE OR REPLACE FUNCTION contract_origin_linger_capture() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NULL; END'`},
		{"prune time index dropped", `DROP INDEX contract_origin_linger_prune_time`},
		{"vacuum may truncate", `ALTER TABLE contract_origin_linger RESET (vacuum_truncate)`},
		{"state singleton unchecked", `ALTER TABLE contract_origin_linger_state DROP CONSTRAINT contract_origin_linger_state_singleton_check`},
		{"prune time optional", `ALTER TABLE contract_origin_linger ALTER COLUMN prune_time DROP NOT NULL`},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("the healthy side table reports drift: %s", drift)
		}
		// before the operator's trigger
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			if _, err := tx.Exec(ctx, `DROP TRIGGER transfer_contract_origin_linger ON transfer_contract`); err != nil {
				t.Fatalf("drop the trigger: %v", err)
			}
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
			if err != nil {
				t.Fatalf("inspect without the trigger: %v", err)
			}
			for _, alert := range alerts {
				if strings.Contains(alert.Markdown(), fmt.Sprintf("@v%d", version)) {
					t.Fatalf("the operator's pending trigger reports drift: %s", alert.Markdown())
				}
			}
		})
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				artifact := migrationCurrentSuffixTestContract(t, version).artifact
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", artifact.name, version),
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
