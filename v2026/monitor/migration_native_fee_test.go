// Native-fee catalog controls run against the actual appended migration780.
// Physical faults are transaction-local; RI trigger states are projected from
// the actual catalog without a superuser grant. Alerts cross a joined fixture.
package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Resolve the exact definition, including this truncated generated key name.
func snNativeFeeDropTransactionUnique() string {
	return snMainnetMigrationDropConstraint("st_operator_native_fee_settlement", "u", "UNIQUE (scope_key, transaction_hash)")
}

const snNativeFeeAddUnapprovedTrigger = `CREATE FUNCTION synthetic_native_fee_trigger() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';
CREATE TRIGGER synthetic_native_fee_write BEFORE INSERT ON st_operator_native_fee_owner FOR EACH ROW EXECUTE FUNCTION synthetic_native_fee_trigger();`

func TestMigrationsNativeFeeSettlementCatalogAcrossPublication(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		contract := snMainnetMigrationTestContract(t, 780)
		for _, version := range []int{779, 780} {
			server.ApplyDbMigrationsUpTo(ctx, version)
			row, drift := migrationPingDatabaseCheck(t, ctx)
			if drift != "" {
				t.Fatalf("native-fee prefix %d reports schema drift: %s", version, drift)
			}
			if contract.artifact.rowColumn != 191 || len(row) <= contract.artifact.rowColumn {
				t.Fatalf("native-fee artifact lost its exact appended column: %d/%d", contract.artifact.rowColumn, len(row))
			}
			if actual := migrationBool(pgRow(row).str(contract.artifact.rowColumn)); actual != (version == 780) {
				t.Fatalf("native-fee artifact admitted=%t at actual prefix%d", actual, version)
			}
		}
	})
}

func TestMigrationsNativeFeeSettlementSchemaFaultsReachSignal(t *testing.T) {
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range []struct {
			name string
			sql  string
		}{
			{name: "native-rao-width", sql: `ALTER TABLE st_operator_native_fee_settlement ALTER COLUMN debit_rao TYPE numeric(21,0)`},
			{name: "credited-debit-required", sql: `ALTER TABLE st_operator_native_fee_settlement ALTER COLUMN debit_wei DROP NOT NULL`},
			{name: "unmapped-hold-stays-nullable", sql: `ALTER TABLE st_operator_native_fee_hold ALTER COLUMN maximum_debit_wei SET NOT NULL`},
			{name: "unmapped-hold-has-no-invented-zero", sql: `ALTER TABLE st_operator_native_fee_hold ALTER COLUMN maximum_debit_wei SET DEFAULT 0`},
			{name: "original-approver-width", sql: `ALTER TABLE st_operator_native_fee_owner ALTER COLUMN approver_public_key TYPE varchar(65)`},
			{name: "original-authority-required", sql: `ALTER TABLE st_operator_native_fee_policy ALTER COLUMN authority_json DROP NOT NULL`},
			{name: "original-ceiling-positive", sql: snMainnetMigrationDropConstraint("st_operator_native_fee_settlement", "c", "CHECK ((original_ceiling_wei > (0)::numeric))")},
			{name: "native-debit-nonnegative", sql: snMainnetMigrationDropConstraint("st_operator_native_fee_settlement", "c", "CHECK ((debit_rao >= (0)::numeric))")},
			{name: "one-scope-transaction", sql: snNativeFeeDropTransactionUnique()},
			{name: "settlement-scope-index", sql: `DROP INDEX st_operator_native_fee_settlement_scope`},
			{name: "hold-scope-index", sql: `DROP INDEX st_operator_native_fee_hold_scope`},
			{name: "original-reference-durable", sql: `ALTER TABLE st_operator_native_fee_original_reference SET UNLOGGED`},
			{name: "original-object-size", sql: snMainnetMigrationDropConstraint("st_operator_native_fee_original_object", "c", "CHECK ((byte_count > 0))")},
			{name: "original-chunk-bound", sql: snMainnetMigrationDropConstraint("st_operator_native_fee_original_chunk", "c", "CHECK (((octet_length(original_bytes) > 0) AND (octet_length(original_bytes) <= 1048576)))")},
			{name: "original-chunk-position", sql: snMainnetMigrationDropConstraint("st_operator_native_fee_original_chunk", "c", "CHECK ((chunk_index >= 0))")},
			{name: "original-path-text", sql: `ALTER TABLE st_operator_native_fee_original_reference ALTER COLUMN original_path TYPE varchar(2048)`},
			{name: "original-statement-has-no-default", sql: `ALTER TABLE st_operator_native_fee_settlement ALTER COLUMN statement_json SET DEFAULT ''::bytea`},
			{name: "no-unapproved-trigger-authority", sql: snNativeFeeAddUnapprovedTrigger},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, 780)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply native-fee fault %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatal(err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: contract.artifact.name + "@v780"})
			})
		}
	})
	if len(observed) != 18 {
		t.Fatalf("native-fee fault fixture returned %d observations, want18", len(observed))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatal("actual native-fee schema drift lost its versioned page", alert.Markdown())
		}
	}
}

