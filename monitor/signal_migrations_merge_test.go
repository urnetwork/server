// The two append-only migration series share one monitor row. Real catalog
// queries must retain their column ownership across the relocated boundary.
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Visit the published main head and each hardening append using the exact
// production probe. A shifted SELECT expression fails before fault injection.
func TestMigrationsSignalMergedCatalogUsesExactColumns(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		for version := 741; version <= 749; version++ {
			server.ApplyDbMigrationsUpTo(ctx, version)
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if drift != "" {
				t.Fatalf("coherent merged schema at version %d reported drift: %s", version, drift)
			}
		}
		for _, fault := range []struct{ apply, restore, artifact string }{
			{
				apply:    `ALTER TABLE provider_egress_health ALTER COLUMN security_measured_at SET DEFAULT now()`,
				restore:  `ALTER TABLE provider_egress_health ALTER COLUMN security_measured_at DROP DEFAULT`,
				artifact: "provider_egress_health.security_measured_at@v724",
			},
			{
				apply:    `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`,
				restore:  `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_usage_guard`,
				artifact: "transfer_contract immutable usage and terminal attribution guard@v745",
			},
		} {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, fault.apply))
			})
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if !strings.Contains(drift, fault.artifact) {
				t.Fatalf("merged probe attributed a changed catalog object incorrectly: want %s, got %s", fault.artifact, drift)
			}
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, fault.restore))
			})
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatalf("restored merged schema reported drift: %s", drift)
			}
		}
	})
}
