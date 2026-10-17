// Register the exact production payer target; no fixture function replaces it.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/task"
)

func legacyPayerPipelineAdditionalTargets() []task.Target {
	return []task.Target{NewLegacyPayerSettlementTaskTarget()}
}

func legacyPayerPipelineMirrorTarget() task.Target {
	return NewLegacyNetEscrowMirrorTaskTarget()
}

// Keep the requested five-second test window in the measured full interval.
// Ordinary production callers retain their thirty-second collection policy.
func legacyPayerPipelineContext(ctx context.Context) context.Context {
	return Testing_WithLegacyPayerSettlementCollectionWindow(ctx)
}

func legacyPayerPipelineCollectionWindow(ctx context.Context) time.Duration {
	return legacyPayerSettlementCollectionWindow(ctx)
}

func TestLegacyPayerPipelineCollectionWindowUsesDeclaredTestDelay(t *testing.T) {
	if got := legacyPayerPipelineCollectionWindow(legacyPayerPipelineContext(t.Context())); got != 5*time.Second {
		t.Fatal("pipeline lost the declared test collection delay", got)
	}
	if got := legacyPayerPipelineCollectionWindow(t.Context()); got != 30*time.Second {
		t.Fatal("pipeline changed the ordinary production collection delay", got)
	}
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
