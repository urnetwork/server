package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The source-generated query is evaluated against actual migration output.
// Every fault rolls back to a healthy shape before the next case.
func TestMigrationReliabilityObservationCatalog(t *testing.T) {
	query := ""
	source := &syntheticSource{postgresFn: func(emitted string) ([]Row, error) {
		if strings.Contains(emitted, "FROM migration_catalog") {
			return syntheticMigrationCatalogRows(server.MigrationCount()), nil
		}
		query = emitted
		return []Row{syntheticMigrationArtifactRow(server.MigrationCount())}, nil
	}}
	if _, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source)); err != nil {
		t.Fatal(err)
	}
	prefix, _, ok := strings.Cut(query, "SELECT version.value,")
	if !ok || !strings.Contains(query, migrationReliabilityObservationArtifactQuery) {
		t.Fatal("missing observation contract from the actual migration query")
	}
	query = prefix + "SELECT " + migrationReliabilityObservationArtifactQuery
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			read := func() bool {
				var matches bool
				server.Raise(tx.QueryRow(ctx, query).Scan(&matches))
				return matches
			}
			if !read() {
				t.Fatal("current migration did not satisfy its observation guard")
			}
			for _, fault := range []struct{ name, sql string }{
				{"count default", `ALTER TABLE client_reliability_running ALTER COLUMN observed_row_count SET DEFAULT 1`},
				{"count type", `ALTER TABLE client_reliability_running ALTER COLUMN observed_row_count TYPE integer`},
				{"count nullability", `ALTER TABLE client_reliability_running ALTER COLUMN observed_row_count DROP NOT NULL`},
				{"negative count", `ALTER TABLE client_reliability_running DROP CONSTRAINT client_reliability_running_observed_row_count_check`},
				{"version default", `ALTER TABLE client_reliability_running_window ALTER COLUMN observation_version SET DEFAULT 1`},
				{"fabricated token", `ALTER TABLE client_reliability_running_window ALTER COLUMN observation_write_token SET DEFAULT gen_random_uuid()`},
				{"disabled guard", `ALTER TABLE client_reliability_running_window DISABLE TRIGGER client_reliability_running_window_observation_guard`},
				{"late guard", `DROP TRIGGER client_reliability_running_window_observation_guard ON client_reliability_running_window; CREATE TRIGGER client_reliability_running_window_observation_guard AFTER INSERT OR UPDATE ON client_reliability_running_window FOR EACH ROW EXECUTE FUNCTION client_reliability_running_window_observation_guard()`},
				{"missing insert guard", `DROP TRIGGER client_reliability_running_window_observation_guard ON client_reliability_running_window; CREATE TRIGGER client_reliability_running_window_observation_guard BEFORE UPDATE ON client_reliability_running_window FOR EACH ROW EXECUTE FUNCTION client_reliability_running_window_observation_guard()`},
				{"conditional guard", `DROP TRIGGER client_reliability_running_window_observation_guard ON client_reliability_running_window; CREATE TRIGGER client_reliability_running_window_observation_guard BEFORE INSERT OR UPDATE ON client_reliability_running_window FOR EACH ROW WHEN (false) EXECUTE FUNCTION client_reliability_running_window_observation_guard()`},
				{"no-op guard", `CREATE OR REPLACE FUNCTION client_reliability_running_window_observation_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`},
				{"missing future schema", `DROP FUNCTION client_reliability_running_window_observation_guard() CASCADE; ALTER TABLE client_reliability_running DROP COLUMN observed_row_count; ALTER TABLE client_reliability_running_window DROP COLUMN observation_version,DROP COLUMN observation_write_token`},
			} {
				server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT observation_catalog_fault`))
				server.RaisePgResult(tx.Exec(ctx, fault.sql))
				if read() {
					t.Fatalf("%s passed observation catalog guard", fault.name)
				}
				server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT observation_catalog_fault`))
				if !read() {
					t.Fatalf("%s recovery did not restore catalog health", fault.name)
				}
			}
		})
	})
}
