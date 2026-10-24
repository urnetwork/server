// The write-path migrations of PARTITION-MAINTENANCE-PLAN.md Part B, read
// back from the migrated schema.
package server

import (
	"context"
	"testing"
	"time"
)

// The legacy auth attempt table is gone after every migration: rate limiting
// counts attempts in Redis and nothing reads or writes the table.
func TestUserAuthAttemptTableIsDropped(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var exists bool
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.user_auth_attempt') IS NOT NULL`).Scan(&exists))
		}, OptReadOnly())
		if exists {
			t.Fatal("user_auth_attempt exists after every migration")
		}
	})
}

// The current transaction's row updates of a table, and how many were HOT
// (heap-only: no index entry written). Pending transaction statistics are
// exact for the transaction that reads them, unlike the shared counters.
func writePathTestTransactionUpdates(t testing.TB, ctx context.Context, tx PgTx, table string) (updated int64, hot int64) {
	t.Helper()
	Raise(tx.QueryRow(
		ctx,
		`SELECT pg_stat_get_xact_tuples_updated(to_regclass($1)), pg_stat_get_xact_tuples_hot_updated(to_regclass($1))`,
		"public."+table,
	).Scan(&updated, &hot))
	return
}

// The heartbeat rewrites only heartbeat_time. Without the heartbeat_time
// index it is a HOT update, so a heartbeat every 5 s adds no index entry.
func TestNetworkClientHandlerHeartbeatIsHot(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		handlerId := NewId()
		heartbeatTime := NowUtc().Truncate(time.Second)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client_handler (handler_id, heartbeat_time, handler_host) VALUES ($1, $2, 'handler.example')`,
				handlerId,
				heartbeatTime.Add(-5*time.Second),
			))
		})
		Tx(ctx, func(tx PgTx) {
			// the statement of model.HeartbeatNetworkClientHandler
			tag := RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client_handler SET heartbeat_time = $2 WHERE handler_id = $1`,
				handlerId,
				heartbeatTime,
			))
			updated, hot := writePathTestTransactionUpdates(t, ctx, tx, "network_client_handler")
			if tag.RowsAffected() != 1 || updated != 1 || hot != 1 {
				t.Fatalf("heartbeat rows=%d updated=%d hot=%d, want one HOT update", tag.RowsAffected(), updated, hot)
			}
		})
	})
}
