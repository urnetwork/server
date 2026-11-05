// A role policy can hold before its migration when that migration explicitly
// skips the pool role. This changes no production catalog predicate or lifetime.
package monitor

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The fixture role is observed on its own returned application connection;
// neither a username nor privilege inferred from the launcher is authority.
func migrationPoolRoleSuperuserForTest(t testing.TB, ctx context.Context) bool {
	t.Helper()
	var superuser bool
	server.Db(ctx, func(conn server.PgConn) {
		if err := conn.QueryRow(ctx, "SELECT "+transactionTimeoutPoolRoleSuperuserQuery).Scan(&superuser); err != nil {
			t.Fatal(err)
		}
	})
	return superuser
}

// Only the published transaction-bound predicate explicitly treats an
// unbounded superuser as satisfied. Every other future artifact stays absent.
func migrationCurrentSuffixExpectedPresenceForTest(artifact migrationArtifact, head int, poolRoleSuperuser bool) bool {
	return artifact.requiredVersion <= head || poolRoleSuperuser &&
		artifact.requiredVersion == server.CodeMigrationVersion(server.ApplicationTransactionTimeoutMigrationId)
}

// The old universal-false expectation contradicted the actual superuser
// predicate at prefix 793. Ordinary roles and unrelated versions keep their
// complete before/at/after checks; there is no skip or future-artifact waiver.
func TestMigrationCurrentSuffixTransactionBoundRoleExpectations(t *testing.T) {
	version := server.CodeMigrationVersion(server.ApplicationTransactionTimeoutMigrationId)
	if version <= migrationCurrentSuffixFirstVersion {
		t.Fatal("transaction-bound fixture requires its published later version")
	}
	artifact := migrationCurrentSuffixTestContract(t, version).artifact
	for _, superuser := range []bool{false, true} {
		for _, head := range []int{migrationCurrentSuffixFirstVersion - 1, version - 1, version, version + 1} {
			want := superuser || version <= head
			if got := migrationCurrentSuffixExpectedPresenceForTest(artifact, head, superuser); got != want {
				t.Fatalf("transaction-bound role=%t head=%d present=%t want=%t", superuser, head, got, want)
			}
			for _, other := range migrationCurrentSuffixContracts {
				if other.artifact.requiredVersion == version {
					continue
				}
				if got := migrationCurrentSuffixExpectedPresenceForTest(other.artifact, head, superuser); got != (other.artifact.requiredVersion <= head) {
					t.Fatalf("role=%t changed unrelated artifact %d at head %d", superuser, other.artifact.requiredVersion, head)
				}
			}
		}
	}
}

// The same native role fact used by the complete-prefix test agrees with the
// real unmodified predicate on both sides of this migration's boundary.
func TestMigrationsTransactionTimeoutPublishedPrefixesFollowPoolRole(t *testing.T) {
	version := server.CodeMigrationVersion(server.ApplicationTransactionTimeoutMigrationId)
	contract := migrationCurrentSuffixTestContract(t, version)
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		superuser := migrationPoolRoleSuperuserForTest(tb, tb.Context())
		for _, head := range []int{migrationCurrentSuffixFirstVersion - 1, version - 1, version} {
			server.ApplyDbMigrationsUpTo(tb.Context(), head)
			row, drift := migrationPingDatabaseCheck(tb, tb.Context())
			want := migrationCurrentSuffixExpectedPresenceForTest(contract.artifact, head, superuser)
			if drift != "" || len(row) <= contract.artifact.rowColumn || migrationBool(row[contract.artifact.rowColumn]) != want {
				tb.Fatalf("transaction-bound role=%t prefix=%d want=%t drift=%s", superuser, head, want, drift)
			}
		}
	})
}
