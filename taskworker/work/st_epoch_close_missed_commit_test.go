// Verify that an epoch whose payout is still held when its root commit window
// closes leaves the close retry loop and reaches the missed-root finalize path.
package work

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Chain reads for one ended epoch whose emission was never captured. The
// payout authority read fails, standing in for any held payout such as a
// missing wallet roster. Methods not listed here panic.
type missedCommitStClient struct {
	controller.StClient
	state                *controller.StEpochState
	closeBlock           uint64
	deferCount           int
	payoutAuthorityCount int
}

func (self *missedCommitStClient) Epoch(context.Context) (*controller.StEpochState, error) {
	return self.state, nil
}

func (self *missedCommitStClient) EpochCloseBlock(context.Context, uint64) (uint64, error) {
	return self.closeBlock, nil
}

func (self *missedCommitStClient) PoolState(context.Context, uint64, uint64) (*controller.StPoolState, error) {
	return &controller.StPoolState{PoolTotalRao: big.NewInt(0), ClaimedRao: big.NewInt(0)}, nil
}

func (self *missedCommitStClient) DeferMissedEmission(context.Context, uint64, uint64) (string, error) {
	self.deferCount += 1
	return "0xsyntheticdefer", nil
}

func (self *missedCommitStClient) PayoutEpochAuthority(context.Context, uint64) (*controller.StPayoutEpochAuthority, error) {
	self.payoutAuthorityCount += 1
	return nil, errors.New("synthetic payout held")
}

// Configures a synthetic release deployment whose epoch 0 spans blocks
// [0, 7200) with a 1200-block root commit window, so its deadline is 8400.
func newMissedCommitStClient(t testing.TB, headBlock uint64) (*missedCommitStClient, model.StDeploymentKey) {
	cfg := &controller.StConfig{
		Enabled:         true,
		ChainId:         945,
		Netuid:          521,
		NoId:            1,
		DeploymentId:    "synthetic-missed-commit",
		ContractAddress: common.HexToAddress("0x0000000000000000000000000000000000000031"),
		SettlementVault: common.HexToAddress("0x0000000000000000000000000000000000000032"),
		BlockSeconds:    12,
	}
	client := &missedCommitStClient{
		state: &controller.StEpochState{
			Epoch:                1,
			PendingEpoch:         1,
			EpochStartBlock:      7_200,
			TEpochBlocks:         7_200,
			CommitWindowBlocks:   1_200,
			TrailsWindowBlocks:   1_200,
			FinalizeOffsetBlocks: 2_400,
			CloseGraceBlocks:     5,
			HeadBlock:            headBlock,
			HeadBlockTime:        server.NowUtc(),
		},
		closeBlock: 7_200,
	}
	controller.SetStConfig(cfg)
	controller.SetStClient(client)
	t.Cleanup(func() { controller.SetStClient(nil); controller.SetStConfig(nil) })
	deploymentKey := cfg.DeploymentKey()
	model.UpsertStEpoch(context.Background(), deploymentKey, &model.StEpoch{
		Epoch:               0,
		StartBlock:          0,
		CommitDeadlineBlock: 8_400,
		TrailsDeadlineBlock: 8_400,
		FinalizeBlock:       9_600,
		Status:              model.StEpochStatusOpen,
	})
	return client, deploymentKey
}

// Runs one close attempt and its Post, then counts the persisted close
// continuations for the epoch.
func runMissedCommitStEpochClose(t testing.TB, owner *session.ClientSession, deploymentKey model.StDeploymentKey) (*StEpochCloseResult, int) {
	ctx := owner.Ctx
	args := &StEpochCloseArgs{DeploymentKey: string(deploymentKey), Epoch: 0}
	result, err := StEpochClose(args, owner)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(StEpochClosePost(args, result, owner, tx))
	})
	functionName := task.NewTaskTargetWithPost(StEpochClose, StEpochClosePost).TargetFunctionName()
	var continuationCount int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name=$1 AND args_json::jsonb->>'epoch'='0'`, functionName).Scan(&continuationCount))
	})
	return result, continuationCount
}

// Once the commit window has closed no root can be committed. The close must
// not recompute the held payout on every retry with the epoch left open,
// where the sync never schedules its finalize and the funded pool total never
// carries. It defers the emission, closes without leaves and does not retry.
func TestStEpochCloseAfterCommitWindowReachesMissedRootFinalize(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		client, deploymentKey := newMissedCommitStClient(t, 8_401)
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		result, continuationCount := runMissedCommitStEpochClose(t, owner, deploymentKey)
		if result.Retry || continuationCount != 0 || client.payoutAuthorityCount != 0 {
			t.Fatal("held payout kept retrying after its commit window closed", result.Retry, continuationCount, client.payoutAuthorityCount)
		}
		if client.deferCount != 1 {
			t.Fatal("missed emission was not deferred before the epoch closed", client.deferCount)
		}
		row := model.GetStEpoch(ctx, deploymentKey, 0)
		if row == nil || row.Status != model.StEpochStatusClosed {
			t.Fatal("epoch past its commit window stayed out of the finalize path", row)
		}
		if leaves := model.GetStPayoutLeaves(ctx, deploymentKey, 0, 1); len(leaves) != 0 {
			t.Fatal("closed missed epoch stored payout leaves", len(leaves))
		}
	})
}

// The deadline block itself still admits a commit, so the payout is computed
// and a failure keeps its retry with the epoch open.
func TestStEpochCloseAtCommitDeadlineKeepsComputingPayout(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		client, deploymentKey := newMissedCommitStClient(t, 8_400)
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		result, continuationCount := runMissedCommitStEpochClose(t, owner, deploymentKey)
		if !result.Retry || continuationCount != 1 || client.payoutAuthorityCount != 1 {
			t.Fatal("payout inside its commit window lost its retry", result.Retry, continuationCount, client.payoutAuthorityCount)
		}
		row := model.GetStEpoch(ctx, deploymentKey, 0)
		if row == nil || row.Status != model.StEpochStatusOpen {
			t.Fatal("epoch inside its commit window left the close path", row)
		}
	})
}
