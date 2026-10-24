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

// The fillfactor of each write-path table, the free space its pages keep so an
// update of unindexed columns stays on its page (HOT).
func TestWritePathTableFillfactors(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, expected := range []struct {
			table      string
			fillfactor string
		}{
			{table: "transfer_balance", fillfactor: "85"},
			{table: "device", fillfactor: "90"},
			{table: "search_provider_stats", fillfactor: "70"},
			{table: "transfer_debit_journal", fillfactor: "70"},
			{table: "client_connection_reliability_score", fillfactor: "50"},
			{table: "network_client_location_reliability", fillfactor: "85"},
		} {
			var fillfactor *string
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(
					ctx,
					`SELECT (SELECT option_value FROM pg_options_to_table(relation.reloptions) WHERE option_name = 'fillfactor')
					FROM pg_class relation WHERE relation.oid = to_regclass('public.'||$1)`,
					expected.table,
				).Scan(&fillfactor))
			}, OptReadOnly())
			if fillfactor == nil || *fillfactor != expected.fillfactor {
				t.Errorf("%s fillfactor=%v, want %s", expected.table, fillfactor, expected.fillfactor)
			}
		}
	})
}

// Marking a debit applied changes only the applied flag. No index keys it once
// the age index is (shard, create_time), so the update is HOT.
func TestTransferDebitJournalAppliedUpdateIsHot(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		balanceId := NewId()
		contractId := NewId()
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO transfer_debit_journal (contract_id, balance_id, debit_byte_count, shard) VALUES ($1, $2, 1024, $3)`,
				contractId,
				balanceId,
				int(balanceId[15])%16,
			))
		})
		Tx(ctx, func(tx PgTx) {
			// the statement of the debit flush (model/subscription_transfer_debit.go)
			tag := RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_debit_journal SET applied=true WHERE balance_id=$1 AND contract_id=ANY($2) AND NOT applied`,
				balanceId,
				[]Id{contractId},
			))
			updated, hot := writePathTestTransactionUpdates(t, ctx, tx, "transfer_debit_journal")
			if tag.RowsAffected() != 1 || updated != 1 || hot != 1 {
				t.Fatalf("applied rows=%d updated=%d hot=%d, want one HOT update", tag.RowsAffected(), updated, hot)
			}
		})
	})
}

// A reliability pass rewrites each client score with its new window. No index
// keys the window or the scores any more, so the rewrite is HOT.
func TestClientReliabilityScoreRewriteIsHot(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := NewId()
		upsert := func(tx PgTx, maxBlockNumber int64) {
			// the shape of the pass's upsert (UpdateClientReliabilityScores)
			RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO client_connection_reliability_score (
					client_id, lookback_index, independent_reliability_score, independent_reliability_weight,
					reliability_score, reliability_weight, min_block_number, max_block_number
				) VALUES ($1, 1, 60, 1, 60, 1, $2, $3)
				ON CONFLICT (client_id, lookback_index) DO UPDATE SET
					min_block_number = EXCLUDED.min_block_number,
					max_block_number = EXCLUDED.max_block_number`,
				clientId,
				maxBlockNumber-60,
				maxBlockNumber,
			))
		}
		Tx(ctx, func(tx PgTx) {
			upsert(tx, 1000)
		})
		Tx(ctx, func(tx PgTx) {
			upsert(tx, 1030)
			updated, hot := writePathTestTransactionUpdates(t, ctx, tx, "client_connection_reliability_score")
			if updated != 1 || hot != 1 {
				t.Fatalf("score rewrite updated=%d hot=%d, want one HOT update", updated, hot)
			}
		})
	})
}
