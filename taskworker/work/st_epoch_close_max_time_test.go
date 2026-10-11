// Verify that every scheduled epoch close allows the census read its time.
package work

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// One census statement reads every contract closed in the epoch: the 24-hour
// epoch 0's 9.2M contracts took 195-419 s on Main, and the 7-day epoch 1
// projects to 64-71M. A one-hour limit cancels a close whose census runs
// past it on every attempt; the scheduled close and its retry both allow
// three hours.
func TestStEpochCloseAllowsTheCensusThreeHours(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: common.HexToAddress("0x0000000000000000000000000000000000000041")}
		controller.SetStConfig(cfg)
		t.Cleanup(func() { controller.SetStConfig(nil) })
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		functionName := task.NewTaskTargetWithPost(StEpochClose, StEpochClosePost).TargetFunctionName()
		maxTimeSeconds := func() (int, int) {
			var count, seconds int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*), COALESCE(max(run_max_time_seconds), 0) FROM pending_task WHERE function_name=$1`, functionName).Scan(&count, &seconds))
			})
			return count, seconds
		}
		scheduleStEpochClose(owner, cfg.DeploymentKey(), 3, 0, server.NowUtc())
		if count, seconds := maxTimeSeconds(); count != 1 || seconds != int((3*time.Hour)/time.Second) {
			t.Fatal("the scheduled close cannot finish a 7-day census", count, seconds)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE function_name=$1`, functionName))
		})
		args := &StEpochCloseArgs{DeploymentKey: string(cfg.DeploymentKey()), Epoch: 3, Attempt: 4}
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(StEpochClosePost(args, &StEpochCloseResult{Retry: true}, owner, tx))
		})
		if count, seconds := maxTimeSeconds(); count != 1 || seconds != int((3*time.Hour)/time.Second) {
			t.Fatal("the retried close cannot finish a 7-day census", count, seconds)
		}
	})
}
