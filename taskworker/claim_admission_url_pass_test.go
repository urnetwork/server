// URL shard passes are claimed one per Run loop. A production worker must hold
// as many concurrent passes as its configured geometry allows, and no more.
package taskworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Holds every claimed URL pass in its task function until release, so a pass
// stays claimed without opening a tunnel. The release returns an error and
// leaves the row queued for retry.
type heldUrlPassTarget struct {
	task.Target
	started chan struct{}
	release chan struct{}
}

// Signals the claim, then waits for release or cancellation.
func (self *heldUrlPassTarget) Run(ctx context.Context, _ *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	self.started <- struct{}{}
	select {
	case <-self.release:
	case <-ctx.Done():
	}
	return nil, nil, errors.New("synthetic URL pass hold released")
}

// Main restored 8 shards of 64 URL workers on six taskworker instances, and
// each instance admitted one pass, so two shards waited for a free instance
// for the whole interval. Under that geometry one production worker holds two
// passes from two claims and leaves the third pass queued for another worker.
func TestProductionWorkerHoldsUrlPassesUpToGeometry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		withProbePlacementGeometry(t, 8, 64, 0)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		worker := task.NewTaskWorker(ctx, taskWorkerSettingsForProfile(task.DefaultTaskWorkerSettings(), WorkloadProfileProduction))
		defer worker.Close()
		held := &heldUrlPassTarget{
			Target:  task.NewTaskTargetWithPost(work.ProviderEgressProbe, work.ProviderEgressProbePost),
			started: make(chan struct{}, 3),
			release: make(chan struct{}),
		}
		worker.AddTargets(held)
		taskIds := make([]server.Id, 3)
		for shardIndex := range taskIds {
			taskIds[shardIndex] = task.ScheduleTask(work.ProviderEgressProbe,
				&work.ProviderEgressProbeArgs{ShardIndex: shardIndex, ShardCount: 8}, clientSession,
				task.RunOnce("synthetic_url_pass_placement", shardIndex),
				task.RunAt(server.NowUtc().Add(-time.Minute)), task.MaxTime(15*time.Minute))
		}

		// Each production Run loop claims at most one long-running pass.
		evaluated := make(chan int, len(taskIds))
		launched, joined := 0, 0
		evaluate := func() {
			launched++
			go func() {
				finished, retried, postRetried, err := worker.EvalTasks(1)
				if err != nil {
					t.Errorf("URL pass claim failed: %v", err)
				}
				evaluated <- len(finished) + len(retried) + len(postRetried)
			}()
		}
		released := false
		defer func() {
			if !released {
				close(held.release)
			}
			for ; joined < launched; joined++ {
				<-evaluated
			}
		}()

		evaluate()
		<-held.started
		evaluate()
		select {
		case <-held.started:
		case count := <-evaluated:
			joined++
			t.Fatalf("worker refused a second URL shard pass the 8 x 64 geometry allows (claimed %d)", count)
		}
		evaluate()
		select {
		case <-held.started:
			t.Fatal("worker held a third URL shard pass beyond its 128-worker instance concurrency")
		case count := <-evaluated:
			joined++
			if count != 0 {
				t.Fatalf("saturated worker evaluated %d tasks", count)
			}
		}

		claimedCount := 0
		now := server.NowUtc()
		for _, queued := range task.GetTasks(ctx, taskIds...) {
			if now.Before(queued.ReleaseTime) {
				claimedCount++
			}
		}
		if claimedCount != 2 {
			t.Fatalf("%d URL shard passes are claimed, want 2 held and 1 queued", claimedCount)
		}
		close(held.release)
		released = true
	})
}
