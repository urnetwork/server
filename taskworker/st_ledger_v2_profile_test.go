package taskworker

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/startifact"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// SV-1 (wiring): both the production and the subnet operator workloads run
// the settlement v2 driver's step task.
func TestWorkloadsRunLedgerV2Steps(t *testing.T) {
	target := task.NewTaskTarget(work.StLedgerV2Step).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(context.Background(), task.DefaultTaskWorkerSettings(), profile)
		if err != nil {
			t.Fatal(err)
		}
		if !worker.HasTarget(target) {
			t.Errorf("%s workload lacks %s", profile, target)
		}
		worker.Close()
	}
}

// §8.2 (server start), SV-1 (wiring): a task worker installs the server's
// settlement v2 ledger builder when v2 is configured, so the driver's steps
// build and read ledgers with it: a read of a ledger missing from the store
// reaches the store. Without v2 it installs none.
func TestWorkloadsRunLedgerV2StepsWithTheBuilder(t *testing.T) {
	controller.SetStLedgerV2ObjectStore(server.NewLocalBlobStore(t.TempDir(), "test"))
	t.Cleanup(func() {
		controller.SetStConfig(nil)
		controller.SetStLedgerBuilder(nil)
		controller.SetStLedgerV2ObjectStore(nil)
	})
	for _, v2 := range []bool{false, true} {
		cfg := &controller.StConfig{Enabled: true}
		if v2 {
			cfg.SettlementV2, cfg.VaultV2 = common.Address{1}, common.Address{2}
		}
		controller.SetStConfig(cfg)
		controller.SetStLedgerBuilder(nil)
		worker, err := InitTaskWorkerForProfile(context.Background(), task.DefaultTaskWorkerSettings(), WorkloadProfileSubnetOperator)
		if err != nil {
			t.Fatal(err)
		}
		worker.Close()
		_, _, err = controller.StLedgerV2Tree(context.Background(), [32]byte{7}, [32]byte{}, new(uint256.Int), 1)
		if v2 && !errors.Is(err, startifact.ErrLedgerObjectNotFound) || !v2 && (err == nil || err.Error() != "st ledger v2: no ledger builder is installed") {
			t.Fatalf("v2 configured %t: %v", v2, err)
		}
	}
}
