package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func ledgerV2WorkConfig() *controller.StConfig {
	cfg := &controller.StConfig{
		Profile: "testnet", Enabled: true, ChainId: 945, NoId: 1, BlockSeconds: 12, DeployBlock: 352_800,
		ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"),
		SettlementV2:    common.HexToAddress("0x00000000000000000000000000000000000000a1"),
		VaultV2:         common.HexToAddress("0x00000000000000000000000000000000000000a2"),
	}
	cfg.GenesisHash[0] = 1
	return cfg
}

// ledgerV2WorkChain registers pool 1 from epoch 5 on a 50,400-block cadence;
// its state read can be made to fail.
type ledgerV2WorkChain struct {
	controller.StLedgerV2Client
	stateErr error
}

func (self *ledgerV2WorkChain) LedgerV2Pool(context.Context, uint64) (bool, uint64, error) {
	return true, 5, nil
}

// The vault is bound to the STSettlement2 of ledgerV2WorkConfig.
func (self *ledgerV2WorkChain) LedgerV2Coordinator(context.Context) (common.Address, error) {
	return common.HexToAddress("0x00000000000000000000000000000000000000a1"), nil
}

// Epoch e ends where epoch e+1 starts in the v1 clock below.
func (self *ledgerV2WorkChain) LedgerV2EpochWindow(_ context.Context, epoch uint64) (uint64, uint64, error) {
	return (epoch + 1) * 50_400, 1_200, nil
}

func (self *ledgerV2WorkChain) LedgerV2State(context.Context, uint64) (*controller.StLedgerV2State, error) {
	return nil, self.stateErr
}

// ledgerV2WorkV1Chain serves the v1 epoch clock at block 352,810 (epoch 7).
type ledgerV2WorkV1Chain struct {
	controller.StClient
}

func (self *ledgerV2WorkV1Chain) Epoch(context.Context) (*controller.StEpochState, error) {
	return &controller.StEpochState{Epoch: 7, PendingEpoch: 7, EpochStartBlock: 352_800, TEpochBlocks: 50_400, CommitWindowBlocks: 1_200, HeadBlock: 352_810, HeadBlockTime: server.NowUtc()}, nil
}

func (self *ledgerV2WorkV1Chain) FinalizedHead(context.Context) (uint64, [32]byte, time.Time, error) {
	return 352_810, [32]byte{1}, server.NowUtc(), nil
}

func (self *ledgerV2WorkV1Chain) BlockHashes(_ context.Context, blocks []uint64) ([][32]byte, error) {
	return make([][32]byte, len(blocks)), nil
}

func (self *ledgerV2WorkV1Chain) SyncEvents(_ context.Context, _ uint64, toBlock uint64) ([]*model.StChainEvent, uint64, error) {
	return nil, toBlock + 1, nil
}

