// Registered factories retain financial task identity and expose the complete
// immutable publication scope before the worker begins its completion Tx.
package work

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func TestSettlementTaskFactoriesDeclareActualQueueScope(t *testing.T) {
	for _, pair := range []struct {
		actual task.Target
		plain  task.Target
	}{
		{actual: NewTransferDebitTaskTarget(), plain: task.NewTaskTarget(FlushTransferDebits)},
		{actual: model.NewLegacyNetEscrowMirrorTaskTarget(), plain: task.NewTaskTarget(model.ApplyLegacyNetEscrowMirror)},
		{actual: model.NewLegacyProviderTotalsTaskTarget(), plain: task.NewTaskTarget(model.ApplyLegacyProviderTotals)},
		{actual: NewScheduledContractClosureTaskTarget(), plain: task.NewTaskTarget(CloseScheduledContract)},
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
}

// An older worker's scheduled close result can carry an owner handoff to the
// retired legacy settlement queue. Its completion declares no legacy queue key,
// and its Post publishes nothing: the expiry sweep retires such a contract.
func TestScheduledContractClosureIgnoresRetiredOwnerHandoff(t *testing.T) {
	contractId, payerNetworkId := server.NewId(), server.NewId()
	args := &CloseScheduledContractArgs{Private: true, ScheduledContractClose: ScheduledContractClose{ContractId: contractId, Deadline: server.NowUtc()}}
	argsJson, err := json.Marshal(args)
	server.Raise(err)
	resultJson, err := json.Marshal(map[string]any{
		"owner":          map[string]any{"kind": "payer-network", "id": payerNetworkId},
		"reconciliation": map[string]any{"contract_id": contractId},
	})
	server.Raise(err)
	declaration := NewScheduledContractClosureTaskTarget().(task.TaskCompletionOwnershipTarget)
	keys, err := declaration.TaskCompletionOwnershipKeys(&task.Task{ArgsJson: string(argsJson)}, string(resultJson))
	if err != nil || len(keys) != 0 {
		t.Fatal("a retired owner handoff declared legacy queue keys", keys, err)
	}
	var result CloseScheduledContractResult
	if err := json.Unmarshal(resultJson, &result); err != nil || result.Reconciliation == nil || result.Reconciliation.ContractId != contractId ||
		result.RetryAt != nil || result.DeferredUntil != nil {
		t.Fatal("an older result with an owner handoff did not decode as a completed close", result, err)
	}
	clientSession := session.NewLocalClientSession(t.Context(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	// No transaction: a Post that wrote anything here would fail.
	if err := CloseScheduledContractPost(args, &result, clientSession, nil); err != nil {
		t.Fatal("a retired owner handoff did not complete as a no-op", err)
	}
}
