package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsCloseReportCatalog(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		check := func(want bool) {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var actual bool
				server.Raise(conn.QueryRow(ctx, "SELECT "+contractCloseReportArtifactQuery).Scan(&actual))
				if actual != want {
					t.Fatal("close-report catalog ownership predicate disagrees with native schema")
				}
			})
		}
		server.ApplyDbMigrationsUpTo(ctx, 763)
		check(false) // Future absence is observable without querying a missing table.
		server.ApplyDbMigrationsUpTo(ctx, 764)
		check(true)
		for _, fault := range []struct{ apply, restore string }{
			{`ALTER TABLE contract_close_report ALTER COLUMN checkpoint DROP NOT NULL`, `ALTER TABLE contract_close_report ALTER COLUMN checkpoint SET NOT NULL`},
			{`ALTER TABLE contract_close_report ALTER COLUMN create_time DROP DEFAULT`, `ALTER TABLE contract_close_report ALTER COLUMN create_time SET DEFAULT now()`},
			{`ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_pkey; ALTER TABLE contract_close_report ADD PRIMARY KEY(contract_id,report_id)`, `ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_pkey; ALTER TABLE contract_close_report ADD PRIMARY KEY(contract_id,party,report_id)`},
			{`ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_report_id_check`, `ALTER TABLE contract_close_report ADD CHECK(report_id<>'00000000-0000-0000-0000-000000000000'::uuid)`},
			{`ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_used_transfer_byte_count_check`, `ALTER TABLE contract_close_report ADD CHECK(used_transfer_byte_count>=0)`},
			{`ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_party_check`, `ALTER TABLE contract_close_report ADD CHECK(party IN('source','destination'))`},
			{`ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_contract_id_fkey; ALTER TABLE contract_close_report ADD FOREIGN KEY(contract_id) REFERENCES transfer_contract(contract_id)`, `ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_contract_id_fkey; ALTER TABLE contract_close_report ADD FOREIGN KEY(contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE`},
			{`ALTER TABLE contract_close_report ALTER CONSTRAINT contract_close_report_contract_id_fkey DEFERRABLE`, `ALTER TABLE contract_close_report ALTER CONSTRAINT contract_close_report_contract_id_fkey NOT DEFERRABLE`},
			{`CREATE UNIQUE INDEX test_global_report_id ON contract_close_report(report_id)`, `DROP INDEX test_global_report_id`},
			{`ALTER TABLE contract_close_report ADD COLUMN test_shared_balance_id uuid REFERENCES transfer_balance(balance_id)`, `ALTER TABLE contract_close_report DROP COLUMN test_shared_balance_id`},
		} {
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.apply)) })
			check(false)
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if !strings.Contains(drift, "logical contract close report receipts@v764") {
				t.Fatal("full catalog did not attribute the changed close-report artifact to764")
			}
			server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, fault.restore)) })
			check(true)
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatal("restored catalog still reports schema drift")
			}
		}
		checkMigrationForeignKeyEnabledCatalog(t, ctx, "contract_close_report", "transfer_contract", "logical contract close report receipts@v764")
	})
}
