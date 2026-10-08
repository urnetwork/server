// The recurring close-path publishers use one bounded startup owner and exact
// per-turn completion declarations. Other task families keep their own policy.
package work

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type legacySettlementDispatcherTaskTarget struct{ task.Target }

func (self *legacySettlementDispatcherTaskTarget) TaskCompletionOwnershipKeys(_ *task.Task, resultJson string) ([]server.PgOwnershipKey, error) {
	var result FlushLegacySettlementsResult
	if err := json.Unmarshal([]byte(resultJson), &result); err != nil {
		return nil, err
	}
	if result.Dispatch == nil {
		return nil, nil
	}
	return model.LegacyPayerSettlementQueueOwnershipKeys(result.Dispatch.PayerNetworkIds)
}

func NewLegacySettlementDispatcherTaskTarget() task.Target {
	return &legacySettlementDispatcherTaskTarget{Target: task.NewTaskTargetWithPost(FlushLegacySettlements, FlushLegacySettlementsPost)}
}

type transferDebitTaskTarget struct{ task.Target }

func (self *transferDebitTaskTarget) TaskCompletionOwnershipKeys(_ *task.Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

func NewTransferDebitTaskTarget() task.Target {
	return &transferDebitTaskTarget{Target: task.NewTaskTargetWithPost(FlushTransferDebits, FlushTransferDebitsPost)}
}

// Independent debit publication owns only the keys it can write.
func transferDebitStartupOwnershipKeys() []server.PgOwnershipKey {
	keys := make([]server.PgOwnershipKey, 0, model.TransferDebitShardCount)
	for shard := range model.TransferDebitShardCount {
		keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce(fmt.Sprintf("flush_transfer_debits_%d", shard))))
	}
	return keys
}

// Independent legacy publication must not need a live debit owner's queue key.
func legacySettlementStartupOwnershipKeys() []server.PgOwnershipKey {
	keys := make([]server.PgOwnershipKey, 0, model.LegacySettlementShardCount)
	for shard := range model.LegacySettlementShardCount {
		keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard))))
	}
	return keys
}

// Combined startup predeclares both families before either publisher writes.
// The inner family checks then validate subsets without extending ownership.
func settlementStartupOwnershipKeys() []server.PgOwnershipKey {
	return append(transferDebitStartupOwnershipKeys(), legacySettlementStartupOwnershipKeys()...)
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
func ScheduleSettlementAccountingTasks(ctx context.Context) {
	server.OwnedTx(ctx, settlementStartupOwnershipKeys(), func(tx server.PgTx) {
		owner := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer owner.Cancel()
		ScheduleFlushTransferDebits(owner, tx)
		ScheduleFlushLegacySettlements(owner, tx)
	}, server.TxReadCommitted, server.OptNoRetry())
}
