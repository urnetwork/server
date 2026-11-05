package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The actual pool role satisfies the migrated predicate. Catalog projections
// exercise both role classes without grants or production-query changes;
// ordinary fixtures also retain the real lost-default regression.
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
		poolSuperuser := evaluate(transactionTimeoutPoolRoleSuperuserQuery)
		if !evaluate(transactionTimeoutRoleDefaultArtifactQuery) {
			t.Fatal("the migrated pool role does not satisfy its actual bound or superuser exemption")
		}
		// Ordinary fixtures retain the real lost-default regression. A
		// superuser has no default to remove; projections below still test
		// both ordinary states without granting or changing privileges.
		if !poolSuperuser {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `DO $$ BEGIN
					EXECUTE format('ALTER ROLE CURRENT_USER IN DATABASE %I RESET transaction_timeout', current_database());
				END $$`))
			}, server.OptReadWrite())
			if evaluate(transactionTimeoutRoleDefaultArtifactQuery) {
				t.Fatal("an ordinary pool role that lost its default does not read as drift")
			}
		}
		// Retain the existing real-catalog superuser control as well as the
		// projections: a skipped default must not page as schema drift.
		superuserRole := `(SELECT rolname FROM pg_roles WHERE rolsuper ORDER BY rolname LIMIT 1)`
		asSuperuserPool := strings.Replace(transactionTimeoutRoleDefaultArtifactQuery, "current_user", superuserRole, 1)
		if asSuperuserPool == transactionTimeoutRoleDefaultArtifactQuery || !evaluate(asSuperuserPool) {
			t.Fatal("a superuser pool role without a default reads as drift")
		}
		for _, fixture := range []struct {
			superuser, hasDefault, sameDatabase bool
			want                                bool
		}{
			{hasDefault: true, sameDatabase: true, want: true},
			{sameDatabase: true},
			{hasDefault: true},
			{superuser: true, sameDatabase: true, want: true},
		} {
			query := fmt.Sprintf(`WITH pg_roles AS (
 SELECT 1::oid AS oid,current_user::name AS rolname,%t AS rolsuper
), pg_db_role_setting AS (
 SELECT 1::oid AS setrole,
  CASE WHEN %t THEN (SELECT oid FROM pg_database WHERE datname=current_database()) ELSE 0::oid END AS setdatabase,
  CASE WHEN %t THEN ARRAY['transaction_timeout=15min']::text[] ELSE ARRAY[]::text[] END AS setconfig
) SELECT `+transactionTimeoutRoleDefaultArtifactQuery, fixture.superuser, fixture.sameDatabase, fixture.hasDefault)
			var value bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, query).Scan(&value))
			})
			if value != fixture.want {
				t.Fatalf("projected role=%t default=%t same-database=%t present=%t want=%t", fixture.superuser, fixture.hasDefault, fixture.sameDatabase, value, fixture.want)
			}
		}
		if !strings.Contains(transactionTimeoutRoleDefaultArtifactQuery, transactionTimeoutPoolRoleSuperuserQuery) ||
			!strings.Contains(transactionTimeoutRoleDefaultArtifactQuery, "setting_row.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())") {
			t.Fatal("transaction-bound predicate lost its role or database scope")
		}
	})
}
