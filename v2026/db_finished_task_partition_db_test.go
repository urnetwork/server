package server

import (
	"context"
	"testing"
	"time"
)

func testFinishedTaskPartitionLogf(t testing.TB) func(string, ...any) {
	return func(format string, args ...any) {
		t.Logf(format, args...)
	}
}

// A database audits clean through the finished_task cutover: while it is
// prepared, once it is partitioned, and once it is rolled back. With
// URNETWORK_TEST_FINISHED_TASK_PARTITIONS the database starts partitioned.
func TestAuditSchemaCleanThroughTheFinishedTaskCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		logf := testFinishedTaskPartitionLogf(t)
		if !IsFinishedTaskPartitioned(ctx) {
			cutoverDay := PartitionGranularityDaily.AddRanges(PartitionGranularityDaily.RangeStart(NowUtc()), 2)
			if err := MigrateFinishedTasksToPartitions(ctx, FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay, Logf: logf}); err != nil {
				t.Fatal(err)
			}
			if result := AuditSchema(ctx); result.Diff.HasDifferences() {
				t.Fatalf("a prepared cutover drifts:\n%s", result.Diff.Report())
			}
			if err := MigrateFinishedTasksToPartitions(ctx, FinishedTaskPartitionMigrateOptions{Abort: true, Logf: logf}); err != nil {
				t.Fatal(err)
			}
			Testing_PartitionFinishedTasks(ctx, TestingFinishedTaskCutoverDay(NowUtc()))
		}
		if result := AuditSchema(ctx); result.Diff.HasDifferences() {
			t.Fatalf("a partitioned database drifts:\n%s", result.Diff.Report())
		}
		if err := MigrateFinishedTasksToPartitions(ctx, FinishedTaskPartitionMigrateOptions{Rollback: true, Logf: logf}); err != nil {
			t.Fatal(err)
		}
		if result := AuditSchema(ctx); IsFinishedTaskPartitioned(ctx) || result.Diff.HasDifferences() {
			t.Fatalf("a rolled-back database drifts:\n%s", result.Diff.Report())
		}
	})
}

// The cutover's long steps run without transaction_timeout: inside the
// exemption the session and the transactions it begins have none, and the
// session's own value is restored after, so the pool never hands the
// exemption to later work.
func TestWithoutTransactionTimeoutExemptsItsSession(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		MaintenanceDb(ctx, func(conn PgConn) {
			var defaultTimeout string
			Raise(conn.QueryRow(ctx, `SHOW transaction_timeout`).Scan(&defaultTimeout))
			var inside, inTransaction string
			WithoutTransactionTimeout(ctx, conn, func() {
				Raise(conn.QueryRow(ctx, `SHOW transaction_timeout`).Scan(&inside))
				TxInConn(ctx, conn, func(tx PgTx) {
					Raise(tx.QueryRow(ctx, `SHOW transaction_timeout`).Scan(&inTransaction))
				}, TxReadCommitted, OptNoRetry())
			})
			var after string
			Raise(conn.QueryRow(ctx, `SHOW transaction_timeout`).Scan(&after))
			if inside != "0" || inTransaction != "0" || after != defaultTimeout {
				t.Fatalf("transaction_timeout %s inside, %s in a transaction, %s after; %s before", inside, inTransaction, after, defaultTimeout)
			}
		})
	})
}
