// The final catalog contracts must inspect their actual published DDL, including
// a partial historical provider-function repair, a nullable grant marker, the
// account message outbox and the search update commit order.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// These are the two historical functions replaced by the actual v782 DDL.
const snMainnetLegacyProviderFunctionsForTest = `
CREATE OR REPLACE FUNCTION provider_work_endpoint_lock(client uuid) RETURNS void LANGUAGE sql AS $$
 SELECT pg_advisory_xact_lock(776,('x'||substr(md5(client::text),1,8))::bit(32)::int);
$$;
CREATE OR REPLACE FUNCTION provider_work_session_statement_fence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM pg_advisory_xact_lock(-776::bigint);
 RETURN NULL;
END;
$$;
`

func TestMigrationsProviderSessionRepairCatalogUpgrade(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 781)
		contract := snMainnetMigrationTestContract(t, 782)
		// Corrected fresh v776 already has both bodies, independently of head.
		server.Db(ctx, func(conn server.PgConn) {
			var present bool
			server.Raise(conn.QueryRow(ctx, "SELECT "+contract.query).Scan(&present))
			if !present {
				t.Fatal("fresh v776 functions are not recognized before v782")
			}
			server.RaisePgResult(conn.Exec(ctx, snMainnetLegacyProviderFunctionsForTest))
			server.Raise(conn.QueryRow(ctx, "SELECT "+contract.query).Scan(&present))
			if present {
				t.Fatal("historical global/waiting functions passed the repaired contract")
			}
		})
		// Exercise the owning appended migration, not a test copy of its body.
		server.ApplyDbMigrationsUpTo(ctx, 782)
		row, drift := migrationPingDatabaseCheck(t, ctx)
		if drift != "" || !migrationBool(pgRow(row).str(contract.artifact.rowColumn)) {
			t.Fatalf("actual v782 repair did not restore its signal contract: %s", drift)
		}
	})
}

func TestMigrationsCurrentSuffixCatalogFaultsReachSignal(t *testing.T) {
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range []struct {
			name    string
			version int
			sql     string
		}{
			{"missing grant marker", 781, `ALTER TABLE transfer_balance DROP COLUMN grant_kind`},
			{"wrong grant marker width", 781, `ALTER TABLE transfer_balance ALTER COLUMN grant_kind TYPE varchar(31)`},
			{"required grant marker", 781, `ALTER TABLE transfer_balance ALTER COLUMN grant_kind SET NOT NULL`},
			{"invented grant marker default", 781, `ALTER TABLE transfer_balance ALTER COLUMN grant_kind SET DEFAULT 'legacy'`},
			{"missing endpoint signature", 782, `ALTER FUNCTION provider_work_endpoint_lock(uuid) RENAME TO synthetic_missing_endpoint_lock`},
			{"only endpoint function regressed", 782, `CREATE OR REPLACE FUNCTION provider_work_endpoint_lock(client uuid) RETURNS void LANGUAGE sql AS $$ SELECT pg_advisory_xact_lock(776,('x'||substr(md5(client::text),1,8))::bit(32)::int); $$`},
			{"only statement function regressed", 782, `CREATE OR REPLACE FUNCTION provider_work_session_statement_fence() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(-776::bigint); RETURN NULL; END; $$`},
			{"changed endpoint privileges", 782, `ALTER FUNCTION provider_work_endpoint_lock(uuid) SECURITY DEFINER`},
			{"changed statement function configuration", 782, `ALTER FUNCTION provider_work_session_statement_fence() SET search_path TO public`},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, fault.version)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: fmt.Sprintf("%s@v%d", contract.artifact.name, fault.version)})
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back suffix faults left catalog drift: %s", drift)
		}
	})
	if len(observed) != 9 {
		t.Fatalf("observed %d suffix faults, want nine", len(observed))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("current suffix drift lost its owning version: %s", alert.Markdown())
		}
	}
}

// The outbox and search commit-order contracts reject each change that would
// break delivery or reading in commit order: a dropped or redefined outbox
// part, a partial index over other rows, a missing, nullable or constant
// transaction id, and a missing or reordered commit-order index.
func TestMigrationsOutboxAndSearchOrderCatalogFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name    string
		version int
		sql     string
	}{
		{"missing outbox", 783, `DROP TABLE account_message_outbox`},
		{"nullable recipient", 783, `ALTER TABLE account_message_outbox ALTER COLUMN user_auth DROP NOT NULL`},
		{"wider message key", 783, `ALTER TABLE account_message_outbox ALTER COLUMN message_key TYPE char(65)`},
		{"missing attempt bound", 783, snMainnetMigrationDropConstraint("account_message_outbox", "c", "CHECK ((attempt_count >= 0))")},
		{"missing message key uniqueness", 783, snMainnetMigrationDropConstraint("account_message_outbox", "u", "UNIQUE (message_key)")},
		{"due index over every row", 783, `DROP INDEX account_message_outbox_due; CREATE INDEX account_message_outbox_due ON account_message_outbox (deliver_time, message_id)`},
		{"missing held index", 783, `DROP INDEX account_message_outbox_held`},
		{"finished index over sent rows only", 783, `DROP INDEX account_message_outbox_finished; CREATE INDEX account_message_outbox_finished ON account_message_outbox (create_time, message_id) WHERE sent_time IS NOT NULL`},
		{"missing transaction id", 784, `ALTER TABLE search_value_update DROP COLUMN xid`},
		{"nullable transaction id", 784, `ALTER TABLE search_value_update ALTER COLUMN xid DROP NOT NULL`},
		{"constant transaction id default", 784, `ALTER TABLE search_value_update ALTER COLUMN xid SET DEFAULT '0'`},
		{"missing commit-order index", 785, `DROP INDEX search_value_update_realm_xid_update_id`},
		{"reordered commit-order index", 785, `DROP INDEX search_value_update_realm_xid_update_id; CREATE INDEX search_value_update_realm_xid_update_id ON search_value_update (realm, update_id, xid)`},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				contract := snMainnetMigrationTestContract(t, fault.version)
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, true)
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				snMainnetMigrationAdmitted(t, ctx, tx, contract.query, false)
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observed = append(observed, snMainnetMigrationAlertObservation{alerts: alerts, want: fmt.Sprintf("%s@v%d", contract.artifact.name, fault.version)})
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back outbox and search order faults left catalog drift: %s", drift)
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d outbox and search order faults, want %d", len(observed), len(faults))
	}
	for _, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("outbox or search order drift lost its owning version: %s", alert.Markdown())
		}
	}
}
