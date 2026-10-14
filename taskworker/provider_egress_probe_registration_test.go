// The taskworker registers the URL probe with the post-step's work for after
// the commit, which recreates shards that lost their rows.
package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// A registration that ran only the transactional post-step left a lost shard
// missing until a taskworker restarted. Through the production worker, a pass
// of shard 0 must recreate missing shard 2 once its finishing transaction
// commits. The claimed snapshots predate the URL arguments, so the pass
// retires them without opening a tunnel.
func TestProductionProbeRegistrationRecreatesMissingShards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		withProbePlacementGeometry(t, 3, 8, 0)
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			for shardIndex := range 2 {
				runAt := server.NowUtc().Add(time.Hour)
				if shardIndex == 0 {
					runAt = server.NowUtc().Add(-time.Minute)
				}
				task.ScheduleTaskInTx(tx, work.ProviderEgressProbe, &work.ProviderEgressProbeArgs{ShardIndex: shardIndex, ShardCount: 3},
					clientSession, task.RunOnce("provider_egress_probe", shardIndex), task.RunAt(runAt), task.MaxTime(15*time.Minute))
			}
		})

		worker := InitTaskWorker(ctx)
		defer worker.Close()
		finished, retried, postRetried, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
			t.Fatalf("the production worker did not finish the pass of shard 0: finished=%d retried=%d err=%v", len(finished), len(retried)+len(postRetried), err)
		}
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("provider_egress_probe", 2).String()).Scan(&count))
		})
		if count != 1 {
			t.Fatalf("the production registration did not recreate missing shard 2: %d rows", count)
		}
	})
}
