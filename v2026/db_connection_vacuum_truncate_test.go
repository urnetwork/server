// The connection tables reuse their emptied tail pages instead of letting
// VACUUM truncate them under AccessExclusiveLock.
package server

import (
	"context"
	"slices"
	"testing"
	"time"
)

// Grows network_client_connection with disconnected rows, which no journal
// trigger records, empties its trailing pages and runs VACUUM. Reports whether
// VACUUM truncated the heap: truncation takes AccessExclusiveLock, which on
// Main queued 361 connection statements behind autovacuum for up to 4 s
// (2026-10-12 18:33Z) while it dropped the cut pages from shared_buffers.
func vacuumTruncatesConnectionTail(t testing.TB, ctx context.Context) bool {
	t.Helper()
	const tailPageCount = 64
	pageCount := func() int64 {
		var count int64
		Db(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT pg_relation_size('network_client_connection') / current_setting('block_size')::bigint`).Scan(&count))
		})
		return count
	}
	startPageCount := pageCount()
	for pageCount() < startPageCount+2*tailPageCount {
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_connection
			 (client_id,connection_id,connect_time,connection_host,connection_service,connection_block,
			  client_address_hash,client_address_port,handler_id,connected,disconnect_time)
			 SELECT gen_random_uuid(),gen_random_uuid(),$1,'synthetic.example','test','test',$2,10001,gen_random_uuid(),false,$1
			 FROM generate_series(1,1000)`, NowUtc(), make([]byte, 32)))
		})
	}
	grownPageCount := pageCount()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_connection WHERE (ctid::text::point)[0] >= $1`, grownPageCount-tailPageCount))
	})
	Db(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, `VACUUM network_client_connection`))
	})
	return pageCount() < grownPageCount
}

// Before the migration the same VACUUM truncates the emptied tail, which
// proves the fixture reaches truncation; after it the heap keeps its pages.
func TestConnectionTablesVacuumWithoutTruncation(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		ApplyDbMigrationsUpTo(ctx, sqlMigrationIndex(t, "ALTER TABLE network_client_connection SET (vacuum_truncate = false)"))
		if !vacuumTruncatesConnectionTail(t, ctx) {
			t.Fatal("control: VACUUM kept an empty tail before the migration, so this fixture cannot observe truncation")
		}
		ApplyDbMigrations(ctx)
		if vacuumTruncatesConnectionTail(t, ctx) {
			t.Fatal("VACUUM truncated network_client_connection, which takes AccessExclusiveLock on every connection statement's table")
		}
		for _, table := range []string{"network_client_connection", "network_client_handler"} {
			var options []string
			Db(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT coalesce(reloptions, '{}') FROM pg_class WHERE oid = to_regclass($1)`, table).Scan(&options))
			})
			if !slices.Contains(options, "vacuum_truncate=false") {
				t.Errorf("%s reloptions = %v, want vacuum_truncate=false", table, options)
			}
		}
	})
}