// Project each guard of all fourteen exact FKs, including the two independent
// hold-to-policy references. The four owner-permitted DDL changes stay physical.
func TestMigrationsNativeFeeSettlementForeignKeyCustody(t *testing.T) {
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		for _, foreignKey := range []struct {
			child, parent, definition string
		}{
			{child: "st_operator_native_fee_owner", parent: "st_operator_gas_budget", definition: "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"},
			{child: "st_operator_native_fee_policy", parent: "st_operator_gas_budget", definition: "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"},
			{child: "st_operator_native_fee_settlement", parent: "st_transaction_intent", definition: "FOREIGN KEY (intent_id) REFERENCES st_transaction_intent(intent_id)"},
			{child: "st_operator_native_fee_settlement", parent: "st_operator_gas_budget", definition: "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"},
			{child: "st_operator_native_fee_settlement", parent: "st_operator_gas_intent", definition: "FOREIGN KEY (logical_key) REFERENCES st_operator_gas_intent(logical_key)"},
			{child: "st_operator_native_fee_settlement", parent: "st_operator_native_fee_policy", definition: "FOREIGN KEY (policy_sha256) REFERENCES st_operator_native_fee_policy(policy_sha256)"},
			{child: "st_operator_native_fee_settlement", parent: "st_operator_gas_reservation", definition: "FOREIGN KEY (intent_id, attempt) REFERENCES st_operator_gas_reservation(intent_id, attempt)"},
			{child: "st_operator_native_fee_hold", parent: "st_transaction_intent", definition: "FOREIGN KEY (intent_id) REFERENCES st_transaction_intent(intent_id)"},
			{child: "st_operator_native_fee_hold", parent: "st_operator_gas_budget", definition: "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"},
			{child: "st_operator_native_fee_hold", parent: "st_operator_gas_intent", definition: "FOREIGN KEY (logical_key) REFERENCES st_operator_gas_intent(logical_key)"},
			{child: "st_operator_native_fee_hold", parent: "st_operator_native_fee_policy", definition: "FOREIGN KEY (first_policy_sha256) REFERENCES st_operator_native_fee_policy(policy_sha256)"},
			{child: "st_operator_native_fee_hold", parent: "st_operator_native_fee_policy", definition: "FOREIGN KEY (maximum_policy_sha256) REFERENCES st_operator_native_fee_policy(policy_sha256)"},
			{child: "st_operator_native_fee_original_chunk", parent: "st_operator_native_fee_original_object", definition: "FOREIGN KEY (original_sha256) REFERENCES st_operator_native_fee_original_object(original_sha256)"},
			{child: "st_operator_native_fee_original_reference", parent: "st_operator_native_fee_original_object", definition: "FOREIGN KEY (original_sha256) REFERENCES st_operator_native_fee_original_object(original_sha256)"},
		} {
			observed = append(observed, snMainnetMigrationForeignKeyCatalogFaults(t, ctx, 780, foreignKey.child, foreignKey.parent, foreignKey.definition)...)
		}
		for _, fault := range []struct {
			name string
			sql  string
		}{
			{name: "original-attempt-immediate", sql: snMainnetMigrationDropConstraint("st_operator_native_fee_settlement", "f", "FOREIGN KEY (intent_id, attempt) REFERENCES st_operator_gas_reservation(intent_id, attempt)") + `
ALTER TABLE st_operator_native_fee_settlement ADD CONSTRAINT synthetic_native_fee_attempt FOREIGN KEY(intent_id,attempt) REFERENCES st_operator_gas_reservation(intent_id,attempt) DEFERRABLE INITIALLY IMMEDIATE`},
			{name: "original-chunk-validated", sql: snMainnetMigrationDropConstraint("st_operator_native_fee_original_chunk", "f", "FOREIGN KEY (original_sha256) REFERENCES st_operator_native_fee_original_object(original_sha256)") + `
ALTER TABLE st_operator_native_fee_original_chunk ADD CONSTRAINT synthetic_native_fee_chunk FOREIGN KEY(original_sha256) REFERENCES st_operator_native_fee_original_object(original_sha256) NOT VALID`},
			{name: "original-object-no-mutable-parent", sql: `ALTER TABLE st_operator_native_fee_original_object ADD COLUMN synthetic_intent_id uuid;
ALTER TABLE st_operator_native_fee_original_object ADD CONSTRAINT synthetic_mutable_native_fee_parent FOREIGN KEY(synthetic_intent_id) REFERENCES st_transaction_intent(intent_id) ON DELETE CASCADE`},
			{name: "hold-original-policy-no-cascade", sql: snMainnetMigrationDropConstraint("st_operator_native_fee_hold", "f", "FOREIGN KEY (first_policy_sha256) REFERENCES st_operator_native_fee_policy(policy_sha256)") + `
ALTER TABLE st_operator_native_fee_hold ADD CONSTRAINT synthetic_native_fee_hold_policy FOREIGN KEY(first_policy_sha256) REFERENCES st_operator_native_fee_policy(policy_sha256) ON DELETE CASCADE`},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, 780)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply native-fee foreign-key fault %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatal(err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: contract.artifact.name + "@v780"})
			})
		}
	})
	if len(observed) != 116 {
		t.Fatalf("native-fee FK fixture returned %d observations, want 112 projected faults and four physical faults", len(observed))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatal("native-fee FK catalog fault lost its versioned page", alert.Markdown())
		}
	}
}

// Removing only the operative unique or trigger predicate admits its actual
// weakened schema. No fabricated query result supplies either verdict.
func TestMigrationsNativeFeeSettlementGuardOmissionControl(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, control := range []struct {
			name, sql, guard, replacement string
		}{
			{name: "one-scope-transaction", sql: snNativeFeeDropTransactionUnique(), guard: snMainnetMigrationLiteral("UNIQUE (scope_key, transaction_hash)"), replacement: snMainnetMigrationLiteral("PRIMARY KEY (intent_id)")},
			{name: "only-original-trigger-authority", sql: snNativeFeeAddUnapprovedTrigger, guard: snMainnetNativeFeeTriggerCensus("st_operator_native_fee_owner"), replacement: "TRUE"},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, 780)
				if strings.Count(contract.query, control.guard) != 1 {
					t.Fatalf("native-fee omission control %s lacks one operative predicate", control.name)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, control.sql); err != nil {
					t.Fatal(err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				snMainnetMigrationAdmitted(t, ctx, tx, strings.Replace(contract.query, control.guard, control.replacement, 1), true)
			})
		}
	})
}
