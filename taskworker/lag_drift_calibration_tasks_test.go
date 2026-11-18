package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The drift model's weekly calibration (lag.dev/DRIFTTEST.md V4) is
// registered with the worker and armed by InitTasks, once however often a
// worker starts.
//
// Both halves are pinned for the reason the lag sweep's are: a chain armed
// with no registered target is reaped as a removed target, and a registered
// target never armed never runs. Unarmed, the drift test would judge with the
// defaults, or a calibration months old, with nothing to say so.
func TestInitTasksArmsTheLagDriftCalibration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		if !taskWorker.HasTarget("github.com/urnetwork/server/taskworker/work.LagDriftCalibration") {
			t.Fatal("the drift calibration is not a registered task target")
		}

		for range 2 {
			InitTasks(ctx)
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = '["lag_drift_calibration"]'`,
				).Scan(&count))
			})
			connect.AssertEqual(t, count, 1)
		}
	})
}
