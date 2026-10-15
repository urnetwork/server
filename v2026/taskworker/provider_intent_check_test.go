// The provider intent check runs on the production worker as a per-client
// chain that reschedules itself while the client is connected with intent.
package taskworker

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
)

// The pending checks of one client chain.
func testingProviderIntentPendingChecks(ctx context.Context, clientId server.Id) []*task.Task {
	runOnceKey := task.RunOnce("provider_intent_check", clientId).String()
	checks := []*task.Task{}
	for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
		if pending.RunOnceKey == runOnceKey {
			checks = append(checks, pending)
		}
	}
	return checks
}

// The production worker runs the chain: schedules of one client merge into one
// pending check, a run reschedules the next check, and a client without a live
// state ends its chain.
func TestProviderIntentCheckChainOnProductionWorker(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		worker := InitTaskWorker(ctx)
		defer worker.Close()

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "testProviderIntentChain", server.NewId())
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")

		// an intent connection starts the attempt and the chain
		result := model.ConnectProviderIntent(ctx, networkId, clientId, 5*time.Minute)
		if !result.ScheduleCheck || result.State.Status != model.ProviderIntentStatusPending {
			t.Fatalf("connect did not start an attempt: %+v", result)
		}
		// the claim admits a run one whole second after its run_at (rounded), so
		// the test schedules two seconds back rather than waiting
		runAt := result.State.CheckTime.Add(-2 * time.Second)
		controller.ScheduleProviderIntentCheck(ctx, networkId, clientId, runAt)
		// a second connection of the client merges into the same chain
		controller.ScheduleProviderIntentCheck(ctx, networkId, clientId, runAt)
		if checks := testingProviderIntentPendingChecks(ctx, clientId); len(checks) != 1 {
			t.Fatalf("pending checks = %d, want one chain", len(checks))
		}

		finished, retried, postRetried, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
			t.Fatalf("the check did not run: %v/%v/%v %v", finished, retried, postRetried, err)
		}
		// the first check requests probe priority and renews it in the grace:
		// the next run is an extend timeout after the run, within the grace
		checks := testingProviderIntentPendingChecks(ctx, clientId)
		if len(checks) != 1 ||
			checks[0].RunAt.Before(result.State.AttemptTime.Add(model.ProviderIntentGraceExtendTimeout)) ||
			checks[0].RunAt.After(result.State.GraceEndTime) {
			t.Fatalf("the chain did not reschedule in the grace: %+v", checks)
		}

		// a client without a live state ends its chain
		awayClientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), awayClientId, "synthetic", "synthetic")
		controller.ScheduleProviderIntentCheck(ctx, networkId, awayClientId, server.NowUtc().Add(-2*time.Second))
		finished, _, _, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 {
			t.Fatalf("the away check did not run: %v %v", finished, err)
		}
		if checks := testingProviderIntentPendingChecks(ctx, awayClientId); len(checks) != 0 {
			t.Fatalf("a chain without a live state rescheduled: %+v", checks)
		}
	})
}