// pendingStArgs returns the epochs, or the step kinds and epochs, of the
// pending tasks of one function.
func pendingStArgs(t testing.TB, ctx context.Context, function string) []string {
	out := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT args_json FROM pending_task WHERE function_name = $1`, function)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var argsJson string
				server.Raise(rows.Scan(&argsJson))
				var args struct {
					Kind    string `json:"kind"`
					Epoch   uint64 `json:"epoch"`
					Attempt int    `json:"attempt"`
				}
				server.Raise(json.Unmarshal([]byte(argsJson), &args))
				out = append(out, strings.TrimSpace(fmt.Sprintf("%s %d attempt %d", args.Kind, args.Epoch, args.Attempt)))
			}
		})
	})
	sort.Strings(out)
	return out
}

// SV-4 (work layer): a sync turn schedules v1 close tasks only for epochs
// below the pool's first v2 epoch, and starts the v2 step for the latest ended
// epoch at or after it.
func TestStSyncChainKeepsV1BelowFirstV2Epoch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := ledgerV2WorkConfig()
		controller.SetStConfig(cfg)
		controller.SetStClient(&ledgerV2WorkV1Chain{})
		controller.SetStLedgerV2Client(&ledgerV2WorkChain{})
		defer func() {
			controller.SetStConfig(nil)
			controller.SetStClient(nil)
			controller.SetStLedgerV2Client(nil)
		}()
		key := cfg.DeploymentKey()
		for epoch := uint64(4); epoch <= 6; epoch++ {
			model.UpsertStEpoch(ctx, key, &model.StEpoch{Epoch: epoch, StartBlock: epoch * 50_400, Status: model.StEpochStatusOpen})
		}
		runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(StSyncChain, StSyncChainPost, &StSyncChainArgs{DeploymentKey: string(key)}), true)
		if got := pendingStArgs(t, ctx, task.NewTaskTarget(StEpochClose).TargetFunctionName()); len(got) != 1 || got[0] != "4 attempt 0" {
			t.Fatalf("v1 close tasks %v, want only epoch 4", got)
		}
		if got := pendingStArgs(t, ctx, task.NewTaskTarget(StLedgerV2Step).TargetFunctionName()); len(got) != 1 || got[0] != "epoch 6 attempt 0" {
			t.Fatalf("v2 steps %v, want the epoch 6 step", got)
		}
	})
}

// SV-9 (work layer): once the pool's activation is recorded, a sync turn
// without the v2 pair completes and recurs but schedules nothing and leaves
// the mirror's checkpoint; the turn after the pair is restored mirrors on from
// that checkpoint and schedules the v1 close below the first v2 epoch and the
// v2 step again.
func TestStLedgerV2ActivationStopsTheSyncTurnWithoutThePair(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := ledgerV2WorkConfig()
		off := *cfg
		off.SettlementV2, off.VaultV2 = common.Address{}, common.Address{}
		controller.SetStConfig(cfg)
		controller.SetStClient(&ledgerV2WorkV1Chain{})
		controller.SetStLedgerV2Client(&ledgerV2WorkChain{})
		defer func() {
			controller.SetStConfig(nil)
			controller.SetStClient(nil)
			controller.SetStLedgerV2Client(nil)
		}()
		key := cfg.DeploymentKey()
		if err := model.RecordStLedgerV2Activation(ctx, key, &model.StLedgerV2Activation{NoId: 1, FirstEpoch: 5,
			VaultV2Address: "0x00000000000000000000000000000000000000a2", SettlementV2Address: "0x00000000000000000000000000000000000000a1"}); err != nil {
			t.Fatal(err)
		}
		for epoch := uint64(4); epoch <= 6; epoch++ {
			model.UpsertStEpoch(ctx, key, &model.StEpoch{Epoch: epoch, StartBlock: epoch * 50_400, Status: model.StEpochStatusOpen})
		}
		model.SetStChainCheckpoint(ctx, key, model.StChainCheckpoint{NextBlock: 352_000})
		closes, steps := task.NewTaskTarget(StEpochClose).TargetFunctionName(), task.NewTaskTarget(StLedgerV2Step).TargetFunctionName()

		controller.SetStConfig(&off)
		runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(StSyncChain, StSyncChainPost, &StSyncChainArgs{DeploymentKey: string(key)}), true)
		for _, function := range []string{closes, task.NewTaskTarget(StCommitRoot).TargetFunctionName(), task.NewTaskTarget(StDeposit).TargetFunctionName(), steps} {
			if got := pendingStArgs(t, ctx, function); len(got) != 0 {
				t.Fatalf("a turn without the v2 pair scheduled %s %v", function, got)
			}
		}
		if checkpoint := model.GetStChainCheckpoint(ctx, key); checkpoint.NextBlock != 352_000 {
			t.Fatalf("a turn without the v2 pair moved the checkpoint to %+v", checkpoint)
		}

		controller.SetStConfig(cfg)
		if _, err := StSyncChain(&StSyncChainArgs{DeploymentKey: string(key)}, owner); err != nil {
			t.Fatal(err)
		}
		if got := pendingStArgs(t, ctx, closes); len(got) != 1 || got[0] != "4 attempt 0" {
			t.Fatalf("v1 close tasks %v, want only epoch 4", got)
		}
		if got := pendingStArgs(t, ctx, steps); len(got) != 1 || got[0] != "epoch 6 attempt 0" {
			t.Fatalf("v2 steps %v, want the epoch 6 step", got)
		}
		if checkpoint := model.GetStChainCheckpoint(ctx, key); checkpoint.NextBlock != 352_811 {
			t.Fatalf("the restored turn mirrored to %+v, want from the checkpoint through the head", checkpoint)
		}
	})
}

// SV-1 (retries, work layer): a step whose chain read fails persists one
// continuation with the next attempt under the same run-once key; a step of
// another deployment does nothing and schedules nothing.
func TestStLedgerV2StepPersistsRetries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := ledgerV2WorkConfig()
		controller.SetStConfig(cfg)
		controller.SetStLedgerV2Client(&ledgerV2WorkChain{stateErr: errors.New("synthetic state unavailable")})
		defer func() {
			controller.SetStConfig(nil)
			controller.SetStLedgerV2Client(nil)
		}()
		function := task.NewTaskTarget(StLedgerV2Step).TargetFunctionName()
		runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(StLedgerV2Step, StLedgerV2StepPost, &StLedgerV2StepArgs{DeploymentKey: "945:0x1000000000000000000000000000000000000001", Kind: controller.StLedgerV2StepEpoch, Epoch: 7}), false)
		if got := pendingStArgs(t, ctx, function); len(got) != 0 {
			t.Fatalf("another deployment's step scheduled %v", got)
		}
		runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(StLedgerV2Step, StLedgerV2StepPost, &StLedgerV2StepArgs{DeploymentKey: string(cfg.DeploymentKey()), Kind: controller.StLedgerV2StepEpoch, Epoch: 7}), true)
		if got := pendingStArgs(t, ctx, function); len(got) != 1 || got[0] != "epoch 7 attempt 1" {
			t.Fatalf("continuations %v", got)
		}
		result, err := StLedgerV2Step(&StLedgerV2StepArgs{DeploymentKey: string(cfg.DeploymentKey()), Kind: "unknown", Epoch: 7}, owner)
		if err != nil || !result.Retry {
			t.Fatalf("an unknown step: %+v, %v", result, err)
		}
		if err := StLedgerV2StepPost(&StLedgerV2StepArgs{DeploymentKey: string(cfg.DeploymentKey()), Kind: controller.StLedgerV2StepEpoch, Epoch: 7, Attempt: stLedgerV2StepMaxAttempts - 1}, result, owner, nil); err != nil {
			t.Fatalf("a last attempt scheduled a continuation: %v", err)
		}
	})
}
