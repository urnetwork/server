package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// §21.9's copy pass (abuse_hold_copy_work.go; ABUSE-M5-15), on MG5's tables in a build with sidecar_pending: one
// pass a minute under the key a placement kicks, and a kick pulling it to now. The copy itself is model's
// (abuse_hold_copy_model_test.go).

// The run time of the pending task under a run-once key, and whether there is one.
func testAbusePendingRunAt(t testing.TB, ctx context.Context, runOnceKey string) (runAt time.Time, found bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT run_at FROM pending_task WHERE run_once_key = $1`, `["`+runOnceKey+`"]`)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&runAt))
				found = true
			}
		})
	})
	return
}

// The pass is scheduled once a minute ahead, a second schedule folding into it, and a kick pulls it to now.
func TestAbuseHoldCopyIsScheduledAndKicked(t *testing.T) {
	if !server.SidecarMg5Linked() {
		t.Skip("MG5 is pending: these cases run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		before := server.NowUtc()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleAbuseHoldCopy(clientSession, tx)
			})
		}
		runAt, found := testAbusePendingRunAt(t, ctx, model.AbuseHoldCopyRunOnceKey)
		interval := model.DefaultAbuseHoldCopySettings().PassInterval
		if !found || runAt.Before(before.Add(interval-time.Second)) {
			t.Fatalf("the pass runs at %s, found %t", runAt, found)
		}
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM pending_task WHERE run_once_key = $1`, `["`+model.AbuseHoldCopyRunOnceKey+`"]`).Scan(&count))
		})
		if count != 1 {
			t.Fatalf("%d pending passes", count)
		}

		model.KickAbuseHoldCopy(ctx)
		kicked, _ := testAbusePendingRunAt(t, ctx, model.AbuseHoldCopyRunOnceKey)
		if !kicked.Before(before.Add(interval - time.Second)) {
			t.Fatalf("the kick left the pass at %s", kicked)
		}

		// a pass with nothing pending does nothing
		result, err := AbuseHoldCopy(&AbuseHoldCopyArgs{}, clientSession)
		if err != nil || result.CompletedCount != 0 || result.FailedCount != 0 {
			t.Fatalf("%+v, %v", result, err)
		}
	})
}
