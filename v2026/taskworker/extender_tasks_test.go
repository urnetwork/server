package taskworker

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The extender task chains (connect/EXTENDER.md C3, C4).

// Both extender tasks are registered with the worker and armed by InitTasks.
//
// The two halves fail in different silent ways, which is why both are pinned
// here. A chain that is armed with no registered target is reaped as a removed
// target at the next start; a registered target that is never armed simply
// never runs. Either way the directory stops being probed and republished, and
// nothing reports it: the addresses keep being published until their records
// expire a fortnight later.
func TestInitTasksArmsTheExtenderChains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		for _, functionName := range []string{
			"github.com/urnetwork/server/v2026/taskworker/work.ExtenderProbe",
			"github.com/urnetwork/server/v2026/taskworker/work.ExtenderPublish",
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
		for _, runOnceKey := range []string{"extender_probe", "extender_publish"} {
			if count := countForRunOnceKey(runOnceKey); count != 1 {
				t.Fatalf("%s is armed %d times, want once", runOnceKey, count)
			}
		}

		// a second start does not arm a second chain, since the run once key is
		// what keeps one chain per task
		InitTasks(ctx)
		for _, runOnceKey := range []string{"extender_probe", "extender_publish"} {
			if count := countForRunOnceKey(runOnceKey); count != 1 {
				t.Fatalf("%s is armed %d times after a second start, want once", runOnceKey, count)
			}
		}
	})
}
