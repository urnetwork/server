// The index diet's drops (db_index_diet_migrations.go), read back from the
// migration list and from migrated schemas.
package server

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The index diet migrations in declaration order.
func indexDietTestMigrations(t testing.TB) []IndexDietMigration {
	t.Helper()
	ordered := []IndexDietMigration{}
	for migration := IndexDietReapPendingIndexDrop; ; migration++ {
		if _, ok := indexDietMigrations[migration]; !ok {
			if int(migration) != len(indexDietMigrations) || len(indexDietDroppedIndexes) != len(indexDietMigrations) {
				t.Fatalf("index diet migrations are not numbered contiguously: %d of %d", migration, len(indexDietMigrations))
			}
			return ordered
		}
		ordered = append(ordered, migration)
	}
}

// Each drop is appended to the migration list: its version is its position,
// the drops are listed in the order they were written, each after the
// migration that last created its index, and each is exactly the online drop
// of its index with the transaction-safe drop as its audit form.
func TestIndexDietMigrationsAreAppendedDrops(t *testing.T) {
	previousVersion := 0
	for _, migration := range indexDietTestMigrations(t) {
		target := indexDietMigrations[migration]
		version := IndexDietMigrationVersion(migration)
		if version < 1 || len(migrations) < version || migrations[version-1] != target {
			t.Fatalf("index diet migration %d version %d is not its position", migration, version)
		}
		if version <= previousVersion {
			t.Fatalf("index diet migration %d version %d is not after %d", migration, version, previousVersion)
		}
		previousVersion = version
		name := indexDietDroppedIndexes[migration]
		if target.sql != `DROP INDEX CONCURRENTLY IF EXISTS `+name || target.auditSql != `DROP INDEX IF EXISTS `+name {
			t.Fatalf("index diet migration %d is not the drop of %s: %q / %q", migration, name, target.sql, target.auditSql)
		}
		if target.recoverySql != "" || target.completion != nil || target.contractIndex != nil {
			t.Fatalf("index diet migration %d has steps beyond its drop", migration)
		}
		creation := regexp.MustCompile(`CREATE (UNIQUE )?INDEX (CONCURRENTLY )?(IF NOT EXISTS )?` + name + `\s`)
		created := 0
		for index, listed := range migrations {
			var sql string
			switch typed := listed.(type) {
			case *SqlMigration:
				sql = typed.sql
			case *OnlineSqlMigration:
				sql = typed.sql
			}
			if creation.MatchString(sql) {
				created = index + 1
			}
		}
		if created == 0 || version <= created {
			t.Fatalf("%s is dropped at %d, not after its creation at %d", name, version, created)
		}
	}
}

