// The recurring debit publishers use one bounded startup owner and exact
// per-turn completion declarations. Other task families keep their own policy.
package work

import (
	"context"
	"fmt"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type transferDebitTaskTarget struct{ task.Target }

func (self *transferDebitTaskTarget) TaskCompletionOwnershipKeys(_ *task.Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

// The commit post keeps the partition's idle mark (transfer_debit_work.go).
func NewTransferDebitTaskTarget() task.Target {
	return &transferDebitTaskTarget{Target: task.NewTaskTargetWithCommitPost(FlushTransferDebits, FlushTransferDebitsCommitPost)}
}

// Independent debit publication owns only the keys it can write.
func transferDebitStartupOwnershipKeys() []server.PgOwnershipKey {
	keys := make([]server.PgOwnershipKey, 0, model.TransferDebitShardCount)
	for shard := range model.TransferDebitShardCount {
		keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce(model.TransferDebitFlushRunOnceKey(shard))))
	}
	return keys
}

func requireSettlementStartupOwnershipInTx(ctx context.Context, tx server.PgTx, keys []server.PgOwnershipKey) {
	admitted, err := server.TryTxOwnership(ctx, tx, keys)
	server.Raise(err)
	if !admitted {
		server.Raise(fmt.Errorf("settlement startup publication ownership is busy"))
	}
}

// Production initializers keep this transaction separate from unrelated
// schedules, admitting the complete queue set before any business statement.
// The publisher's own check then validates the same set without extending
// ownership.
func ScheduleSettlementAccountingTasks(ctx context.Context) {
	server.OwnedTx(ctx, transferDebitStartupOwnershipKeys(), func(tx server.PgTx) {
		owner := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer owner.Cancel()
		ScheduleFlushTransferDebits(owner, tx)
	}, server.TxReadCommitted, server.OptNoRetry())
}
