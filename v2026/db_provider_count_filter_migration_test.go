// Sparse complete-exclusion indexes must recover online and match schema audit replay.
package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Resolve the actual appended migration, never an independently copied DDL fixture.
func testingProviderCountFilterMigration(t testing.TB) *OnlineSqlMigration {
	t.Helper()
	var found *OnlineSqlMigration
	for _, migration := range migrations {
		if online, ok := migration.(*OnlineSqlMigration); ok && strings.Contains(online.sql, "network_client_location_reliability_arin_exceptions") {
			if found != nil {
				t.Fatal("duplicate complete-exclusion index migration")
			}
			found = online
		}
	}
	if found == nil {
		t.Fatal("missing complete-exclusion index migration")
	}
	return found
}

// Concurrent recovery and create are separate calls, while audit replay has
// exactly the same index definition without transaction-forbidden keywords.
func TestProviderCountFilterMigrationIsRestartableOnline(t *testing.T) {
	migration := testingProviderCountFilterMigration(t)
	steps := migration.productionSqlSteps()
	if len(steps) != 2 {
		t.Fatal("index recovery and creation must be separate online statements")
	}
	normalize := func(sql string) string { return strings.Join(strings.Fields(sql), " ") }
	if normalize(steps[0]) != "DROP INDEX CONCURRENTLY IF EXISTS network_client_location_reliability_arin_exceptions" {
		t.Fatal("online recovery does not clear interrupted index residue")
	}
	if normalize(steps[1]) != "CREATE INDEX CONCURRENTLY network_client_location_reliability_arin_exceptions ON network_client_location_reliability (client_id) INCLUDE (arin_risk, arin_non_quality) WHERE arin_risk OR arin_non_quality" {
		t.Fatal("online sparse covering index definition changed")
	}
	if normalize(migration.auditSql) != strings.ReplaceAll(normalize(steps[0])+"; "+normalize(steps[1]), " CONCURRENTLY", "") {
		t.Fatal("audit replay differs from online index definition")
	}
	var calls []string
	if err := executeOnlineSqlMigration(t.Context(), migration, func(_ context.Context, query string) error {
		calls = append(calls, query)
		return nil
	}); err != nil || len(calls) != 2 || calls[0] != steps[0] || calls[1] != steps[1] {
		t.Fatal("online index steps were combined or reordered")
	}
}

// A disposable database exercises the exact production executor twice over
// same-name residue and compares its live catalog with actual audit SQL replay.
func TestProviderCountFilterMigrationReplaysAgainstPostgres(t *testing.T) {
	testEnv := DefaultTestEnv()
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		migration := testingProviderCountFilterMigration(t)
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE network_client_location_reliability(
				client_id uuid PRIMARY KEY, arin_risk bool NOT NULL, arin_non_quality bool NOT NULL)`))
			for _, flags := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
				RaisePgResult(conn.Exec(ctx, `INSERT INTO network_client_location_reliability VALUES($1,$2,$3)`, NewId(), flags[0], flags[1]))
			}
			RaisePgResult(conn.Exec(ctx, `CREATE INDEX network_client_location_reliability_arin_exceptions
				ON network_client_location_reliability(arin_risk)`))
			readDefinition := func() string {
				t.Helper()
				var definition string
				var valid, ready bool
				Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid),indisvalid,indisready
					FROM pg_index WHERE indexrelid='network_client_location_reliability_arin_exceptions'::regclass`).Scan(&definition, &valid, &ready))
				if !valid || !ready {
					t.Fatal("complete-exclusion index is not valid and ready")
				}
				return definition
			}
			for range 2 {
				Raise(executeOnlineSqlMigration(ctx, migration, func(ctx context.Context, query string) error {
					_, err := conn.Exec(ctx, query)
					return err
				}))
				if readDefinition() != "CREATE INDEX network_client_location_reliability_arin_exceptions ON public.network_client_location_reliability USING btree (client_id) INCLUDE (arin_risk, arin_non_quality) WHERE (arin_risk OR arin_non_quality)" {
					t.Fatal("production replay did not install the sparse covering index")
				}
			}
			productionDefinition := readDefinition()
			RaisePgResult(conn.Exec(ctx, migration.auditSql))
			if readDefinition() != productionDefinition {
				t.Fatal("schema audit replay changed index definition")
			}
			var exceptions int
			Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client_location_reliability WHERE arin_risk OR arin_non_quality`).Scan(&exceptions))
			if exceptions != 3 {
				t.Fatal("index migration changed exception rows")
			}
		}, OptReadWrite(), OptNoRetry())
	})
}