// Whether an index of the name exists, and whether it is valid and ready.
func indexDietTestIndexState(t testing.TB, ctx context.Context, name string) (exists bool, usable bool) {
	t.Helper()
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid = to_regclass('public.'||$1)`, name)
		WithPgResult(result, err, func() {
			if result.Next() {
				exists = true
				Raise(result.Scan(&usable))
			}
		})
	}, OptReadOnly())
	return
}

// The operator drops each index by hand before its migration runs. The
// schema audit then reports none of them missing, so its fix never recreates
// one, and the migrations apply as no-ops. Replaying a drop or its audit form
// on a database without the index changes nothing, and the head audits clean.
func TestIndexDietDropsRecordHandDrops(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ordered := indexDietTestMigrations(t)
		first := IndexDietMigrationVersion(ordered[0])
		ApplyDbMigrationsUpTo(ctx, first-1)
		names := []string{}
		for _, migration := range ordered {
			name := indexDietDroppedIndexes[migration]
			if exists, usable := indexDietTestIndexState(t, ctx, name); !exists || !usable {
				t.Fatalf("%s is not a usable index before its drop: exists=%t usable=%t", name, exists, usable)
			}
			names = append(names, name)
		}

		// the operator's statement, on a direct session outside a transaction
		MaintenanceDb(ctx, func(conn PgConn) {
			for _, name := range names {
				RaisePgResult(conn.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+name))
			}
		}, OptReadWrite(), OptNoRetry())

		before := AuditSchema(ctx)
		for _, name := range names {
			if slices.Contains(before.Diff.missingIdx, name) {
				t.Fatalf("the audit reports hand-dropped %s missing:\n%s", name, before.Diff.Report())
			}
			for _, statement := range before.Diff.FixStatements() {
				if strings.Contains(statement, name) {
					t.Fatalf("the audit's fix would recreate %s: %s", name, statement)
				}
			}
		}

		ApplyDbMigrations(ctx)
		if version := DbVersion(ctx); version != MigrationCount() {
			t.Fatalf("migrated to %d, want the head %d", version, MigrationCount())
		}
		for _, name := range names {
			if exists, _ := indexDietTestIndexState(t, ctx, name); exists {
				t.Fatalf("%s exists after its drop migration", name)
			}
		}

		MaintenanceDb(ctx, func(conn PgConn) {
			for _, migration := range ordered {
				Raise(executeOnlineSqlMigrationOnConn(ctx, conn, indexDietMigrations[migration]))
			}
		}, OptReadWrite(), OptNoRetry())
		MaintenanceTx(ctx, func(tx PgTx) {
			for _, migration := range ordered {
				RaisePgResult(tx.Exec(ctx, indexDietMigrations[migration].auditSql))
			}
		})
		if after := AuditSchema(ctx); after.Diff.HasDifferences() {
			t.Fatalf("the head with every drop applied reports drift:\n%s", after.Diff.Report())
		}
	})
}

// A migrated database has none of the dropped indexes, and the maintenance
// rotation never names one.
func TestIndexDietIndexesAreAbsentAtHead(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, migration := range indexDietTestMigrations(t) {
			name := indexDietDroppedIndexes[migration]
			if exists, _ := indexDietTestIndexState(t, ctx, name); exists {
				t.Errorf("%s exists after every migration", name)
			}
			if slices.Contains(priorityReindexIndexes, name) {
				t.Errorf("the maintenance rotation still rebuilds %s", name)
			}
		}
	})
}

// An interrupted drop, or an interrupted rebuild, leaves an invalid index of
// the name. Each drop removes it, and a second run is a no-op.
func TestIndexDietDropsRemoveInvalidLeftovers(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE transfer_contract (contract_id uuid PRIMARY KEY, synthetic_key integer NOT NULL)`))
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE transfer_escrow_sweep (contract_id uuid PRIMARY KEY, synthetic_key integer NOT NULL)`))
			for _, table := range []string{"transfer_contract", "transfer_escrow_sweep"} {
				RaisePgResult(conn.Exec(ctx, `INSERT INTO `+table+` VALUES (gen_random_uuid(), 1), (gen_random_uuid(), 1)`))
			}
			for _, migration := range indexDietTestMigrations(t) {
				name := indexDietDroppedIndexes[migration]
				table := "transfer_contract"
				if strings.HasPrefix(name, "transfer_escrow_sweep_") {
					table = "transfer_escrow_sweep"
				}
				// a unique build over duplicate keys fails and leaves its index invalid
				if _, err := conn.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY `+name+` ON `+table+` (synthetic_key)`); err == nil {
					t.Fatalf("the %s leftover built valid", name)
				}
				if exists, usable := indexDietTestIndexState(t, ctx, name); !exists || usable {
					t.Fatalf("the %s leftover exists=%t usable=%t, want an invalid index", name, exists, usable)
				}
				for range 2 {
					Raise(executeOnlineSqlMigrationOnConn(ctx, conn, indexDietMigrations[migration]))
					if exists, _ := indexDietTestIndexState(t, ctx, name); exists {
						t.Fatalf("the %s leftover survived its drop", name)
					}
				}
			}
		}, OptReadWrite(), OptNoRetry())
	})
}
