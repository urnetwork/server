// Late migration contracts execute against the original append-only schema.
// Physical faults stay in disposable transactions. RI trigger-state faults use
// the existing catalog projection because the fixture has no superuser grant.
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

// PostgreSQL may derive a table-level name for a cross-column CHECK or truncate
// a generated name. Resolve exactly one original definition before changing it.
func snMainnetMigrationDropConstraint(table, kind, definition string) string {
	return fmt.Sprintf(`DO $sn_mainnet_fault$
DECLARE matched_constraint name;
BEGIN
 SELECT conname INTO STRICT matched_constraint FROM pg_catalog.pg_constraint
 WHERE conrelid=to_regclass(%s) AND contype=%s AND pg_get_constraintdef(oid)=%s;
 EXECUTE format('ALTER TABLE public.%%I DROP CONSTRAINT %%I', %s, matched_constraint);
END;
$sn_mainnet_fault$;`, snMainnetMigrationLiteral("public."+table), snMainnetMigrationLiteral(kind), snMainnetMigrationLiteral(definition), snMainnetMigrationLiteral(table))
}

// Only the observed state of one real RI trigger changes. These are projected
// catalog-read faults, not privileged ALTER TRIGGER operations or host drills.
// Exact FK definitions distinguish multiple references to the same parent.
func snMainnetMigrationForeignKeyCatalogFaults(t testing.TB, ctx context.Context, version int, child, parent, definition string) []snMainnetMigrationAlertObservation {
	t.Helper()
	contract := snMainnetMigrationTestContract(t, version)
	var triggerIds []uint32
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT t.oid FROM pg_catalog.pg_trigger t
 JOIN pg_catalog.pg_constraint k ON k.oid=t.tgconstraint
 WHERE k.conrelid=to_regclass('public.'||$1) AND k.confrelid=to_regclass('public.'||$2)
 AND k.contype='f' AND pg_get_constraintdef(k.oid)=$3 AND t.tgisinternal ORDER BY t.oid`, child, parent, definition)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var triggerId uint32
			if err := rows.Scan(&triggerId); err != nil {
				t.Fatal(err)
			}
			triggerIds = append(triggerIds, triggerId)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	if len(triggerIds) != 4 {
		t.Fatalf("exact FK %s / %s has %d RI triggers, want four", child, definition, len(triggerIds))
	}
	var observed []snMainnetMigrationAlertObservation
	for _, triggerId := range triggerIds {
		for _, enabled := range []string{"D", "R", "A"} {
			source := &migrationFKTriggerProjectionSource{oid: triggerId, enabled: enabled}
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			if enabled == "A" {
				for _, alert := range alerts {
					if alert.Class == "migration-schema-drift" {
						t.Fatalf("always-enabled RI trigger %d for %s reports drift: %s", triggerId, child, alert.Markdown())
					}
				}
			} else {
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: fmt.Sprintf("%s@v%d", contract.artifact.name, version)})
			}
		}
	}
	if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
		t.Fatalf("unchanged physical FK catalog after projection reports drift: %s", drift)
	}
	return observed
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
		for version := 769; version <= server.MigrationCount(); version++ {
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
				// The corrected fresh v776 already has the two repaired bodies;
				// v782 independently requires them on upgraded installations.
				if contract.artifact.requiredVersion == 782 && version >= 776 {
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
			{version: 770, sql: snMainnetMigrationDropConstraint("verify_original_transition", "c", "CHECK ((octet_length(original_signature) = 64))")},
			{version: 770, sql: `ALTER TABLE st_fleet_binding_original ALTER COLUMN deployment_key TYPE varchar(97)`},
			{version: 771, sql: `ALTER TABLE provider_work_request ALTER COLUMN epoch TYPE numeric(21,0)`},
			{version: 771, sql: snMainnetMigrationDropConstraint("provider_work_request", "c", "CHECK (((expires_at > issued_at) AND ((expires_at - issued_at) <= 3600)))")},
			{version: 772, sql: snMainnetMigrationDropConstraint("wallet_mapping_consent", "f", "FOREIGN KEY (nonce) REFERENCES wallet_mapping_challenge(nonce)")},
			{version: 773, sql: `DROP INDEX verify_original_request_lookup_identity`},
			{version: 773, sql: `ALTER TABLE verify_original_request_lookup ALTER COLUMN request_signature SET DEFAULT decode(repeat('00',64),'hex')`},
			{version: 774, sql: snMainnetMigrationDropConstraint("provider_work_owner", "u", "UNIQUE (owner_hash)")},
			{version: 775, sql: snMainnetMigrationDropConstraint("verify_original_request_closed", "c", "CHECK (((octet_length(receipt_body) >= 1) AND (octet_length(receipt_body) <= 4096)))")},
			{version: 776, sql: snMainnetMigrationDropConstraint("provider_work_session_head", "c", "CHECK ((sequence > 0))")},
			{version: 776, sql: `DROP INDEX provider_work_session_event_transaction`},
			{version: 777, sql: snMainnetMigrationDropConstraint("st_operator_gas_reservation", "c", "CHECK ((attempt > 0))")},
			{version: 777, sql: `ALTER TABLE st_operator_gas_budget ALTER COLUMN maximum_lifetime_wei TYPE numeric(79,0)`},
			{version: 778, sql: snMainnetMigrationDropConstraint("provider_work_open_original", "c", "CHECK ((observed_at >= boundary_time))")},
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

// Project disabled/replica-only states of each real FK's four internal guards;
// an always-enabled projection and the unchanged catalog remain healthy. The
// added cascading dependency is a physical transaction owned by this fixture.
func TestMigrationsMainnetOriginalForeignKeyCustody(t *testing.T) {
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		for _, foreignKey := range []struct {
			version                   int
			child, parent, definition string
		}{
			{version: 771, child: "provider_work_cut", parent: "provider_work_request", definition: "FOREIGN KEY (request_hash) REFERENCES provider_work_request(request_hash)"},
			{version: 772, child: "wallet_mapping_consent", parent: "wallet_mapping_challenge", definition: "FOREIGN KEY (nonce) REFERENCES wallet_mapping_challenge(nonce)"},
			{version: 773, child: "verify_original_request_lookup", parent: "verify_original_transition", definition: "FOREIGN KEY (trail_id, previous_depth) REFERENCES verify_original_transition(trail_id, previous_depth)"},
			{version: 776, child: "provider_work_session_receipt", parent: "provider_work_session_event", definition: "FOREIGN KEY (client_id, sequence) REFERENCES provider_work_session_event(client_id, sequence)"},
			{version: 777, child: "st_operator_gas_reservation", parent: "st_transaction_intent", definition: "FOREIGN KEY (intent_id) REFERENCES st_transaction_intent(intent_id)"},
			{version: 777, child: "st_operator_gas_reservation", parent: "st_operator_gas_budget", definition: "FOREIGN KEY (scope_key) REFERENCES st_operator_gas_budget(scope_key)"},
			{version: 777, child: "st_operator_gas_reservation", parent: "st_operator_gas_intent", definition: "FOREIGN KEY (logical_key) REFERENCES st_operator_gas_intent(logical_key)"},
			{version: 777, child: "st_operator_gas_reservation", parent: "st_operator_gas_policy", definition: "FOREIGN KEY (policy_sha256) REFERENCES st_operator_gas_policy(policy_sha256)"},
		} {
			observed = append(observed, snMainnetMigrationForeignKeyCatalogFaults(t, ctx, foreignKey.version, foreignKey.child, foreignKey.parent, foreignKey.definition)...)
		}
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			contract := snMainnetMigrationTestContract(t, 778)
			snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
			if _, err := tx.Exec(ctx, `ALTER TABLE provider_work_open_original ADD CONSTRAINT synthetic_mutable_contract_parent FOREIGN KEY(contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE`); err != nil {
				t.Fatal(err)
			}
			snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
			if err != nil {
				t.Fatal(err)
			}
			observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: contract.artifact.name + "@v778"})
		})
	})
	if len(observed) != 65 {
		t.Fatalf("mainnet FK fault fixture returned %d observations, want 64 projected faults and one physical fault", len(observed))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("FK catalog fault omitted %q: %s", observation.want, alert.Markdown())
		}
	}
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
			{version: 776, sql: `CREATE OR REPLACE FUNCTION provider_work_endpoint_lock(client uuid) RETURNS void LANGUAGE sql AS $$ SELECT pg_advisory_xact_lock(776,('x'||substr(md5(client::text),1,8))::bit(32)::int); $$`},
			{version: 776, sql: `CREATE OR REPLACE FUNCTION provider_work_session_statement_fence() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(-776::bigint); RETURN NULL; END; $$`},
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
