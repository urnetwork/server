package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The lag sweep's chain (lag.dev/PLAN.md A2, A5) is registered with the
// worker and armed by InitTasks, once however often a worker starts.
//
// Both halves are pinned for the reason the extender chains are: a chain
// armed with no registered target is reaped as a removed target, and a
// registered target never armed never runs. Unswept, every lag guest's jwt
// would outlive its expiry for as long as expired tokens are accepted.
func TestInitTasksArmsTheLagSweep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		if !taskWorker.HasTarget("github.com/urnetwork/server/taskworker/work.SweepLag") {
			t.Fatal("the lag sweep is not a registered task target")
		}

		for range 2 {
			InitTasks(ctx)
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = '["sweep_lag"]'`,
				).Scan(&count))
			})
			connect.AssertEqual(t, count, 1)
		}
	})
}
