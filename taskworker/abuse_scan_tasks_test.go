package taskworker

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
)

// §21.6's three scanners (ABUSE-M5-13): Web Risk, the feeds and the CT watch are registered with the worker and, in a
// binary that links MG5, armed once by InitTasks, as the extender's are (extender_tasks_test.go). A chain armed with
// no registered target is reaped at the next start, and a registered target never armed never runs: neither reports
// itself, and a scanner that never runs leaves a listed phishing host up until someone reports it by hand.
func TestInitTasksArmsTheAbuseScanners(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		for _, functionName := range []string{
			"github.com/urnetwork/server/taskworker/work.AbuseWebRisk",
			"github.com/urnetwork/server/taskworker/work.AbuseFeeds",
			"github.com/urnetwork/server/taskworker/work.AbuseCtWatch",
		} {
			if !taskWorker.HasTarget(functionName) {
				t.Fatalf("%s is not a registered task target", functionName)
			}
		}

		want := 0
		if server.SidecarMg5Linked() {
			want = 1
		}
		countForRunOnceKey := func(runOnceKey string) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = $1`,
					fmt.Sprintf("[%q]", runOnceKey),
				).Scan(&count))
			})
			return
		}
		runOnceKeys := []string{"abuse_web_risk", "abuse_feeds", "abuse_ct_watch"}
		// a second start does not arm a second chain: the run once key keeps one chain per task
		for range 2 {
			InitTasks(ctx)
			for _, runOnceKey := range runOnceKeys {
				if count := countForRunOnceKey(runOnceKey); count != want {
					t.Fatalf("%s is armed %d times, want %d", runOnceKey, count, want)
				}
			}
		}
	})
}

// §21.5's breakers (ABUSE-M9-02): registered with the worker, and armed once by InitTasks in a binary that links
// MG5 and MG9 only, since their rows are MG5's and the shared sending addresses they guard arrive with MG9. A pass
// that never runs leaves a listed sending address in the sending set and a level never tripped.
func TestInitTasksArmsTheAbuseBreakers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		if functionName := "github.com/urnetwork/server/taskworker/work.AbuseBreakers"; !taskWorker.HasTarget(functionName) {
			t.Fatalf("%s is not a registered task target", functionName)
		}

		want := 0
		if server.SidecarMg5Linked() && server.SidecarMg9Linked() {
			want = 1
		}
		for range 2 {
			InitTasks(ctx)
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = $1`,
					fmt.Sprintf("[%q]", "abuse_breakers"),
				).Scan(&count))
			})
			if count != want {
				t.Fatalf("abuse_breakers is armed %d times, want %d", count, want)
			}
		}
	})
}
