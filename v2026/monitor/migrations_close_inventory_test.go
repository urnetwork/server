// The optional inventory is version-attributed without changing old receipt rules.
package monitor

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsOriginalCloseInventoryCatalog(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 768)
		check := func(want bool) {
			t.Helper()
			server.Db(ctx, func(conn server.PgConn) {
				var got bool
				server.Raise(conn.QueryRow(ctx, "SELECT "+contractCloseInventoryArtifactQuery).Scan(&got))
				if got != want {
					t.Fatal("original inventory catalog differs", got, want)
				}
			})
		}
		check(false)
		server.ApplyDbMigrationsUpTo(ctx, 769)
		check(true)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE contract_close_report_evidence DROP CONSTRAINT contract_close_inventory_requires_report`))
		})
		check(false)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE contract_close_report_evidence ADD CONSTRAINT contract_close_inventory_requires_report CHECK(original_inventory IS NULL OR original_report IS NOT NULL)`))
		})
		check(true)
	})
}
