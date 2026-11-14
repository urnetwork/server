// Policy namespace admission must supersede old identity keys without losing
// the signed-history and client-retirement guards that remain operative.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsSignalClientKeyNamespaceLifetime(t *testing.T) {
	for _, version := range []int{743, 744, server.MigrationCount()} {
		row := syntheticMigrationArtifactRow(version)
		row[62] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			return []Row{row}, nil
		}}
		assertDrift := func(want string) {
			t.Helper()
			alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			var drift string
			for _, alert := range alerts {
				if alert.Class == "migration-schema-drift" {
					drift = alert.Markdown()
				}
			}
			if want == "" && drift != "" || want != "" && !strings.Contains(drift, want) {
				t.Errorf("head %d drift=%q, want %q", version, drift, want)
			}
		}
		if version < 744 {
			assertDrift("signed client-key history tables and guards@v651")
		} else {
			assertDrift("")
			row[155] = "f"
			assertDrift("signed client-key policy namespaces and active head@v744")
		}
	}
}

func TestMigrationsSignalClientKeyNamespaceExecutesActualSchema(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, version := range []int{743, 744, server.MigrationCount()} {
			server.ApplyDbMigrationsUpTo(ctx, version)
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if drift != "" {
				t.Errorf("coherent client-key schema at head %d reported drift: %s", version, drift)
			}
		}
		for _, guard := range []struct{ table, trigger string }{
			{"st_client_key_history", "st_client_key_history_immutable"},
			{"st_client_key_head", "st_client_key_head_identity"},
			{"network_client", "st_client_key_retire_on_client_delete"},
		} {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, "ALTER TABLE "+guard.table+" DISABLE TRIGGER "+guard.trigger))
			})
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if !strings.Contains(drift, "signed client-key policy namespaces and active head@v744") {
				t.Errorf("disabled %s was not owned by successor admission: %s", guard.trigger, drift)
			}
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, "ALTER TABLE "+guard.table+" ENABLE TRIGGER "+guard.trigger))
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Errorf("restored client-key schema reported drift: %s", drift)
		}
	})
}
