// Exercise actual admission SQL, including every reservation transition and
// the durable revision-retention guard, in a disposable database.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// An absent unpublished migration is lag; missing fencing after publication
// is schema drift even if a later migration has not yet been applied.
func TestMigrationsSignalNetEscrowRevisionLifetime(t *testing.T) {
	for _, version := range []int{746, 747} {
		row := syntheticMigrationArtifactRow(version)
		row[158] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			if !strings.Contains(query, netEscrowRevisionArtifactQuery) {
				t.Fatal("migration admission omitted net escrow fencing")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if version == 746 {
			requireAlertClass(t, alerts, "migration-behind")
			if len(alerts) != 1 {
				t.Fatalf("unpublished fence reported as drift: %+v", alerts)
			}
		} else {
			alert := requireAlertClass(t, alerts, "migration-schema-drift")
			if !strings.Contains(alert.Markdown(), "net escrow durable snapshot revision and retention fences@v747") {
				t.Fatalf("missing net escrow fence not identified: %+v", alerts)
			}
		}
	}
}

// A disabled trigger or replaced no-op function must fail admission even when
// the schema version and relation names remain unchanged.
func TestMigrationsSignalNetEscrowRevisionActualSchema(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			assertAdmitted := func(want bool) {
				t.Helper()
				var admitted bool
				server.Raise(conn.QueryRow(ctx, `SELECT `+netEscrowRevisionArtifactQuery).Scan(&admitted))
				if admitted != want {
					t.Fatalf("net escrow fence admitted=%t, want %t", admitted, want)
				}
			}
			assertAdmitted(true)
			for _, guard := range []struct{ table, trigger string }{
				{table: "transfer_escrow", trigger: "transfer_escrow_revision_insert"},
				{table: "transfer_escrow", trigger: "transfer_escrow_revision_update"},
				{table: "transfer_escrow", trigger: "transfer_escrow_revision_delete"},
				{table: "transfer_contract", trigger: "transfer_contract_escrow_revision_insert"},
				{table: "transfer_contract", trigger: "transfer_contract_escrow_revision_update"},
				{table: "transfer_contract", trigger: "transfer_contract_escrow_revision_delete"},
				{table: "transfer_balance", trigger: "transfer_balance_escrow_revision_insert"},
				{table: "transfer_balance", trigger: "transfer_balance_escrow_revision_update"},
				{table: "transfer_balance", trigger: "transfer_balance_escrow_revision_delete"},
				{table: "transfer_balance_net_escrow_revision", trigger: "net_escrow_revision_guard"},
				{table: "transfer_balance_net_escrow_revision", trigger: "net_escrow_revision_truncate_guard"},
				{table: "transfer_escrow", trigger: "transfer_escrow_revision_truncate_guard"},
				{table: "transfer_balance", trigger: "transfer_balance_revision_truncate_guard"},
			} {
				server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE `+guard.table+` DISABLE TRIGGER `+guard.trigger))
				assertAdmitted(false)
				server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE `+guard.table+` ENABLE TRIGGER `+guard.trigger))
				assertAdmitted(true)
			}
			server.RaisePgResult(conn.Exec(ctx, `CREATE OR REPLACE FUNCTION advance_net_escrow_revision(balance_ids uuid[]) RETURNS void LANGUAGE plpgsql AS 'BEGIN RETURN; END'`))
			assertAdmitted(false)
		})
	})
}
