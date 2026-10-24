// A debit flush chain backs off while its partition's journal is empty
// (taskworker/work/transfer_debit_work.go). Each settlement that journals a
// debit wakes the chains of the partitions it wrote once its commit returns,
// through the idle marks of task.WakeIdleRunOnce: an active chain costs the
// settlement one Redis round trip and no queue write.
package model

import (
	"context"
	"fmt"
	"slices"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The raw run-once key of a partition's flush chain, the form KickTasks and
// the idle marks take. The stored key is its RunOnce JSON encoding.
func TransferDebitFlushRunOnceKey(shard int) string {
	return fmt.Sprintf("flush_transfer_debits_%d", shard)
}

// Whether the partition holds any journal row, pending or applied and awaiting
// its Redis release: exactly the rows the flush chain visits. One seek of
// transfer_debit_journal_shard.
func TransferDebitShardPending(ctx context.Context, shard int) (pending bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE shard=$1)`, shard).Scan(&pending))
	}, server.OptNoRetry())
	return
}

// Kicks the backed-off flush chain of each partition the balances' journal
// rows belong to. Call it after those rows committed. Returns the run-once
// keys kicked; a chain that is not backed off is left alone.
func WakeTransferDebitFlushes(ctx context.Context, balanceIds []server.Id) (kickedKeys []string, err error) {
	keys := make([]string, 0, len(balanceIds))
	for _, balanceId := range balanceIds {
		keys = append(keys, TransferDebitFlushRunOnceKey(transferDebitShard(balanceId)))
	}
	return task.WakeIdleRunOnce(ctx, task.DefaultIdleWakeSettings(), keys...)
}

// The settlement post form. The wake belongs to the committed journal rows,
// not to the request that wrote them, so it outlives the caller's
// cancellation within the wake's own timeout.
func transferDebitWakePost(ctx context.Context, balanceIds []server.Id) server.PostFunction {
	wakeCtx := context.WithoutCancel(ctx)
	balanceIds = slices.Clone(balanceIds)
	return func() any {
		// Failures are counted by the wake; the chain then runs at its idle delay.
		_, _ = WakeTransferDebitFlushes(wakeCtx, balanceIds)
		return nil
	}
}
