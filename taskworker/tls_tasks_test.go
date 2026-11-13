package taskworker

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
)

// certs' chains (TLS-M5-17; §13.8, §19.4): orders, renewals, the revocation
// sweep and the purge are registered with the worker and, in a binary that
// links MG5, armed once by InitTasks, as the extender's are
// (extender_tasks_test.go). A chain armed with no registered target is reaped
// at the next start, and a registered target never armed never runs: neither
// reports itself, and a sweep that never runs leaves a removed device's
// certificate good until it expires (I-15).
func TestInitTasksArmsTheTlsCertChains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		for _, functionName := range []string{
			"github.com/urnetwork/server/taskworker/work.TlsCertOrders",
			"github.com/urnetwork/server/taskworker/work.TlsCertRenewals",
			"github.com/urnetwork/server/taskworker/work.TlsCertRevocations",
			"github.com/urnetwork/server/taskworker/work.TlsCertPurge",
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
		runOnceKeys := []string{"tls_cert_orders", "tls_cert_renewals", "tls_cert_revocations", "tls_cert_purge"}
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
