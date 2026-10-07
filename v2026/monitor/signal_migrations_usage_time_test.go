package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func TestMigrationsSignalUsageTimeIndexLifetime(t *testing.T) {
	for _, version := range []int{747, 748} {
		row := syntheticMigrationArtifactRow(version)
		row[159] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			if !strings.Contains(query, providerUsageTimeIndexArtifactQuery) {
				t.Fatal("migration admission omitted missing timestamp index")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if version == 747 {
			requireAlertClass(t, alerts, "migration-behind")
			if len(alerts) != 1 {
				t.Fatalf("unpublished index reported as drift: %+v", alerts)
			}
		} else {
			alert := requireAlertClass(t, alerts, "migration-schema-drift")
			if !strings.Contains(alert.Markdown(), "terminal usage missing timestamp index@v748") {
				t.Fatalf("missing timestamp index not identified: %+v", alerts)
			}
		}
	}
}

// A look-alike index must not pass the rollout gate: its predicate and key
// order determine whether the debt probe has bounded work on a large history.
func TestMigrationsSignalUsageTimeIndexActualSchema(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			assertAdmitted := func(want bool) {
				t.Helper()
				var admitted bool
				server.Raise(conn.QueryRow(ctx, `SELECT `+providerUsageTimeIndexArtifactQuery).Scan(&admitted))
				if admitted != want {
					var definition string
					server.Raise(conn.QueryRow(ctx, `SELECT coalesce(pg_get_indexdef(to_regclass('public.transfer_contract_usage_missing_time')),'')`).Scan(&definition))
					t.Fatalf("usage time index admitted=%t, want %t: %s", admitted, want, definition)
				}
			}
			assertAdmitted(true)
			for _, replacement := range []string{
				`CREATE INDEX transfer_contract_usage_missing_time ON transfer_contract (contract_id)`,
				`CREATE INDEX transfer_contract_usage_missing_time ON transfer_contract (contract_id) WHERE close_time IS NULL`,
				`CREATE INDEX transfer_contract_usage_missing_time ON transfer_contract (close_time, contract_id) WHERE close_time IS NULL AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')`,
				`CREATE INDEX transfer_contract_usage_missing_time ON transfer_contract (contract_id DESC) WHERE close_time IS NULL AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')`,
			} {
				server.RaisePgResult(conn.Exec(ctx, `DROP INDEX transfer_contract_usage_missing_time`))
				assertAdmitted(false)
				server.RaisePgResult(conn.Exec(ctx, replacement))
				assertAdmitted(false)
			}
			// Reapplying rendered DDL is also the database dump/restore path.
			// Its equivalent varchar-array coercion must remain admissible.
			for _, definition := range []string{providerUsageTimeIndexDefinition, providerUsageTimeRestoredIndexDefinition} {
				server.RaisePgResult(conn.Exec(ctx, `DROP INDEX transfer_contract_usage_missing_time`))
				server.RaisePgResult(conn.Exec(ctx, definition))
				assertAdmitted(true)
			}
		})
	})
}
