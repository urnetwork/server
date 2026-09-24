package taskworker

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The extender task chains (connect/EXTENDER.md C3, C4).

// Both extender data producers, the retention of what extenders and pingers
// report, and the derivation over it (connect/GEOMAP.md §5) are registered
// with the worker and armed by InitTasks.
//
// The two halves fail in different silent ways, which is why both are pinned
// here. A chain that is armed with no registered target is reaped as a removed
// target at the next start; a registered target that is never armed simply
// never runs. Either way nothing reports it: a directory that stops being
// probed and republished keeps its addresses until their records expire a day
// later, and a sweep that never runs lets the latency and ping tables grow
// past the window the derive phase reads (connect/GEOMAP.md §5.7).
func TestInitTasksArmsTheExtenderChains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		for _, functionName := range []string{
			"github.com/urnetwork/server/taskworker/work.ExtenderProbe",
			"github.com/urnetwork/server/taskworker/work.ExtenderPublish",
			"github.com/urnetwork/server/taskworker/work.RemoveOldExtenderLatencies",
			"github.com/urnetwork/server/taskworker/work.RemoveExpiredPings",
			"github.com/urnetwork/server/taskworker/work.DeriveLocations",
		} {
			if !taskWorker.HasTarget(functionName) {
				t.Fatalf("%s is not a registered task target", functionName)
			}
		}

		InitTasks(ctx)

		countForRunOnceKey := func(runOnceKey string) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = $1`,
					fmt.Sprintf("[%q]", runOnceKey),
				)
				server.WithPgResult(result, err, func() {
					connect.AssertEqual(t, result.Next(), true)
					server.Raise(result.Scan(&count))
				})
			})
			return
		}
		for _, runOnceKey := range []string{"extender_probe", "extender_publish", "remove_old_extender_latencies", "remove_expired_pings", "derive_locations"} {
			if count := countForRunOnceKey(runOnceKey); count != 1 {
				t.Fatalf("%s is armed %d times, want once", runOnceKey, count)
			}
		}

		// a second start does not arm a second chain, since the run once key is
		// what keeps one chain per task
		InitTasks(ctx)
		for _, runOnceKey := range []string{"extender_probe", "extender_publish", "remove_old_extender_latencies", "remove_expired_pings", "derive_locations"} {
			if count := countForRunOnceKey(runOnceKey); count != 1 {
				t.Fatalf("%s is armed %d times after a second start, want once", runOnceKey, count)
			}
		}
	})
}
