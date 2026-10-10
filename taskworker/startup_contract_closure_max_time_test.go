// The all-nonterminal pass must outlive the default task deadline.
package taskworker

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Production held two passes that never finished: one under the earlier key
// with a 30s deadline and one under the startup key with the 2 minute default.
// Every attempt timed out and restarted. Startup keeps both pending passes and
// gives each a 24 hour deadline.
func TestTaskworkerStartupGivesEveryPendingScanTwentyFourHours(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "startup-close.example")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		legacyKey := task.RunOnce("schedule_open_contract_closures")
		startupKey := task.RunOnce("schedule_open_contract_closures_on_startup")
		startedAt := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			task.ScheduleTaskInTx(tx, work.ScheduleOpenContractClosures,
				&work.ScheduleOpenContractClosuresArgs{StartedAt: startedAt},
				clientSession, legacyKey, task.RunAt(server.NowUtc()), task.MaxTime(30*time.Second))
			task.ScheduleTaskInTx(tx, work.ScheduleOpenContractClosures,
				&work.ScheduleOpenContractClosuresArgs{PageSize: 1024, StartedAt: startedAt},
				clientSession, startupKey, task.RunAt(server.NowUtc()))
		})

		server.Raise(initTaskScheduleForProfile(model.WithProviderWorkSessionSource(ctx, nil), WorkloadProfileProduction))

		wanted := int((24 * time.Hour) / time.Second)
		server.Db(ctx, func(conn server.PgConn) {
			for _, key := range []*task.RunOnceOption{legacyKey, startupKey} {
				var count, maxTimeSeconds int
				server.Raise(conn.QueryRow(ctx,
					`SELECT count(*), coalesce(max(run_max_time_seconds), 0) FROM pending_task WHERE run_once_key=$1`,
					key.String(),
				).Scan(&count, &maxTimeSeconds))
				if count != 1 {
					t.Fatal("startup changed how many passes are pending", key.String(), count)
				}
				if maxTimeSeconds != wanted {
					t.Fatal("pending pass keeps a deadline shorter than 24 hours", key.String(), maxTimeSeconds)
				}
			}
		})
	})
}
