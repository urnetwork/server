// The candidate benchmark registers the same declared dispatcher as production.
package model_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

func legacyPayerPipelineShardTarget() task.Target {
	return work.NewLegacySettlementDispatcherTaskTarget()
}

// A generic function/post pair lacks this declaration and cannot preadmit all
// payer queue keys before completion publishes the dispatcher's durable result.
func TestLegacyPayerPipelineDispatcherDeclaresPayerCompletionKeys(t *testing.T) {
	dispatcher := legacyPayerPipelineShardTarget()
	if dispatcher.TargetFunctionName() != task.NewTaskTarget(work.FlushLegacySettlements).TargetFunctionName() {
		t.Fatal("pipeline replaced the production dispatcher function")
	}
	declared, ok := dispatcher.(task.TaskCompletionOwnershipTarget)
	if !ok {
		t.Fatal("pipeline dispatcher omitted its completion owner")
	}
	payers := []server.Id{server.NewId(), server.NewId()}
	result, err := json.Marshal(&work.FlushLegacySettlementsResult{
		Dispatch: &model.LegacySettlementDispatchResult{PayerNetworkIds: payers},
	})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := declared.TaskCompletionOwnershipKeys(nil, string(result))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := model.LegacyPayerSettlementQueueOwnershipKeys(payers)
	if err != nil || len(keys) != len(payers) || !slices.Equal(keys, expected) {
		t.Fatal("pipeline dispatcher omitted a payer queue owner", keys, expected, err)
	}
}
