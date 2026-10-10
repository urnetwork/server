// Verify serialized settlement-task failure/retry and obsolete-chain behavior.
package work

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// No transport or signing method is available. Every required chain read fails
// explicitly, so a retry test cannot accidentally contact or change a chain.
type asyncAuditStClient struct {
	controller.StClient
}

// Epoch state cannot be acknowledged as fresh after a failed read.
func (*asyncAuditStClient) Epoch(context.Context) (*controller.StEpochState, error) {
	return nil, errors.New("synthetic epoch unavailable")
}

// The publication controllers must retain work when pool state is unknown.
func (*asyncAuditStClient) PoolState(context.Context, uint64, uint64) (*controller.StPoolState, error) {
	return nil, errors.New("synthetic pool unavailable")
}

// Retry results are successful task bodies only because their Post persists a
// bounded continuation with the same deployment/epoch and next attempt.
func TestStTasksPersistReadFailureContinuations(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002")}
		controller.SetStConfig(cfg)
		controller.SetStClient(&asyncAuditStClient{})
		defer func() { controller.SetStClient(nil); controller.SetStConfig(nil) }()
		key := string(cfg.DeploymentKey())
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		for _, sample := range []asyncMaintenanceCase{
			newAsyncMaintenanceCase(StEpochClose, StEpochClosePost, &StEpochCloseArgs{DeploymentKey: key, Epoch: 7}),
			newAsyncMaintenanceCase(StCommitRoot, StCommitRootPost, &StCommitRootArgs{DeploymentKey: key, Epoch: 7}),
			newAsyncMaintenanceCase(StDeposit, StDepositPost, &StDepositArgs{DeploymentKey: key, Epoch: 7}),
			newAsyncMaintenanceCase(StFinalizePoke, StFinalizePokePost, &StFinalizePokeArgs{DeploymentKey: key, Epoch: 7}),
		} {
			id := runProjectionAuditTask(t, owner, sample, true)
			var result struct {
				Retry bool `json:"retry"`
			}
			server.Raise(json.Unmarshal([]byte(task.GetFinishedTasks(ctx, id)[id].ResultJson), &result))
			server.Db(ctx, func(conn server.PgConn) {
				var correct bool
				server.Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(args_json::jsonb->>'deployment_key'=$2 AND
					args_json::jsonb->>'epoch'='7' AND args_json::jsonb->>'attempt'='1')
					FROM pending_task WHERE function_name=$1`, sample.target.TargetFunctionName(), key).Scan(&correct))
				if !result.Retry || !correct {
					t.Fatal("failed chain read lost its continuation", sample.target.TargetFunctionName(), result, correct)
				}
			})
		}
		runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(StSyncChain, StSyncChainPost, &StSyncChainArgs{DeploymentKey: key}), true)
	})
}

// The old sync Post revived an obsolete generation even though its body was a
// no-op. Exercise the real queue, including disabled and pre-key payloads.
func TestStSyncChainObsoleteTaskCompletesWithoutSuccessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002")}
		controller.SetStConfig(cfg)
		defer controller.SetStConfig(nil)
		for _, sample := range []struct {
			key     string
			enabled bool
		}{
			{key: "945:0x1000000000000000000000000000000000000001", enabled: true},
			{enabled: true},
			{key: string(cfg.DeploymentKey())},
		} {
			cfg.Enabled = sample.enabled
			controller.SetStConfig(cfg)
			runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(StSyncChain, StSyncChainPost, &StSyncChainArgs{DeploymentKey: sample.key}), false)
			server.Db(ctx, func(conn server.PgConn) {
				var pending int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_at<$1`, time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)).Scan(&pending))
				if pending != 0 {
					t.Fatal("obsolete chain revived work", pending)
				}
			})
		}
	})
}
