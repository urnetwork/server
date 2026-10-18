// Migration admission checks the installed custody guard, not just its name
// or the numeric head reported by a mixed-version operator fleet.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A coherent predecessor remains behind; once applied, missing usage custody
// is schema drift independently of any later migration's numeric head.
func TestMigrationsSignalContractUsageGuardLifetime(t *testing.T) {
	for _, version := range []int{744, 745} {
		row := syntheticMigrationArtifactRow(version)
		row[156] = "f"
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "FROM migration_catalog") {
				return syntheticMigrationCatalogRows(version), nil
			}
			if !strings.Contains(query, contractUsageGuardArtifactQuery) {
				t.Fatal("migration admission omitted the actual usage guard")
			}
			return []Row{row}, nil
		}}
		alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if version == 744 {
			requireAlertClass(t, alerts, "migration-behind")
			if len(alerts) != 1 {
				t.Fatalf("predecessor reported unpublished usage drift: %+v", alerts)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "migration-schema-drift")
		if !strings.Contains(alert.Markdown(), "transfer_contract immutable usage and terminal attribution guard@v745") {
			t.Fatalf("missing usage custody was not identified: %+v", alerts)
		}
	}
}

// Exact production catalog reads run over a disposable migrated database.
// Disabling the trigger, changing its events or replacing its body all refuse.
func TestMigrationsSignalContractUsageGuardExecutesActualSchema(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			assertAdmitted := func(want bool) {
				t.Helper()
				var admitted bool
				server.Raise(conn.QueryRow(ctx, `SELECT `+contractUsageGuardArtifactQuery).Scan(&admitted))
				if admitted != want {
					t.Fatalf("actual usage custody admitted=%t, want %t", admitted, want)
				}
			}
			assertAdmitted(true)
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`))
			assertAdmitted(false)
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_usage_guard`))
			assertAdmitted(true)
			server.RaisePgResult(conn.Exec(ctx, `CREATE OR REPLACE FUNCTION transfer_contract_usage_guard() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`))
			assertAdmitted(false)
			server.RaisePgResult(conn.Exec(ctx, `CREATE OR REPLACE FUNCTION transfer_contract_usage_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$`+server.ContractUsageGuardFunctionBodySql+`$guard$`))
			assertAdmitted(true)
			server.RaisePgResult(conn.Exec(ctx, `DROP TRIGGER transfer_contract_usage_guard ON transfer_contract;
				CREATE TRIGGER transfer_contract_usage_guard BEFORE UPDATE ON transfer_contract
				FOR EACH ROW EXECUTE FUNCTION transfer_contract_usage_guard()`))
			assertAdmitted(false)
			server.RaisePgResult(conn.Exec(ctx, `DROP TRIGGER transfer_contract_usage_guard ON transfer_contract;
				CREATE TRIGGER transfer_contract_usage_guard BEFORE INSERT OR UPDATE OF provider_usage ON transfer_contract
				FOR EACH ROW EXECUTE FUNCTION transfer_contract_usage_guard()`))
			assertAdmitted(false)
		})
	})
}
