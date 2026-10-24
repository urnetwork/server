// Online index recovery on partitioned contract tables (CONTRACT-PARTITION.md):
// migration 810's completion evidence after the cutover.
package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The relation kind, validity and definition of an index; kind "" when it does
// not exist.
func testContractIndexState(t testing.TB, ctx context.Context, indexName string) (relkind string, valid bool, definition string) {
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT index_class.relkind::text, index_state.indisvalid, pg_get_indexdef(index_state.indexrelid)
			FROM pg_index index_state
			INNER JOIN pg_class index_class ON index_class.oid = index_state.indexrelid
			WHERE index_state.indexrelid = to_regclass($1)
			`,
			"public."+indexName,
		)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&relkind, &valid, &definition))
			}
		})
	})
	return
}

func testExecuteOnlineMigration(ctx context.Context, migration *OnlineSqlMigration) (err error) {
	MaintenanceDb(ctx, func(conn PgConn) {
		err = executeOnlineSqlMigrationOnConn(ctx, conn, migration)
	}, OptReadWrite(), OptNoRetry())
	return
}

// Migration 810's recovery inspection recognizes its published index in
// either layout. After the cutover pg_get_indexdef renders the partitioned
// index ON ONLY the parent, which the exact comparison read as incomplete;
// replaying 810's online body then ran DDL a partitioned parent refuses.
// Builders and maintenance on a partition still count as busy, and a
// different definition or an invalid shell is still incomplete.
func TestOnlineIndexCompletionRecognizesPartitionedIndex(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		migration := migrations[809].(*OnlineSqlMigration)
		completion := migration.completion
		connect.AssertEqual(t, completion.version, int32(810))
		connect.AssertEqual(t, completion.table, "transfer_contract")
		inspect := func(completion *onlineIndexMigrationCompletion) (complete bool, busy bool) {
			MaintenanceDb(ctx, func(conn PgConn) {
				var err error
				complete, busy, err = completion.inspect(ctx, conn)
				Raise(err)
			})
			return
		}

		if !IsContractPartitioned(ctx) {
			complete, busy := inspect(completion)
			connect.AssertEqual(t, complete, true)
			connect.AssertEqual(t, busy, false)
			cutoverStart := time.Now()
			Testing_PartitionContracts(ctx)
			t.Logf("cut over in %s", time.Since(cutoverStart))
		}
		relkind, valid, actual := testContractIndexState(t, ctx, completion.index)
		connect.AssertEqual(t, relkind, "I")
		connect.AssertEqual(t, valid, true)
		connect.AssertEqual(t, strings.Contains(actual, " ON ONLY public.transfer_contract "), true)

		complete, busy := inspect(completion)
		connect.AssertEqual(t, complete, true)
		connect.AssertEqual(t, busy, false)
		// replaying the online body is recognized as done before any DDL runs
		before := testDbMaintenanceIndexOid(ctx, completion.index)
		connect.AssertEqual(t, testExecuteOnlineMigration(ctx, migration), nil)
		connect.AssertEqual(t, testDbMaintenanceIndexOid(ctx, completion.index), before)

		reordered := *completion
		reordered.definition = strings.Replace(completion.definition, "(close_time, contract_id)", "(contract_id, close_time)", 1)
		complete, busy = inspect(&reordered)
		connect.AssertEqual(t, complete, false)
		connect.AssertEqual(t, busy, false)

		// maintenance on a weekly partition is a busy owner
		weekly := ContractPartitionName("transfer_contract", ContractPartitionWeekStart(NowUtc()))
		lockStart := time.Now()
		holder, release := testDbMaintenanceHoldVacuumLock(t, ctx, weekly)
		connect.AssertEqual(t, holder(weekly), true)
		_, busy = inspect(&reordered)
		release()
		connect.AssertEqual(t, busy, true)
		t.Logf("checked the busy partition in %s", time.Since(lockStart))

		// a shell that is not valid on every partition is incomplete
		dropStart := time.Now()
		defer func() {
			t.Logf("replaced the index with a shell in %s", time.Since(dropStart))
		}()
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, "DROP INDEX "+completion.index))
			RaisePgResult(conn.Exec(ctx, "CREATE INDEX "+completion.index+" ON ONLY transfer_contract (close_time, contract_id) WHERE outcome IS NULL AND close_time IS NOT NULL"))
		})
		complete, _ = inspect(completion)
		connect.AssertEqual(t, complete, false)
	})
}
