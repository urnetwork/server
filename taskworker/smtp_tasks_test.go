package taskworker

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
)

// §14.14's four smtpedge jobs (SMTP-M9-17): registered with the worker, and armed once by InitTasks in a binary
// that links MG5 and MG9 only, however often it starts. A chain never armed never runs, and then smtp_send's days
// outlive their 30 days, retired selectors stay in DNS, and no RUA signal reaches §21.
func TestInitTasksArmsTheSmtpJobs(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		taskWorker := InitTaskWorker(ctx)
		for _, functionName := range []string{
			"github.com/urnetwork/server/controller.SmtpRetention",
			"github.com/urnetwork/server/controller.SmtpReconcileTask",
			"github.com/urnetwork/server/taskworker/work.SmtpSealedReports",
			"github.com/urnetwork/server/taskworker/work.SmtpDkimExpiry",
		} {
			if !taskWorker.HasTarget(functionName) {
				t.Fatalf("%s is not a registered task target", functionName)
			}
		}

		want := 0
		if server.SidecarMg5Linked() && server.SidecarMg9Linked() {
			want = 1
		}
		for range 2 {
			InitTasks(ctx)
			for _, runOnceKey := range []string{"smtp_retention", "smtp_reconcile", "smtp_sealed_reports", "smtp_dkim_expiry"} {
				var count int
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(
						ctx,
						`SELECT COUNT(*) FROM pending_task WHERE run_once_key = $1`,
						fmt.Sprintf("[%q]", runOnceKey),
					).Scan(&count))
				})
				if count != want {
					t.Fatalf("%s is armed %d times, want %d", runOnceKey, count, want)
				}
			}
		}
	})
}
