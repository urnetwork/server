package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// While ADD COLUMN waits for its ACCESS EXCLUSIVE lock, new readers of
// network_extender queue behind it, so the lock timeout bounds how long a busy
// table can hold up the extender's readers.
func TestExtenderCamouflageKeyMigrationBoundsItsLockWait(t *testing.T) {
	if !strings.Contains(extenderCamouflageKeyMigration.sql, "SET LOCAL lock_timeout = '2s';") {
		t.Fatalf("the migration does not bound its lock wait at 2 s: %s", extenderCamouflageKeyMigration.sql)
	}
	identity, err := migrationIdentity(extenderCamouflageKeyMigration)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"migration identity %s, version %d of %d",
		identity,
		ExtenderCamouflageKeyMigrationVersion(),
		MigrationCount(),
	)
}

// A reader holding network_extender makes the migration give up with a lock
// timeout rather than wait, and nothing of it is kept: the version stays where it
// was and the columns are absent. Migrating again once the reader has gone applies
// it, which is the recovery, since the runner has no retry of its own.
func TestExtenderCamouflageKeyMigrationGivesUpAndAppliesOnARerun(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		version := ExtenderCamouflageKeyMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)

		columnCount := func() int {
			var count int
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(
					ctx,
					`
					SELECT COUNT(*)
					FROM information_schema.columns
					WHERE
						table_name = 'network_extender' AND
						column_name IN ('reality_public_key', 'reality_public_key_network_id')
					`,
				).Scan(&count))
			})
			return count
		}
		// the migration's panic, nil when it applied. The deadline fails a
		// migration with no lock timeout, which would otherwise wait on the
		// reader below for as long as the test runs.
		migrate := func() (raised any) {
			defer func() {
				raised = recover()
			}()
			migrateCtx, migrateCancel := context.WithTimeout(ctx, 2*time.Minute)
			defer migrateCancel()
			ApplyDbMigrationsUpTo(migrateCtx, version)
			return nil
		}

		MaintenanceTx(ctx, func(tx PgTx) {
			// held until this transaction ends
			RaisePgResult(tx.Exec(ctx, `LOCK TABLE network_extender IN ACCESS SHARE MODE`))
			raised := migrate()
			err, ok := raised.(error)
			if !ok || !IsLockTimeout(err) {
				t.Fatalf("with a reader holding the table the migration raised %v, expected a lock timeout", raised)
			}
		})
		if got := DbVersion(ctx); got != version-1 {
			t.Fatalf("the version after the lock timeout is %d, expected %d", got, version-1)
		}
		if count := columnCount(); count != 0 {
			t.Fatalf("%d columns survived the lock timeout", count)
		}

		if raised := migrate(); raised != nil {
			t.Fatalf("the rerun raised %v", raised)
		}
		if got := DbVersion(ctx); got != version {
			t.Fatalf("the version after the rerun is %d, expected %d", got, version)
		}
		if count := columnCount(); count != 2 {
			t.Fatalf("the rerun left %d columns, expected 2", count)
		}
	})
}
