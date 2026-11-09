//go:build sidecar_pending

package taskworker

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A binary that links MG2 arms one run-once row for the spent pass however often a worker starts.
func TestInitTasksArmsTheQuotaSpentPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for range 2 {
			InitTasks(ctx)
			var count int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = '["quota_spent_pass"]'`,
				).Scan(&count))
			})
			connect.AssertEqual(t, count, 1)
		}
	})
}
