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

// Each write-path migration's version is its position in the migration list,
// unique, and in the order the write-path migrations were written: a
// replacement index is built before the migration that drops what it
// replaces.
func TestWritePathMigrationVersionsFollowTheirPosition(t *testing.T) {
	previousVersion := 0
	for migration := WritePathUserAuthAttemptDrop; ; migration++ {
		target, ok := writePathMigrations[migration]
		if !ok {
			if int(migration) != len(writePathMigrations) {
				t.Fatalf("write-path migrations are not numbered contiguously: %d of %d", migration, len(writePathMigrations))
			}
			break
		}
		version := WritePathMigrationVersion(migration)
		if version < 1 || len(migrations) < version || migrations[version-1] != target {
			t.Fatalf("write-path migration %d version %d is not its position", migration, version)
		}
		if version <= previousVersion {
			t.Fatalf("write-path migration %d version %d is not after %d", migration, version, previousVersion)
		}
		previousVersion = version
	}
}

// The partial indexes of network_client_connection, by the migration that
// builds each, with the definition PostgreSQL reports for it.
var writePathTestConnectionIndexes = []struct {
	migration  WritePathMigration
	name       string
	definition string
	predicate  string
}{
	{
		migration:  WritePathConnectionOpenClientIndex,
		name:       "network_client_connection_connected_client_id_extender_id",
		definition: "CREATE INDEX network_client_connection_connected_client_id_extender_id ON public.network_client_connection USING btree (client_id, extender_id) WHERE connected",
		predicate:  "connected",
	},
	{
		migration:  WritePathConnectionOpenAddressIndex,
		name:       "network_client_connection_connected_client_address_hash",
		definition: "CREATE INDEX network_client_connection_connected_client_address_hash ON public.network_client_connection USING btree (client_address_hash) WHERE connected",
		predicate:  "connected",
	},
	{
		migration:  WritePathConnectionClosedDisconnectIndex,
		name:       "network_client_connection_closed_disconnect_time",
		definition: "CREATE INDEX network_client_connection_closed_disconnect_time ON public.network_client_connection USING btree (disconnect_time) WHERE (NOT connected)",
		predicate:  "(NOT connected)",
	},
}

// The exact usable definition and predicate of an index, or empty strings when
// it does not exist.
func writePathTestIndexShape(t testing.TB, ctx context.Context, conn PgConn, name string) (definition string, predicate string, valid bool) {
	t.Helper()
	result, err := conn.Query(ctx, `SELECT pg_get_indexdef(indexrelid), COALESCE(pg_get_expr(indpred, indrelid), ''), indisvalid AND indisready AND indislive
		FROM pg_index WHERE indexrelid = to_regclass('public.'||$1)`, name)
	WithPgResult(result, err, func() {
		if result.Next() {
			Raise(result.Scan(&definition, &predicate, &valid))
		}
	})
	return
}

// After every migration the connection table carries the three partial
// indexes, each exactly as published, and the full ones are gone.
func TestNetworkClientConnectionPartialIndexes(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		MaintenanceDb(ctx, func(conn PgConn) {
			for _, index := range writePathTestConnectionIndexes {
				definition, predicate, valid := writePathTestIndexShape(t, ctx, conn, index.name)
				if definition != index.definition || predicate != index.predicate || !valid {
					t.Errorf("%s = %q predicate %q valid=%t, want %q", index.name, definition, predicate, valid, index.definition)
				}
			}
		}, OptReadOnly())
	})
}

// Each partial index migration recovers from an interrupted or look-alike
// build: its recovery step drops the leftover and the build makes the exact
// index, again on replay, and its audit form makes the same index.
func TestNetworkClientConnectionPartialIndexMigrationsRecover(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `CREATE TABLE network_client_connection (
				connection_id uuid PRIMARY KEY, client_id uuid NOT NULL, handler_id uuid NULL, extender_id uuid NULL,
				client_address_hash bytea NULL, connected boolean NOT NULL DEFAULT true, disconnect_time timestamp NULL)`))
			for _, index := range writePathTestConnectionIndexes {
				migration, ok := writePathMigrations[index.migration].(*OnlineSqlMigration)
				if !ok || len(migration.productionSqlSteps()) != 2 {
					t.Fatalf("%s needs separate recovery and online-create steps", index.name)
				}
				// same-name residue over every row, as an interrupted or
				// hand-made build could leave
				RaisePgResult(conn.Exec(ctx, `CREATE INDEX `+index.name+` ON network_client_connection (connection_id)`))
				Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
				definition, predicate, valid := writePathTestIndexShape(t, ctx, conn, index.name)
				if definition != index.definition || predicate != index.predicate || !valid {
					t.Fatalf("%s after recovery = %q predicate %q valid=%t", index.name, definition, predicate, valid)
				}
				Raise(executeOnlineSqlMigrationOnConn(ctx, conn, migration))
				definition, predicate, valid = writePathTestIndexShape(t, ctx, conn, index.name)
				if definition != index.definition || predicate != index.predicate || !valid {
					t.Fatalf("%s after replay = %q predicate %q valid=%t", index.name, definition, predicate, valid)
				}
			}
		}, OptReadWrite(), OptNoRetry())
		MaintenanceTx(ctx, func(tx PgTx) {
			for _, index := range writePathTestConnectionIndexes {
				migration := writePathMigrations[index.migration].(*OnlineSqlMigration)
				RaisePgResult(tx.Exec(ctx, migration.auditSql))
			}
		})
		MaintenanceDb(ctx, func(conn PgConn) {
			for _, index := range writePathTestConnectionIndexes {
				definition, predicate, valid := writePathTestIndexShape(t, ctx, conn, index.name)
				if definition != index.definition || predicate != index.predicate || !valid {
					t.Fatalf("%s after the audit form = %q predicate %q valid=%t", index.name, definition, predicate, valid)
				}
			}
		}, OptReadOnly(), OptNoRetry())
	})
}
