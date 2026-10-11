package server

// The fillfactor migrations (820, 821) on the two contract tables: published
// positions, lock behavior on a live table, and the HOT updates they enable.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
)

// Published versions and the table each one sets, in migration order.
var contractFillfactorMigrations = []struct {
	version int
	table   string
}{
	{version: 820, table: "transfer_contract"},
	{version: 821, table: "contract_close"},
}

// Whether the table's own storage parameters carry fillfactor=90.
func contractTableHasFillfactor(ctx context.Context, table string) bool {
	var present bool
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `
			SELECT coalesce(reloptions, ARRAY[]::text[]) @> ARRAY['fillfactor=90']
			FROM pg_class
			WHERE oid = to_regclass('public.'||$1)
		`, table).Scan(&present))
	}, OptReadOnly())
	return present
}

// What one hot-path update did to a row on a page that inserts had filled.
type contractPageUpdate struct {
	samePage bool
	hot      bool
}

// Fills the first heap page of both empty contract tables with fresh rows, as
// far as the table's fillfactor lets inserts fill a page, then applies the hot
// path's update to the last row on that page: the usage_unverified update of an
// open contract, and the close upsert of a checkpoint row. Neither changes an
// indexed column, so each is HOT exactly when its new version fits on the page.
func updateRowsOnFilledContractPages(t testing.TB, ctx context.Context) map[string]contractPageUpdate {
	t.Helper()
	tablePageUpdates := map[string]contractPageUpdate{}
	var tablesEmpty bool
	var pagesFilled bool
	MaintenanceTx(ctx, func(tx PgTx) {
		Raise(tx.QueryRow(ctx, `
			SELECT pg_relation_size('transfer_contract') = 0 AND pg_relation_size('contract_close') = 0
		`).Scan(&tablesEmpty))
		RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count)
			SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 1024
			FROM generate_series(1, 1000)
		`))
		RaisePgResult(tx.Exec(ctx, `
			INSERT INTO contract_close (contract_id, party, used_transfer_byte_count, checkpoint)
			SELECT gen_random_uuid(), 'source', 1024, true
			FROM generate_series(1, 1000)
		`))
		// rows past the first page mean the inserts left it no further room
		Raise(tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM transfer_contract WHERE ctid >= '(1,0)'::tid)
				AND EXISTS (SELECT 1 FROM contract_close WHERE ctid >= '(1,0)'::tid)
		`).Scan(&pagesFilled))

		for _, c := range []struct {
			table     string
			updateSql string
		}{
			{
				table:     "transfer_contract",
				updateSql: `UPDATE transfer_contract SET usage_unverified=true WHERE contract_id=$1 AND outcome IS NULL RETURNING (ctid::text::point)[0]::bigint`,
			},
			{
				table: "contract_close",
				updateSql: `INSERT INTO contract_close (contract_id, party, used_transfer_byte_count, close_time, checkpoint)
					VALUES ($1, 'source', 1024, now(), true)
					ON CONFLICT (contract_id, party) DO UPDATE SET
						used_transfer_byte_count = contract_close.used_transfer_byte_count + EXCLUDED.used_transfer_byte_count,
						close_time = EXCLUDED.close_time, checkpoint = EXCLUDED.checkpoint
					WHERE contract_close.checkpoint
					RETURNING (ctid::text::point)[0]::bigint`,
			},
		} {
			var contractId Id
			Raise(tx.QueryRow(ctx, fmt.Sprintf(
				`SELECT contract_id FROM %s WHERE ctid < '(1,0)'::tid ORDER BY ctid DESC LIMIT 1`, c.table,
			)).Scan(&contractId))
			hotUpdateCount := func() int64 {
				var count int64
				Raise(tx.QueryRow(ctx, `
					SELECT n_tup_hot_upd FROM pg_stat_xact_user_tables WHERE relid = to_regclass('public.'||$1)
				`, c.table).Scan(&count))
				return count
			}
			hotCountBefore := hotUpdateCount()
			var block int64
			Raise(tx.QueryRow(ctx, c.updateSql, contractId).Scan(&block))
			tablePageUpdates[c.table] = contractPageUpdate{
				samePage: block == 0,
				hot:      hotUpdateCount() == hotCountBefore+1,
			}
		}
	})
	if !tablesEmpty {
		t.Fatal("contract tables are not empty before the page is filled")
	}
	if !pagesFilled {
		t.Fatal("inserts did not fill the first page of both contract tables")
	}
	return tablePageUpdates
}

// Each version is one transaction that bounds its lock wait and sets one storage
// parameter. The numbers are published: a database records 820 and 821 by
// position, and the migration signal reads each table's fillfactor by version.
func TestContractFillfactorMigrationsArePublishedAt820And821(t *testing.T) {
	for _, c := range contractFillfactorMigrations {
		migration, ok := migrations[c.version-1].(*SqlMigration)
		if !ok {
			t.Fatalf("migration %d is %T, want *SqlMigration", c.version, migrations[c.version-1])
		}
		want := fmt.Sprintf("SET LOCAL lock_timeout = '5s'; ALTER TABLE %s SET (fillfactor = 90);", c.table)
		if sql := strings.Join(strings.Fields(migration.sql), " "); sql != want {
			t.Fatalf("migration %d sql = %q, want %q", c.version, sql, want)
		}
	}
}

