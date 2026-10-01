// Archive admission executes the actual catalog expression against a private
// database and catches drift before billing cleanup can discard live proof.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A missing future archive is only migration lag; a missing published archive
// is schema drift even when another later migration also remains unapplied.
func TestMigrationsSignalUsageArchiveLifetime(t *testing.T) {
	for _, version := range []int{745, 746} {
		row := syntheticMigrationArtifactRow(version)
		row[157] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			if !strings.Contains(query, providerUsageArchiveArtifactQuery) {
				t.Fatal("migration admission omitted archive custody")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if version == 745 {
			requireAlertClass(t, alerts, "migration-behind")
			if len(alerts) != 1 {
				t.Fatalf("predecessor reported unpublished archive drift: %+v", alerts)
			}
		} else {
			alert := requireAlertClass(t, alerts, "migration-schema-drift")
			if !strings.Contains(alert.Markdown(), "st_provider_usage_archive exact atomic copy and append-only custody@v746") {
				t.Fatalf("missing archive custody not identified: %+v", alerts)
			}
		}
	}
}

// Restore each trigger after a forced absence so every independent deletion
// path must be checked; a name-compatible no-op function is also refused.
func TestMigrationsSignalUsageArchiveExecutesActualSchema(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			assertAdmitted := func(want bool) {
				t.Helper()
				var admitted bool
				server.Raise(conn.QueryRow(ctx, `SELECT `+providerUsageArchiveArtifactQuery).Scan(&admitted))
				if admitted != want {
					t.Fatalf("actual archive custody admitted=%t, want %t", admitted, want)
				}
			}
			assertAdmitted(true)
			for _, guard := range []struct{ table, trigger string }{
				{table: "transfer_contract", trigger: "transfer_contract_usage_archive_capture"},
				{table: "transfer_contract", trigger: "transfer_contract_usage_archive_truncate_guard"},
				{table: "st_provider_usage_archive", trigger: "st_provider_usage_archive_guard"},
				{table: "st_provider_usage_archive", trigger: "st_provider_usage_archive_truncate_guard"},
			} {
				server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE `+guard.table+` DISABLE TRIGGER `+guard.trigger))
				assertAdmitted(false)
				server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE `+guard.table+` ENABLE TRIGGER `+guard.trigger))
				assertAdmitted(true)
			}
			server.RaisePgResult(conn.Exec(ctx, `CREATE OR REPLACE FUNCTION transfer_contract_usage_archive_capture() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN OLD; END'`))
			assertAdmitted(false)
		})
	})
}
