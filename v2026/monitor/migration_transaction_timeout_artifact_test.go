package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The application role transaction bound's artifact (v889) follows the
// migration's own rule. An ordinary pool role must carry the 15-minute default,
// so a lost default is drift. A superuser pool role, which the migration skips
// (main's pool role is one), carries no default to lose, so the artifact holds;
// it once paged main as absent forever.
func TestMigrationsTransactionTimeoutArtifactFollowsThePoolRole(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		evaluate := func(query string) bool {
			var value bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, "SELECT "+query).Scan(&value))
			})
			return value
		}
		if evaluate(transactionTimeoutPoolRoleSuperuserQuery) {
			t.Fatal("the test pool role is a superuser; the ordinary-role cases need an ordinary role")
		}
		if !evaluate(transactionTimeoutRoleDefaultArtifactQuery) {
			t.Fatal("the migrated ordinary pool role's bound does not satisfy the artifact")
		}

		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DO $$ BEGIN
				EXECUTE format('ALTER ROLE CURRENT_USER IN DATABASE %I RESET transaction_timeout', current_database());
			END $$`))
		}, server.OptReadWrite())
		if evaluate(transactionTimeoutRoleDefaultArtifactQuery) {
			t.Fatal("an ordinary pool role that lost its default does not read as drift")
		}

		// the pool role is a superuser: the migration set nothing to lose
		superuserRole := `(SELECT rolname FROM pg_roles WHERE rolsuper ORDER BY rolname LIMIT 1)`
		asSuperuserPool := strings.Replace(transactionTimeoutRoleDefaultArtifactQuery, "current_user", superuserRole, 1)
		if asSuperuserPool == transactionTimeoutRoleDefaultArtifactQuery {
			t.Fatal("the artifact does not read the pool role")
		}
		if !evaluate(asSuperuserPool) {
			t.Fatal("a superuser pool role without a default reads as drift")
		}
	})
}
