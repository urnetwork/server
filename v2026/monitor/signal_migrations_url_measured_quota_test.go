package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The source-generated query is evaluated against actual migration output.
// Every fault rolls back to a healthy shape before the next case.
func TestMigrationUrlMeasuredQuotaCatalog(t *testing.T) {
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
	if !ok || !strings.Contains(query, migrationUrlProbeMeasuredQuotaArtifactQuery) {
		t.Fatal("missing measured quota contract from the actual migration query")
	}
	query = prefix + "SELECT " + migrationUrlProbeMeasuredQuotaArtifactQuery
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			read := func() bool {
				var matches bool
				server.Raise(tx.QueryRow(ctx, query).Scan(&matches))
				return matches
			}
			if !read() {
				t.Fatal("current migration did not satisfy its measured quota index guard")
			}
			for _, fault := range []struct{ name, sql string }{
				{"missing index", `DROP INDEX provider_egress_health_history_url_run`},
				{"success-only index", `DROP INDEX provider_egress_health_history_url_run; CREATE INDEX provider_egress_health_history_url_run ON provider_egress_health_history(client_id,measured_at DESC) WHERE url_probe AND url_probe_policy_version=1 AND total_count=1 AND ok_count=1`},
				{"future policy admitted", `DROP INDEX provider_egress_health_history_url_run; CREATE INDEX provider_egress_health_history_url_run ON provider_egress_health_history(client_id,measured_at DESC) WHERE url_probe AND url_probe_policy_version>=1 AND total_count=1 AND (ok_count=0 OR ok_count=1)`},
				{"unmeasured admitted", `DROP INDEX provider_egress_health_history_url_run; CREATE INDEX provider_egress_health_history_url_run ON provider_egress_health_history(client_id,measured_at DESC) WHERE url_probe AND url_probe_policy_version=1 AND (ok_count=0 OR ok_count=1)`},
				{"wrong key order", `DROP INDEX provider_egress_health_history_url_run; CREATE INDEX provider_egress_health_history_url_run ON provider_egress_health_history(measured_at DESC,client_id) WHERE url_probe AND url_probe_policy_version=1 AND total_count=1 AND (ok_count=0 OR ok_count=1)`},
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
