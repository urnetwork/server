// Late migration contracts execute against the original append-only schema.
// Every fault stays in a disposable transaction and rollback restores custody.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Use the production signal's SQL on the same transaction as a schema fault.
type snMainnetMigrationTxSource struct {
	syntheticSource
	tx server.PgTx
}

// Actual transactional observations cross the joined fixture boundary as data.
// The shared *testing.T assertion runs only on the original test goroutine.
type snMainnetMigrationAlertObservation struct {
	alerts []Alert
	want   string
}

// A schema qualification retains its first failure without a development rerun.
func snMainnetMigrationTestEnv() *server.TestEnv {
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	return environment
}

// Render catalog values as psql does, without replacing any predicate result.
func (self *snMainnetMigrationTxSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	result, err := self.tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	rows := []Row{}
	for result.Next() {
		values, err := result.Values()
		if err != nil {
			return nil, err
		}
		row := make(Row, len(values))
		for i, value := range values {
			switch value := value.(type) {
			case nil:
				row[i] = ""
			case bool:
				row[i] = "f"
				if value {
					row[i] = "t"
				}
			default:
				row[i] = fmt.Sprint(value)
			}
		}
		rows = append(rows, row)
	}
	return rows, result.Err()
}

// The independent local fixture owns both the connection and joined rollback.
func snMainnetMigrationFaultTx(t testing.TB, callback func(context.Context, server.PgTx)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := tx.Rollback(cleanupCtx); err != nil {
				t.Errorf("join schema fixture rollback: %v", err)
			}
		}()
		callback(ctx, tx)
	})
}

// The helper is selected by its published version rather than a guessed index.
func snMainnetMigrationTestContract(t testing.TB, version int) snMainnetMigrationContract {
	t.Helper()
	for _, contract := range snMainnetMigrationContracts {
		if contract.artifact.requiredVersion == version {
			return contract
		}
	}
	t.Fatalf("missing mainnet migration contract at version %d", version)
	return snMainnetMigrationContract{}
}

// Execute a real catalog predicate before and after changing its schema.
func snMainnetMigrationAdmitted(t testing.TB, ctx context.Context, tx server.PgTx, query string, want bool) {
	t.Helper()
	var admitted bool
	if err := tx.QueryRow(ctx, "SELECT "+query).Scan(&admitted); err != nil {
		t.Fatal(err)
	}
	if admitted != want {
		t.Fatalf("actual mainnet schema admitted=%t, want %t", admitted, want)
	}
}

// These paths must be wired into the actual signal, including the original
// positional row protocol, before any standalone predicate can count as done.
func TestMigrationsMainnetOriginalSignalReportsEachMissingContract(t *testing.T) {
	head := server.MigrationCount()
	if head < 779 {
		t.Fatalf("mainnet catalog requires the original JSON repair at head779, got %d", head)
	}
	for _, contract := range snMainnetMigrationContracts {
		row := syntheticMigrationMissingArtifactRow(t, head, contract.artifact.name)
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(head), nil
			}
			if !strings.Contains(query, contract.query) {
				return nil, fmt.Errorf("signal omitted original mainnet contract %d", contract.artifact.requiredVersion)
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%s@v%d", contract.artifact.name, contract.artifact.requiredVersion)
		if alert := requireAlertClass(t, alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("original mainnet drift lost its version: %s", alert.Markdown())
		}
	}
}

// Missing future relations/functions return false without failing inspection.
// Fresh 773/775 installation already includes the approved projection repair;
// old deployed 773/775 bodies are tested separately through the upgrade below.
func TestMigrationsMainnetOriginalCatalogAcrossPublishedPrefixes(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		for version := 769; version <= 779; version++ {
			server.ApplyDbMigrationsUpTo(ctx, version)
			row, drift := migrationPingDatabaseCheck(t, ctx)
			if drift != "" {
				t.Fatalf("coherent mainnet prefix %d reports drift: %s", version, drift)
			}
			for _, contract := range snMainnetMigrationContracts {
				if len(row) <= contract.artifact.rowColumn {
					t.Fatalf("mainnet prefix %d omitted artifact column %d", version, contract.artifact.rowColumn)
				}
				want := contract.artifact.requiredVersion <= version
				if contract.artifact.requiredVersion == 779 && version >= 775 {
					want = true
				}
				if actual := migrationBool(pgRow(row).str(contract.artifact.rowColumn)); actual != want {
					t.Fatalf("mainnet prefix %d contract %d=%t, want %t", version, contract.artifact.requiredVersion, actual, want)
				}
			}
		}
	})
}

