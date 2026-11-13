// The composed suffix replaces exactly four body custodians. Their complete
// remaining predicates survive in the new exact-version contract.
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// An early, missing or blanket retirement would either hide old drift or make
// the new schema permanently unhealthy. No hardcoded unpublished ordinal.
func TestMigrationsCompetitionIntakeFirstIntegrationRetiresExactlyFourOwners(t *testing.T) {
	version := server.CompetitionIntakeFirstMigrationVersion()
	if version < 1 {
		t.Fatal("intake custody cannot qualify before exact migration registration")
	}
	expected := map[int]bool{846: true, server.CompetitionDistributedEvaluationMigrationVersion(): true,
		server.CompetitionProductionBenefitMigrationVersion(): true, server.CompetitionWorkCoverageMigrationVersion(): true}
	if len(expected) != 4 || expected[0] {
		t.Fatal("four distinct registered predecessor owners are required")
	}
	retired := map[int]bool{}
	for _, contract := range migrationCurrentSuffixContracts {
		if contract.artifact.removedVersion == version {
			retired[contract.artifact.requiredVersion] = true
		}
	}
	if len(retired) != len(expected) {
		t.Fatalf("intake retired %v instead of exactly %v", retired, expected)
	}
	for predecessor := range expected {
		if predecessor >= version || !retired[predecessor] {
			t.Fatalf("predecessor %d was retired early, late or not at all", predecessor)
		}
	}
	// its own custody moves, whole, to the review handoff that re-created three
	// of its winner-review bodies
	contract := migrationCurrentSuffixTestContract(t, version)
	handoff := server.CompetitionReviewHandoffMigrationVersion()
	if handoff <= version || contract.artifact.removedVersion != handoff || contract.artifact.rowColumn != version-589 || contract.query != competitionIntakeFirstArtifactQuery() {
		t.Fatal("integrated intake does not pin the exact complete custody query until its registered successor")
	}
	oldCoverage := migrationCurrentSuffixTestContract(t, server.CompetitionWorkCoverageMigrationVersion())
	if !strings.Contains(oldCoverage.query, "p.prosrc="+snMainnetMigrationLiteral(server.CompetitionIntakeFirstLegacyCoverageRoundBodySql())) {
		t.Fatal("coverage predecessor was rewritten instead of retired unchanged")
	}
}

// The actual catalog signal must be healthy on both sides of the append.
// Updating a contract row without replacing its old-body predicate fails here.
func TestMigrationsCompetitionIntakeFirstIntegratedPrefixesRemainCoherent(t *testing.T) {
	version := server.CompetitionIntakeFirstMigrationVersion()
	if version < 1 {
		t.Fatal("integrated intake prefix test requires its registered migration")
	}
	contract := migrationCurrentSuffixTestContract(t, version)
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			var actualHead int
			server.Db(tb.Context(), func(conn server.PgConn) {
				server.Raise(conn.QueryRow(tb.Context(), `SELECT COALESCE(max(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&actualHead))
			})
			if actualHead != head {
				tb.Fatalf("intake prefix fixture schema=%d want=%d", actualHead, head)
			}
			tb.Logf("intake_prefix_fixture_schema_head=%d", actualHead)
			row, drift := migrationPingDatabaseCheck(tb, tb.Context())
			want := "f"
			if head == version {
				want = "t"
			}
			if drift != "" || row[contract.artifact.rowColumn] != want {
				tb.Fatalf("intake prefix %d is incoherent: new=%q expected=%q drift=%s", head, row[contract.artifact.rowColumn], want, drift)
			}
		}
	})
}
