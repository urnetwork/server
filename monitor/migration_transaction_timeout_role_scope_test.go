// A maintenance or other ordinary role's default is not evidence that the
// observing application role kept its own transaction bound.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Only two catalog inputs are projected. Every other catalog, the complete
// production artifact query and the actual signal implementation stay native.
type transactionTimeoutRoleCatalogSource struct {
	migrationPingDatabaseSource
	superuser, sameDatabase  bool
	ownDefault, otherDefault string
}

// Role OIDs and names exist only within this read-only statement. No role,
// default, grant, database object or application configuration is changed.
func (self *transactionTimeoutRoleCatalogSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	if strings.Contains(query, "FROM migration_catalog") {
		return self.migrationPingDatabaseSource.PostgreSQL(ctx, query)
	}
	query = strings.TrimSpace(query)
	if !strings.HasPrefix(query, "WITH ") {
		return nil, fmt.Errorf("transaction-bound projection requires the complete catalog query")
	}
	settings := func(value string) string {
		if value == "" {
			return "ARRAY[]::text[]"
		}
		return "ARRAY[" + snMainnetMigrationLiteral(value) + "]::text[]"
	}
	projection := fmt.Sprintf(`pg_roles AS (
 SELECT 1::oid AS oid,current_user::text AS rolname,%t AS rolsuper
 UNION ALL SELECT 2::oid,current_user::text||'/synthetic-other-pool',false
), pg_db_role_setting AS (
 SELECT 1::oid AS setrole,
  CASE WHEN %t THEN (SELECT oid FROM pg_database WHERE datname=current_database()) ELSE 0::oid END AS setdatabase,
  %s AS setconfig
 UNION ALL SELECT 2::oid,(SELECT oid FROM pg_database WHERE datname=current_database()),%s
)`, self.superuser, self.sameDatabase, settings(self.ownDefault), settings(self.otherDefault))
	return self.migrationPingDatabaseSource.PostgreSQL(ctx, "WITH "+projection+", "+strings.TrimPrefix(query, "WITH "))
}

// Both parts of the catalog row must identify the role whose session the
// monitor observes; the explicit superuser exemption remains unchanged.
func TestMigrationsTransactionTimeoutArtifactPinsObservedRole(t *testing.T) {
	if !strings.Contains(transactionTimeoutRoleDefaultArtifactQuery, "role_row.rolname = current_user") ||
		!strings.Contains(transactionTimeoutRoleDefaultArtifactQuery, transactionTimeoutPoolRoleSuperuserQuery) {
		t.Fatal("transaction-bound predicate is not scoped to the observing role")
	}
}

// Two ordinary roles reproduce the false green: only the other pool has the
// 15-minute default. The same native full-probe path also proves positive own
// defaults, lost/wrong defaults, wrong databases and the superuser exemption.
func TestMigrationsTransactionTimeoutOtherRoleCannotMaskMissingDefault(t *testing.T) {
	version := server.CodeMigrationVersion(server.ApplicationTransactionTimeoutMigrationId)
	contract := migrationCurrentSuffixTestContract(t, version)
	owner := fmt.Sprintf("%s@v%d", contract.artifact.name, version)
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		for _, fixture := range []struct {
			superuser, sameDatabase  bool
			ownDefault, otherDefault string
			want                     bool
		}{
			{sameDatabase: true, otherDefault: "transaction_timeout=15min"},
			{sameDatabase: true, ownDefault: "transaction_timeout=15min", want: true},
			{ownDefault: "transaction_timeout=15min", otherDefault: "transaction_timeout=15min"},
			{sameDatabase: true, ownDefault: "transaction_timeout=10min", otherDefault: "transaction_timeout=15min"},
			{sameDatabase: true, ownDefault: "transaction_timeout=15min", otherDefault: "transaction_timeout=15min", want: true},
			{sameDatabase: true},
			{superuser: true, sameDatabase: true, want: true},
		} {
			source := &transactionTimeoutRoleCatalogSource{
				superuser: fixture.superuser, sameDatabase: fixture.sameDatabase,
				ownDefault: fixture.ownDefault, otherDefault: fixture.otherDefault,
			}
			alerts, err := NewMigrationsSignal().Run(tb.Context(), syntheticSettings(source))
			if err != nil || len(source.artifactRow) <= contract.artifact.rowColumn {
				tb.Fatal("native transaction-bound catalog probe failed", err)
			}
			present := migrationBool(source.artifactRow[contract.artifact.rowColumn])
			matched := false
			for _, alert := range alerts {
				matched = matched || alert.Class == "migration-schema-drift" && strings.Contains(alert.Markdown(), owner)
			}
			if present != fixture.want || matched == fixture.want {
				tb.Fatalf("observing-role bound=%t want=%t drift=%t for own=%q other=%q superuser=%t same-database=%t", present, fixture.want, matched, fixture.ownDefault, fixture.otherDefault, fixture.superuser, fixture.sameDatabase)
			}
		}
	})
}
