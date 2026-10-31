// Published index shape and catalog authority are separate from score freshness.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Pending DDL only warns about lag; a known missing index pages, while a
// future database cannot be certified from this older index predicate.
func TestMigrationsCompetitionSubmissionMetricsIndexLifetimeAndFuture(t *testing.T) {
	version := server.CompetitionSubmissionMetricsMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.artifact.rowColumn != version-589 || contract.artifact.removedVersion != 0 {
		t.Fatalf("metrics index contract is %+v", contract.artifact)
	}
	compiledHead := server.MigrationCount()
	for _, test := range []struct {
		name    string
		head    int
		present bool
		class   string
	}{
		{name: "pending index", head: version - 1},
		{name: "published index", head: version, present: true},
		{name: "missing published index", head: version, class: "migration-schema-drift"},
		{name: "unknown successor changed index", head: compiledHead + 1, class: "cannot-observe"},
	} {
		row := syntheticMigrationArtifactRow(test.head)
		row[contract.artifact.rowColumn] = "f"
		if test.present {
			row[contract.artifact.rowColumn] = "t"
		}
		catalog := syntheticMigrationCatalogRows(min(test.head, compiledHead))
		if compiledHead < test.head {
			catalog = migrationFutureSchemaCatalogRows()
		}
		alerts := migrationFutureSchemaAlerts(t, NewMigrationsSignal(), test.name, row, catalog)
		classes := map[string]int{}
		for _, alert := range alerts {
			classes[alert.Class]++
			switch alert.Class {
			case "migration-behind":
				if alert.Severity != SeverityWarn || alert.PageSustain != 0 {
					t.Fatalf("%s promoted numeric migration lag", test.name)
				}
			case "migration-schema-drift":
				owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
				if test.class != alert.Class || alert.Severity != SeverityPage || !strings.Contains(alert.Observed, owner) {
					t.Fatalf("%s lost exact metrics index drift ownership", test.name)
				}
			case "cannot-observe":
				if test.class != alert.Class || alert.Severity != SeverityWarn || !alert.unobservable || !strings.Contains(alert.Observed, fmt.Sprintf("catalog_known_version=%d", compiledHead)) {
					t.Fatalf("%s falsely certified a future index", test.name)
				}
			default:
				t.Fatalf("%s emitted unexpected class %s", test.name, alert.Class)
			}
		}
		wantLag := 0
		if test.head < compiledHead {
			wantLag = 1
		}
		wantAlerts := wantLag
		if test.class != "" {
			wantAlerts++
		}
		if classes["migration-behind"] != wantLag || len(alerts) != wantAlerts || (test.class != "" && classes[test.class] != 1) {
			t.Fatalf("%s classes=%v, want lag=%d and class=%s", test.name, classes, wantLag, test.class)
		}
	}
}

// Real published prefixes supply the healthy oracle; each DDL fault and the
// public signal's catalog reads share one transaction before rollback.
func TestMigrationsCompetitionSubmissionMetricsIndexPublishedSchemaAndFaults(t *testing.T) {
	version := server.CompetitionSubmissionMetricsMigrationVersion()
	contract := migrationCurrentSuffixTestContract(t, version)
	owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
	faults := []struct{ name, sql string }{
		{"index absent", `DROP INDEX competition_job_completed_score_metrics_idx`},
		{"keys reversed", `DROP INDEX competition_job_completed_score_metrics_idx; CREATE INDEX competition_job_completed_score_metrics_idx ON competition_job(job_id,round_id) WHERE state IN ('succeeded','failed') AND score_json IS NOT NULL`},
		{"full index", `DROP INDEX competition_job_completed_score_metrics_idx; CREATE INDEX competition_job_completed_score_metrics_idx ON competition_job(round_id,job_id)`},
		{"failed scores omitted", `DROP INDEX competition_job_completed_score_metrics_idx; CREATE INDEX competition_job_completed_score_metrics_idx ON competition_job(round_id,job_id) WHERE state='succeeded' AND score_json IS NOT NULL`},
		{"unscored terminal jobs admitted", `DROP INDEX competition_job_completed_score_metrics_idx; CREATE INDEX competition_job_completed_score_metrics_idx ON competition_job(round_id,job_id) WHERE state IN ('succeeded','failed')`},
		{"wrong method", `DROP INDEX competition_job_completed_score_metrics_idx; CREATE INDEX competition_job_completed_score_metrics_idx ON competition_job USING hash(round_id) WHERE state IN ('succeeded','failed') AND score_json IS NOT NULL`},
		{"invented uniqueness", `DROP INDEX competition_job_completed_score_metrics_idx; CREATE UNIQUE INDEX competition_job_completed_score_metrics_idx ON competition_job(round_id,job_id) WHERE state IN ('succeeded','failed') AND score_json IS NOT NULL`},
	}
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			row, drift := migrationPingDatabaseCheck(tb, tb.Context())
			want := "f"
			if head == version {
				want = "t"
			}
			if drift != "" || row[contract.artifact.rowColumn] != want {
				tb.Fatalf("published prefix %d metrics index=%q want=%q drift=%s", head, row[contract.artifact.rowColumn], want, drift)
			}
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					tb.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					tb.Fatalf("inspect %s: %v", fault.name, err)
				}
				alert := requireAlertClass(t, alerts, "migration-schema-drift")
				if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, owner) {
					tb.Fatalf("%s lost exact metrics index owner", fault.name)
				}
			})
		}
		if _, drift := migrationPingDatabaseCheck(tb, tb.Context()); drift != "" {
			tb.Fatalf("rolled-back metrics index faults left drift: %s", drift)
		}
	})
}

// Read the real published definition once, then vary only independent catalog
// readiness flags on one owned connection; no system catalogs are changed.
func TestMigrationsCompetitionSubmissionMetricsIndexReadiness(t *testing.T) {
	contract := migrationCurrentSuffixTestContract(t, server.CompetitionSubmissionMetricsMigrationVersion())
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			var definition, predicate string
			server.Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(i.indexrelid),pg_get_expr(i.indpred,i.indrelid) FROM pg_index i WHERE i.indexrelid=to_regclass('public.competition_job_completed_score_metrics_idx')`).Scan(&definition, &predicate))
			for _, test := range []struct{ valid, ready, want bool }{
				{valid: true, ready: true, want: true},
				{valid: false, ready: true},
				{valid: true, ready: false},
				{valid: false, ready: false},
			} {
				var matched bool
				server.Raise(conn.QueryRow(ctx, `WITH index_artifact AS (SELECT 'competition_job'::text AS table_name,'competition_job_completed_score_metrics_idx'::text AS index_name,$1::text AS definition,$2::text AS predicate_definition,$3::boolean AS indisvalid,$4::boolean AS indisready) SELECT `+contract.query, definition, predicate, test.valid, test.ready).Scan(&matched))
				if matched != test.want {
					t.Fatalf("valid=%t ready=%t matched=%t want=%t", test.valid, test.ready, matched, test.want)
				}
			}
		})
	})
}
