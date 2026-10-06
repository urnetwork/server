package work

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server/v2026/controller"
)

// Persisted task payloads survive process and configuration rollovers. Every
// old or pre-key payload must terminate before touching the active controller,
// and its Post hook must not enqueue a retry or the next pipeline stage.
func TestStSettlementTasksRejectStaleCoordinatorPayloads(t *testing.T) {
	cfg := &controller.StConfig{
		Enabled: true, ChainId: 945,
		ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"),
	}
	controller.SetStConfig(cfg)
	t.Cleanup(func() { controller.SetStConfig(nil) })
	current := string(cfg.DeploymentKey())
	stale := "945:0x1000000000000000000000000000000000000001"
	if key, ok := stTaskDeploymentCurrent(current); !ok || string(key) != current {
		t.Fatalf("current deployment key=%q/%t, want %q/true", key, ok, current)
	}
	for _, value := range []string{"", stale} {
		if key, ok := stTaskDeploymentCurrent(value); ok || key != "" {
			t.Fatalf("stale deployment %q accepted as %q", value, key)
		}
	}

	closeArgs := &StEpochCloseArgs{DeploymentKey: stale, Epoch: 7, Attempt: 99}
	closeResult, err := StEpochClose(closeArgs, nil)
	if err != nil || closeResult.Retry {
		t.Fatalf("stale close result/error=%+v/%v", closeResult, err)
	}
	if err := StEpochClosePost(closeArgs, closeResult, nil, nil); err != nil {
		t.Fatal(err)
	}

	commitArgs := &StCommitRootArgs{DeploymentKey: stale, Epoch: 7, Attempt: 99}
	commitResult, err := StCommitRoot(commitArgs, nil)
	if err != nil || commitResult.Retry {
		t.Fatalf("stale commit result/error=%+v/%v", commitResult, err)
	}
	if err := StCommitRootPost(commitArgs, commitResult, nil, nil); err != nil {
		t.Fatal(err)
	}

	depositArgs := &StDepositArgs{DeploymentKey: stale, Epoch: 7, Attempt: 7}
	depositResult, err := StDeposit(depositArgs, nil)
	if err != nil || depositResult.Retry {
		t.Fatalf("stale deposit result/error=%+v/%v", depositResult, err)
	}
	if err := StDepositPost(depositArgs, depositResult, nil, nil); err != nil {
		t.Fatal(err)
	}

	finalizeArgs := &StFinalizePokeArgs{DeploymentKey: stale, Epoch: 7, Attempt: 99}
	finalizeResult, err := StFinalizePoke(finalizeArgs, nil)
	if err != nil || finalizeResult.Retry {
		t.Fatalf("stale finalize result/error=%+v/%v", finalizeResult, err)
	}
	if err := StFinalizePokePost(finalizeArgs, finalizeResult, nil, nil); err != nil {
		t.Fatal(err)
	}

	if result, err := StSyncChain(&StSyncChainArgs{DeploymentKey: stale}, nil); err != nil || result == nil {
		t.Fatalf("stale sync result/error=%+v/%v", result, err)
	}
}
