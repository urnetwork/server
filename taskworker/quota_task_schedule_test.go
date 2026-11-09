//go:build sidecar_pending

package taskworker

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A binary that links MG2 arms one run-once row for the spent pass and one per service debit shard,
// however often a worker starts.
func TestInitTasksArmsTheQuotaChains(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		count := func(runOnceKey string) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM pending_task WHERE run_once_key = $1`,
					runOnceKey,
				).Scan(&count))
			})
			return
		}
		for range 2 {
			InitTasks(ctx)
			connect.AssertEqual(t, count(`["quota_spent_pass"]`), 1)
			for shard := range model.ServiceDebitShardCount {
				connect.AssertEqual(t, count(fmt.Sprintf(`["%s"]`, model.ServiceDebitFlushRunOnceKey(shard))), 1)
			}
		}
	})
}
