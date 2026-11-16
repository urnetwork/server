package work

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// §21.7's closure-growth pass (abuse_closure_work.go; ABUSE-M5-19), on the private stack in a build with
// sidecar_pending: scheduled once, late in the UTC day, and a pass with no network over its limit opening nothing.
// The pass itself is model's (abuse_limit_model_test.go).
func TestAbuseClosureGrowthIsScheduledLateEachDay(t *testing.T) {
	if !server.SidecarMg5Linked() || !server.SidecarMg2Linked() {
		t.Skip("MG2 and MG5 are pending: these cases run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		before := server.NowUtc()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleAbuseClosureGrowth(clientSession, tx)
			})
		}
		runAt, found := testAbusePendingRunAt(t, ctx, "abuse_closure_growth")
		want := model.NextAbuseClosureGrowthTime(before, model.DefaultAbuseSettings())
		if !found || runAt.Sub(want).Abs() > 2e9 {
			t.Fatalf("the pass runs at %s, want %s", runAt, want)
		}
		result, err := AbuseClosureGrowth(&AbuseClosureGrowthArgs{}, clientSession)
		if err != nil || result.OpenedCount != 0 {
			t.Fatalf("%+v, %v", result, err)
		}
	})
}
