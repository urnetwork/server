// Raising a pending run-once task's deadline, against the real pending_task row.
package task

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Raising a pending run-once task's deadline lengthens a shorter one, never
// shortens a longer one, and schedules nothing when no task is pending.
func TestRaiseRunOnceMaxTimeOnlyLengthensAPendingTask(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		shortKey := RunOnce("raise_run_once_max_time", "short")
		longKey := RunOnce("raise_run_once_max_time", "long")
		absentKey := RunOnce("raise_run_once_max_time", "absent")
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleTaskInTx(tx, claimProfileAllowed, &claimProfileArgs{}, clientSession, shortKey, MaxTime(30*time.Second))
			ScheduleTaskInTx(tx, claimProfileAllowed, &claimProfileArgs{}, clientSession, longKey, MaxTime(48*time.Hour))
		})

		var raisedShort, raisedLong, raisedAbsent bool
		server.Tx(ctx, func(tx server.PgTx) {
			raisedShort = RaiseRunOnceMaxTimeInTx(ctx, tx, shortKey, 24*time.Hour)
			raisedLong = RaiseRunOnceMaxTimeInTx(ctx, tx, longKey, 24*time.Hour)
			raisedAbsent = RaiseRunOnceMaxTimeInTx(ctx, tx, absentKey, 24*time.Hour)
		})
		if !raisedShort || raisedLong || raisedAbsent {
			t.Fatal("raise reported the wrong rows", raisedShort, raisedLong, raisedAbsent)
		}

		maxTimeSeconds := func(key *RunOnceOption) (count int, seconds int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx,
					`SELECT count(*), coalesce(max(run_max_time_seconds), 0) FROM pending_task WHERE run_once_key=$1`,
					key.String(),
				).Scan(&count, &seconds))
			})
			return
		}
		if count, seconds := maxTimeSeconds(shortKey); count != 1 || seconds != int((24*time.Hour)/time.Second) {
			t.Fatal("shorter deadline was not raised", count, seconds)
		}
		if count, seconds := maxTimeSeconds(longKey); count != 1 || seconds != int((48*time.Hour)/time.Second) {
			t.Fatal("longer deadline was shortened", count, seconds)
		}
		if count, _ := maxTimeSeconds(absentKey); count != 0 {
			t.Fatal("raise scheduled a task that was not pending", count)
		}
	})
}
