package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The stats export loop was re-enabled 2026-08-08 after being gated for load:
// the old loop recomputed seven 90-day aggregation passes every 30 SECONDS on
// the primary. This pins both halves of that decision: the loop is enabled
// (a pending_task row is scheduled), and it runs at exportStatsInterval
// (hourly), not the old 30s cadence — so neither a re-gate nor a cadence
// regression can land silently.
func TestExportStatsScheduledAtHourlyCadence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		if exportStatsDisabled {
			t.Fatal("export loop must be enabled (user decision 2026-08-08)")
		}
		if exportStatsInterval < 15*time.Minute {
			t.Fatalf("export cadence %v is under 15m — the 30s-loop load problem returns", exportStatsInterval)
		}

		before := server.NowUtc()
		var taskId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleExportStats(clientSession, tx)
			// find the row it scheduled
			result, err := tx.Query(
				ctx,
				`SELECT task_id, run_at FROM pending_task WHERE run_once_key = '["export_stats"]'`,
			)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					t.Fatal("ScheduleExportStats scheduled nothing while enabled")
				}
				var runAt time.Time
				server.Raise(result.Scan(&taskId, &runAt))
				// scheduled exportStatsInterval out, not 30s out
				delay := runAt.Sub(before)
				if delay < exportStatsInterval-time.Minute || delay > exportStatsInterval+time.Minute {
					t.Fatalf("export scheduled %v out, want ~%v", delay, exportStatsInterval)
				}
			})
		})

		tasks := task.GetTasks(ctx, taskId)
		if tasks[taskId] == nil {
			t.Fatal("scheduled export task not readable")
		}
	})
}
