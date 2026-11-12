//go:build sidecar_pending

package monitor

import (
	"testing"

	"github.com/urnetwork/server"
)

// The catalog signal is healthy on both sides of MG0's permissive step: below
// it the intake-first contract holds and the successor does not, and from it on
// the successor holds, with no drift either side. Retiring the predecessor
// without republishing its custody, or the reverse, fails here.
func TestMigrationsCompetitionSidecarSourcePrefixesRemainCoherent(t *testing.T) {
	version := server.CompetitionSidecarSourceMigrationVersion()
	if version < 1 {
		t.Fatal("a pending build does not register the permissive step")
	}
	predecessor := migrationCurrentSuffixTestContract(t, server.CompetitionIntakeFirstMigrationVersion())
	contract := migrationCurrentSuffixTestContract(t, version)
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		for _, head := range []int{version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			var actualHead int
			server.Db(tb.Context(), func(conn server.PgConn) {
				server.Raise(conn.QueryRow(tb.Context(), `SELECT COALESCE(max(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&actualHead))
			})
			if actualHead != head {
				tb.Fatalf("fixture schema = %d, want %d", actualHead, head)
			}
			row, drift := migrationPingDatabaseCheck(tb, tb.Context())
			wantPredecessor, want := "t", "f"
			if head == version {
				wantPredecessor, want = "f", "t"
			}
			if drift != "" || row[predecessor.artifact.rowColumn] != wantPredecessor || row[contract.artifact.rowColumn] != want {
				tb.Fatalf("head %d is incoherent: intake-first %q want %q, successor %q want %q, drift %s",
					head, row[predecessor.artifact.rowColumn], wantPredecessor, row[contract.artifact.rowColumn], want, drift)
			}
		}
	})
}
