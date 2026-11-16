package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// §21.9's hold pass (abuse_hold_work.go; ABUSE-M5-14), on MG5's tables in a build with sidecar_pending: one pass an
// hour, and a pass in a process with no hold/ writer ending nothing. The pass itself is controller's
// (abuse_hold_controller_test.go).

// The hold pass is scheduled an hour ahead, once.
func TestAbuseHoldPassIsScheduledHourly(t *testing.T) {
	if !server.SidecarMg5Linked() {
		t.Skip("MG5 is pending: these cases run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleAbuseHoldPass(clientSession, tx)
		})
		runAt, found := testAbusePendingRunAt(t, ctx, "abuse_hold_pass")
		if !found || runAt.Before(before.Add(model.DefaultAbuseHoldSettings().PassInterval-time.Second)) {
			t.Fatalf("the pass runs at %s, found %t", runAt, found)
		}
		// no hold/ writer in this process: the pass counts and ends nothing
		result, err := AbuseHoldPass(&AbuseHoldPassArgs{}, clientSession)
		if err != nil || result.EndedCount != 0 {
			t.Fatalf("%+v, %v", result, err)
		}
	})
}
