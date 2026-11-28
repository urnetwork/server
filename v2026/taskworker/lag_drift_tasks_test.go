package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The drift sweep's chain (lag.dev/DRIFTTEST.md E5, K3, R1) is registered
// with the worker and armed by InitTasks, once however often a worker starts,
// as the lag sweep's is. Unarmed, a run nobody reads would stay open, and a
// closed token's client_ip hash would outlive it.
func TestInitTasksArmsTheDriftSweep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		if !taskWorker.HasTarget("github.com/urnetwork/server/v2026/taskworker/work.SweepLagDrift") {
			t.Fatal("the drift sweep is not a registered task target")
		}

		for range 2 {
			InitTasks(ctx)
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = '["sweep_lag_drift"]'`,
				).Scan(&count))
			})
			connect.AssertEqual(t, count, 1)
		}
	})
}