// A conflicting lock that the server does not cancel for the ALTER (an
// anti-wraparound vacuum, REINDEX CONCURRENTLY, other DDL) makes each migration
// give up at its lock timeout: it rolls back, records no version and leaves the
// table unchanged. The rerun then applies while readers, writers and pg_dump
// hold their locks, which SHARE UPDATE EXCLUSIVE does not conflict with, and
// neither heap is rewritten.
func TestContractFillfactorMigrationsWaitOnlyForConflictingLocks(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, contractFillfactorMigrations[0].version-1)

		// Holds the locks lockSql takes in a transaction on another connection
		// until the returned release is called. The hold ends after 30s, so a
		// migration that waits for it fails the test instead of hanging it.
		holdLocks := func(lockSql string) func() {
			locked := make(chan struct{})
			release := make(chan struct{})
			released := make(chan struct{})
			go func() {
				defer close(released)
				MaintenanceTx(ctx, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, lockSql))
					close(locked)
					select {
					case <-release:
					case <-time.After(30 * time.Second):
					}
				}, OptNoRetry())
			}()
			<-locked
			return func() {
				close(release)
				<-released
			}
		}
		migrate := func(version int) (migrateErr any, elapsed time.Duration) {
			startTime := time.Now()
			func() {
				defer func() {
					migrateErr = recover()
				}()
				ApplyDbMigrationsUpTo(ctx, version)
			}()
			return migrateErr, time.Since(startTime)
		}
		heapFileNodes := func() map[string]int64 {
			tableFileNodes := map[string]int64{}
			MaintenanceDb(ctx, func(conn PgConn) {
				for _, c := range contractFillfactorMigrations {
					var fileNode int64
					Raise(conn.QueryRow(ctx, `SELECT pg_relation_filenode(to_regclass('public.'||$1))::bigint`, c.table).Scan(&fileNode))
					tableFileNodes[c.table] = fileNode
				}
			}, OptReadOnly())
			return tableFileNodes
		}

		fileNodesBefore := heapFileNodes()
		for _, c := range contractFillfactorMigrations {
			release := holdLocks(`LOCK TABLE ` + c.table + ` IN SHARE UPDATE EXCLUSIVE MODE`)
			migrateErr, elapsed := migrate(c.version)
			release()
			var pgErr *pgconn.PgError
			err, _ := migrateErr.(error)
			if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.LockNotAvailable {
				t.Fatalf("migration %d behind the lock returned %v after %s, want a lock timeout", c.version, migrateErr, elapsed)
			}
			if 20*time.Second < elapsed {
				t.Fatalf("migration %d waited %s behind the lock", c.version, elapsed)
			}
			if version := DbVersion(ctx); version != c.version-1 {
				t.Fatalf("db version = %d after migration %d gave up, want %d", version, c.version, c.version-1)
			}
			if contractTableHasFillfactor(ctx, c.table) {
				t.Fatalf("%s fillfactor changed although migration %d gave up", c.table, c.version)
			}

			release = holdLocks(`
				LOCK TABLE transfer_contract, contract_close IN ACCESS SHARE MODE;
				LOCK TABLE transfer_contract, contract_close IN ROW EXCLUSIVE MODE;
			`)
			migrateErr, elapsed = migrate(c.version)
			release()
			if migrateErr != nil {
				t.Fatalf("migration %d rerun beside readers and writers failed after %s: %v", c.version, elapsed, migrateErr)
			}
			if version := DbVersion(ctx); version != c.version {
				t.Fatalf("db version = %d after the rerun of migration %d", version, c.version)
			}
			if !contractTableHasFillfactor(ctx, c.table) {
				t.Fatalf("%s lacks fillfactor=90 after migration %d", c.table, c.version)
			}
		}
		fileNodesAfter := heapFileNodes()
		for table, fileNode := range fileNodesBefore {
			if fileNodesAfter[table] != fileNode {
				t.Fatalf("%s heap was rewritten: file node %d became %d", table, fileNode, fileNodesAfter[table])
			}
		}
	})
}

// Before 820 and 821, inserts fill a page completely. An update of a row on it
// has no room, so its new version moves to another page and every index of the
// table gains an entry, even though no indexed column changed.
func TestContractUpdatesLeaveAFullPageBeforeFillfactor(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, contractFillfactorMigrations[0].version-1)

		for table, pageUpdate := range updateRowsOnFilledContractPages(t, ctx) {
			if pageUpdate.samePage || pageUpdate.hot {
				t.Fatalf("%s update on a full page stayed on it (hot=%t)", table, pageUpdate.hot)
			}
		}
	})
}

// After 820 and 821, inserts leave a tenth of each page free, so the update of
// a row on a page they filled stays on that page as a HOT update.
func TestContractUpdatesStayOnTheirPageAfterFillfactor(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, c := range contractFillfactorMigrations {
			if !contractTableHasFillfactor(ctx, c.table) {
				t.Fatalf("%s lacks fillfactor=90 after all migrations", c.table)
			}
		}

		tablePageUpdates := updateRowsOnFilledContractPages(t, ctx)
		for _, c := range contractFillfactorMigrations {
			if pageUpdate := tablePageUpdates[c.table]; !pageUpdate.samePage || !pageUpdate.hot {
				t.Fatalf("%s update left its page or was not HOT: same page=%t hot=%t", c.table, pageUpdate.samePage, pageUpdate.hot)
			}
		}
	})
}
