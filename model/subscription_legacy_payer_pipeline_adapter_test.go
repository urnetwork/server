// Register the exact production payer target; no fixture function replaces it.
package model

import (
	"testing"

	"github.com/urnetwork/server/task"
)

func legacyPayerPipelineAdditionalTargets() []task.Target {
	return []task.Target{NewLegacyPayerSettlementTaskTarget()}
}

func legacyPayerPipelineMirrorTarget() task.Target {
	return NewLegacyNetEscrowMirrorTaskTarget()
}

// The completion interface opts the mirror into queue and finished-row ownership,
// including same-key revision publication after the function has completed.
func TestLegacyPayerPipelineMirrorDeclaresCompletionOwnership(t *testing.T) {
	mirror := legacyPayerPipelineMirrorTarget()
	if mirror.TargetFunctionName() != task.NewTaskTarget(ApplyLegacyNetEscrowMirror).TargetFunctionName() {
		t.Fatal("pipeline replaced the production mirror function")
	}
	declared, ok := mirror.(task.TaskCompletionOwnershipTarget)
	if !ok {
		t.Fatal("pipeline mirror omitted its completion owner")
	}
	keys, err := declared.TaskCompletionOwnershipKeys(nil, "")
	if err != nil || len(keys) != 0 {
		t.Fatal("mirror completion must retain its own queue and finished-row scope", keys, err)
	}
}