// Column width, a missing bound, an unreviewed default, a disabled foreign key
// or a dropped read index must survive through to the actual versioned page.
func TestMigrationsMainnetOriginalSchemaFaultsReachSignal(t *testing.T) {
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range []struct {
			version int
			sql     string
		}{
			{version: 770, sql: `ALTER TABLE verify_original_transition DROP CONSTRAINT verify_original_transition_original_signature_check`},
			{version: 770, sql: `ALTER TABLE st_fleet_binding_original ALTER COLUMN deployment_key TYPE varchar(97)`},
			{version: 771, sql: `ALTER TABLE provider_work_request ALTER COLUMN epoch TYPE numeric(21,0)`},
			{version: 771, sql: `ALTER TABLE provider_work_request DROP CONSTRAINT provider_work_request_expires_at_check`},
			{version: 772, sql: `ALTER TABLE wallet_mapping_consent DROP CONSTRAINT wallet_mapping_consent_nonce_fkey`},
			{version: 773, sql: `DROP INDEX verify_original_request_lookup_identity`},
			{version: 773, sql: `ALTER TABLE verify_original_request_lookup ALTER COLUMN request_signature SET DEFAULT decode(repeat('00',64),'hex')`},
			{version: 774, sql: `ALTER TABLE provider_work_owner DROP CONSTRAINT provider_work_owner_owner_hash_key`},
			{version: 775, sql: `ALTER TABLE verify_original_request_closed DROP CONSTRAINT verify_original_request_closed_receipt_body_check`},
			{version: 776, sql: `ALTER TABLE provider_work_session_head DROP CONSTRAINT provider_work_session_head_sequence_check`},
			{version: 776, sql: `DROP INDEX provider_work_session_event_transaction`},
			{version: 777, sql: `ALTER TABLE st_operator_gas_reservation DROP CONSTRAINT st_operator_gas_reservation_attempt_check`},
			{version: 777, sql: `ALTER TABLE st_operator_gas_budget ALTER COLUMN maximum_lifetime_wei TYPE numeric(79,0)`},
			{version: 778, sql: `ALTER TABLE provider_work_open_original DROP CONSTRAINT provider_work_open_original_observed_at_check`},
			{version: 778, sql: `ALTER TABLE provider_work_open_original SET UNLOGGED`},
			{version: 779, sql: `ALTER FUNCTION verify_original_request_index_body(bytea) CALLED ON NULL INPUT`},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, fault.version)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply mainnet schema fault %q: %v", fault.sql, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf("%s@v%d", contract.artifact.name, fault.version)
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: want})
			})
		}
	})
	if len(observed) != 16 {
		t.Fatalf("mainnet schema fault fixture returned %d alert observations, want 16", len(observed))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("actual changed schema omitted %q: %s", observation.want, alert.Markdown())
		}
	}
}

// A valid FK declaration cannot conceal disabled internal enforcement, and
// retained originals cannot acquire a new cascading mutable-parent dependency.
func TestMigrationsMainnetOriginalForeignKeyCustody(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range []struct {
			version int
			sql     string
		}{
			{version: 771, sql: `ALTER TABLE provider_work_cut DISABLE TRIGGER ALL`},
			{version: 772, sql: `ALTER TABLE wallet_mapping_challenge DISABLE TRIGGER ALL`},
			{version: 773, sql: `ALTER TABLE verify_original_request_lookup DISABLE TRIGGER ALL`},
			{version: 776, sql: `ALTER TABLE provider_work_session_receipt DISABLE TRIGGER ALL`},
			{version: 777, sql: `ALTER TABLE st_operator_gas_reservation DISABLE TRIGGER ALL`},
			{version: 778, sql: `ALTER TABLE provider_work_open_original ADD CONSTRAINT synthetic_mutable_contract_parent FOREIGN KEY(contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE`},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, fault.version)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatal(err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
			})
		}
	})
}

