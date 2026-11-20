// Persisted v2 activation must stop continuations from already-running v1 tasks.
package work

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Exercises the real Post transaction after activation, including a successful
// close that finished before the latch and retry results retained across it.
func TestStV1PostsStopAtRecordedActivation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, Netuid: 521, NoId: 7, DeploymentId: "synthetic-post-cutoff", ContractAddress: common.Address{1}, SettlementV2: common.Address{2}, VaultV2: common.Address{3}}
		controller.SetStConfig(cfg)
		t.Cleanup(func() { controller.SetStConfig(nil) })
		ctx := t.Context()
		key := cfg.DeploymentKey()
		if err := model.RecordStLedgerV2Activation(ctx, key, &model.StLedgerV2Activation{NoId: cfg.NoId, FirstEpoch: 4, VaultV2Address: strings.ToLower(cfg.VaultV2.Hex()), SettlementV2Address: strings.ToLower(cfg.SettlementV2.Hex())}); err != nil {
			t.Fatal(err)
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(StEpochClosePost(&StEpochCloseArgs{DeploymentKey: string(key), Epoch: 4}, &StEpochCloseResult{Retry: true}, owner, tx))
			server.Raise(StEpochClosePost(&StEpochCloseArgs{DeploymentKey: string(key), Epoch: 5}, &StEpochCloseResult{}, owner, tx))
			server.Raise(StCommitRootPost(&StCommitRootArgs{DeploymentKey: string(key), Epoch: 6}, &StCommitRootResult{Retry: true}, owner, tx))
		})
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE args_json::jsonb->>'deployment_key'=$1`, string(key)).Scan(&count))
		})
		if count != 0 {
			t.Fatalf("retired v1 tasks scheduled %d continuations after recorded activation", count)
		}
	})
}
