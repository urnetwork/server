// Catalog faults test coverage custody independently from application guards.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Every operative function and trigger has an exact predicate, including the
// old statistical eligibility functions invoked by the new wrapper.
func TestMigrationsWorkCoverageExactCustodyCensus(t *testing.T) {
	query := strings.Join(competitionWorkCoverageArtifactQueries(), " AND ")
	for name, body := range server.CompetitionWorkCoverageBodiesSql() {
		if !strings.Contains(query, "p.prosrc="+snMainnetMigrationLiteral(body)) {
			t.Fatalf("coverage %s body is unowned", name)
		}
	}
	for _, fragment := range []string{"competition_v2_score_eligible_mode(jsonb,double precision,double precision,boolean)", "competition_work_coverage_job", "competition_work_coverage_baseline", "competition_work_coverage_seal", "competition_work_coverage_source", "competition_work_coverage_round", "competition_work_coverage_equivalence", "p.proparallel='s'", "NOT p.prosecdef", "t.tgtype=23", "t.tgattr=''::int2vector", "t.tgqual IS NULL"} {
		if !strings.Contains(query, fragment) {
			t.Errorf("coverage custody omitted %s", fragment)
		}
	}
}

// Integration must register both migration and monitor before qualification.
// Actual catalogs establish the healthy before/after controls; each weakened
// body or trigger is rolled back and must fail the independent catalog probe.
func TestMigrationsWorkCoveragePublishedPrefixAndGuardFaults(t *testing.T) {
	version := server.CompetitionWorkCoverageMigrationVersion()
	if version == 0 {
		t.Fatal("append coverage SQL and register its monitor before running integrated qualification")
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	if contract.query != competitionWorkCoverageMigrationContract().query {
		t.Fatal("registered coverage custody differs from authored contract")
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
				tb.Fatalf("coverage prefix %d custody=%q want=%q drift=%s", head, row[contract.artifact.rowColumn], want, drift)
			}
		}
		faults := []struct{ name, sql string }{
			{name: "score helper weakened", sql: `CREATE OR REPLACE FUNCTION competition_work_coverage_score_valid(score jsonb,evaluation_policy jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$ BEGIN RETURN true; END $$`},
			{name: "eligibility metadata changed", sql: `ALTER FUNCTION competition_v2_score_eligible_policy(jsonb,double precision,double precision,boolean,jsonb) VOLATILE`},
			{name: "source privilege changed", sql: `ALTER FUNCTION competition_work_coverage_source_guard() SECURITY DEFINER`},
			{name: "policy parallel metadata changed", sql: `ALTER FUNCTION competition_work_coverage_policy_valid(jsonb) PARALLEL UNSAFE`},
			{name: "source trigger narrowed", sql: `DROP TRIGGER competition_work_coverage_source ON competition_source_activation; CREATE TRIGGER competition_work_coverage_source BEFORE INSERT ON competition_source_activation FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_source_guard()`},
			{name: "job trigger column restricted", sql: `DROP TRIGGER competition_work_coverage_job ON competition_job; CREATE TRIGGER competition_work_coverage_job BEFORE INSERT OR UPDATE OF score_json ON competition_job FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_job_guard()`},
		}
		for _, table := range []struct{ name, table string }{
			{name: "job", table: "competition_job"}, {name: "baseline", table: "competition_round_baseline"}, {name: "seal", table: "competition_evaluation_seal"},
			{name: "source", table: "competition_source_activation"}, {name: "round", table: "competition_round"},
			{name: "equivalence", table: "competition_executor_equivalence"},
		} {
			faults = append(faults, struct{ name, sql string }{name: table.name + " disabled", sql: "ALTER TABLE " + table.table + " DISABLE TRIGGER competition_work_coverage_" + table.name})
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(tb, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					tb.Fatalf("apply %s: %v", fault.name, err)
				}
				var healthy bool
				if err := tx.QueryRow(ctx, "SELECT "+contract.query).Scan(&healthy); err != nil || healthy {
					tb.Fatalf("%s escaped coverage custody: healthy=%t err=%v", fault.name, healthy, err)
				}
			})
		}
		if _, drift := migrationPingDatabaseCheck(tb, tb.Context()); drift != "" {
			tb.Fatal("rolled-back coverage faults left drift", drift)
		}
	})
}