// Trigger events and function authority are operative inputs, not names.
func TestMigrationsMainnetOriginalFunctionAndTriggerCustody(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range []struct {
			version int
			sql     string
		}{
			{version: 770, sql: `CREATE OR REPLACE FUNCTION verify_original_append_only_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
			{version: 771, sql: `CREATE OR REPLACE FUNCTION provider_work_original_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
			{version: 772, sql: `ALTER FUNCTION wallet_mapping_original_guard() SECURITY DEFINER`},
			{version: 773, sql: `ALTER TABLE verify_original_transition DISABLE TRIGGER verify_original_request_capture`},
			{version: 774, sql: `ALTER TABLE provider_work_owner DISABLE TRIGGER provider_work_owner_truncate_guard`},
			{version: 775, sql: `CREATE OR REPLACE FUNCTION verify_original_request_wire_lock(message bytea, signature bytea) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`},
			{version: 775, sql: `CREATE OR REPLACE FUNCTION verify_original_request_no_received_tombstone() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
			{version: 776, sql: `CREATE OR REPLACE FUNCTION provider_work_endpoint_lock(client uuid) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`},
			{version: 776, sql: `ALTER FUNCTION provider_work_session_append(uuid,uuid,text,uuid) SECURITY DEFINER`},
			{version: 776, sql: `ALTER TABLE network_client_connection DISABLE TRIGGER provider_work_session_statement_fence`},
			{version: 776, sql: `DROP TRIGGER provider_work_session_mutation ON network_client_connection; CREATE TRIGGER provider_work_session_mutation AFTER UPDATE ON network_client_connection FOR EACH ROW EXECUTE FUNCTION provider_work_session_mutation()`},
			{version: 776, sql: `CREATE OR REPLACE FUNCTION provider_work_session_head_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`},
			{version: 778, sql: `DROP TRIGGER provider_work_open_guard ON provider_work_open_original; CREATE TRIGGER provider_work_open_guard BEFORE UPDATE ON provider_work_open_original FOR EACH ROW EXECUTE FUNCTION provider_work_original_guard()`},
			{version: 779, sql: `CREATE OR REPLACE FUNCTION verify_original_request_index_body(original bytea) RETURNS jsonb LANGUAGE plpgsql IMMUTABLE STRICT AS 'BEGIN RETURN ''{}''::jsonb; END'`},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, fault.version)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatal(err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
			})
		}
	})
}

// The approved historical functions remain interpretable as their old
// contracts, but head779 must identify either unupgraded consumer as drift.
func TestMigrationsMainnetOriginalJsonUpgradeRejectsLegacyConsumers(t *testing.T) {
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, legacy := range []struct {
			version int
			name    string
			body    string
		}{
			{version: 773, name: "verify_original_request_capture", body: snMainnetVerifyCaptureLegacyBody},
			{version: 775, name: "verify_original_request_closed_fence", body: snMainnetVerifyFenceLegacyBody},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				upgrade := snMainnetMigrationTestContract(t, 779)
				snMainnetMigrationAdmitted(t, ctx, tx, upgrade.query, true)
				if _, err := tx.Exec(ctx, "CREATE OR REPLACE FUNCTION "+legacy.name+"() RETURNS trigger LANGUAGE plpgsql AS "+snMainnetMigrationLiteral(legacy.body)); err != nil {
					t.Fatal(err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, snMainnetMigrationTestContract(t, legacy.version).query, true)
				snMainnetMigrationAdmitted(t, ctx, tx, upgrade.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatal(err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: upgrade.artifact.name + "@v779"})
			})
		}
	})
	if len(observed) != 2 {
		t.Fatalf("mainnet legacy consumer fixture returned %d alert observations, want 2", len(observed))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatal("legacy request consumer hid the required v779 repair", alert.Markdown())
		}
	}
}

// Removing only the tested guard predicate admits the real weakened schema;
// this counterfactual demonstrates that the fault is caught by that guard.
func TestMigrationsMainnetOriginalGuardOmissionControl(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			contract := snMainnetMigrationTestContract(t, 778)
			guard := snMainnetMigrationTrigger("provider_work_open_original", "provider_work_open_guard", "provider_work_original_guard", 27)
			if strings.Count(contract.query, guard) != 1 {
				t.Fatal("guard omission control cannot locate its one operative predicate")
			}
			snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
			if _, err := tx.Exec(ctx, `ALTER TABLE provider_work_open_original DISABLE TRIGGER provider_work_open_guard`); err != nil {
				t.Fatal(err)
			}
			snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
			snMainnetMigrationAdmitted(t, ctx, tx, strings.Replace(contract.query, guard, "TRUE", 1), true)
		})
	})
}
