// Registered factories retain financial task identity and expose the complete
// immutable publication scope before the worker begins its completion Tx.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
)

// Direct Post controls declare the same immutable result scope as the real
// finalizer, including the original recurring shard key.
func withLegacyDispatcherQueueTestTx(ctx context.Context, shard int, result *FlushLegacySettlementsResult, callback func(server.PgTx)) {
	data, err := json.Marshal(result)
	server.Raise(err)
	keys, err := NewLegacySettlementDispatcherTaskTarget().(task.TaskCompletionOwnershipTarget).TaskCompletionOwnershipKeys(nil, string(data))
	server.Raise(err)
	keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard))))
	server.OwnedTx(ctx, keys, callback, server.TxReadCommitted, server.OptNoRetry())
}

func TestSettlementTaskFactoriesDeclareActualQueueScope(t *testing.T) {
	for _, pair := range []struct {
		actual task.Target
		plain  task.Target
	}{
		{actual: NewLegacySettlementDispatcherTaskTarget(), plain: task.NewTaskTarget(FlushLegacySettlements)},
		{actual: NewTransferDebitTaskTarget(), plain: task.NewTaskTarget(FlushTransferDebits)},
		{actual: model.NewLegacyPayerSettlementTaskTarget(), plain: task.NewTaskTarget(model.ApplyLegacyPayerSettlements)},
		{actual: model.NewLegacyNetEscrowMirrorTaskTarget(), plain: task.NewTaskTarget(model.ApplyLegacyNetEscrowMirror)},
		{actual: model.NewLegacyProviderTotalsTaskTarget(), plain: task.NewTaskTarget(model.ApplyLegacyProviderTotals)},
	} {
		if pair.actual.TargetFunctionName() != pair.plain.TargetFunctionName() {
			t.Fatal("ownership wrapper changed the durable task function")
		}
		declaration, ok := pair.actual.(task.TaskCompletionOwnershipTarget)
		if !ok {
			t.Fatal("actual close-path factory omitted queue-owned completion", pair.actual.TargetFunctionName())
		}
		if keys, err := declaration.TaskCompletionOwnershipKeys(nil, `{}`); err != nil || len(keys) != 0 {
			t.Fatal("same-key completion invented another publication scope", keys, err)
		}
	}
	dispatcher := NewLegacySettlementDispatcherTaskTarget().(task.TaskCompletionOwnershipTarget)
	payers := []server.Id{server.NewId(), server.NewId()}
	data, err := json.Marshal(FlushLegacySettlementsResult{Dispatch: &model.LegacySettlementDispatchResult{PayerNetworkIds: payers}})
	server.Raise(err)
	keys, err := dispatcher.TaskCompletionOwnershipKeys(nil, string(data))
	if err != nil || len(keys) != len(payers) {
		t.Fatal("dispatcher did not declare every bounded payer", keys, err)
	}
	for _, id := range payers {
		if !slices.Contains(keys, task.RunOnceOwnershipKey(task.RunOnce("flush_legacy_payer_settlements", id))) {
			t.Fatal("dispatcher declaration differs from its actual publisher identity")
		}
	}
	for _, bad := range []string{`{`, `{"dispatch":{"payer_network_ids":["00000000-0000-0000-0000-000000000000"]}}`} {
		if _, err := dispatcher.TaskCompletionOwnershipKeys(nil, bad); err == nil {
			t.Fatal("dispatcher admitted malformed completion publication", bad)
		}
	}
	tooMany := make([]server.Id, 17)
	for i := range tooMany {
		tooMany[i] = server.NewId()
	}
	data, err = json.Marshal(FlushLegacySettlementsResult{Dispatch: &model.LegacySettlementDispatchResult{PayerNetworkIds: tooMany}})
	server.Raise(err)
	if _, err := dispatcher.TaskCompletionOwnershipKeys(nil, string(data)); err == nil {
		t.Fatal("dispatcher expanded beyond its sixteen bounded publications")
	}
}
