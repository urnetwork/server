package task

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/session"
)

// The global LIMIT window contains only the older producer. Its newer
// downstream consumer cannot enter that window until the producer drains.
// Run both policies against the real claim/evaluate/finalize helpers, with an
// exact number of admissions rather than an elapsed-time throughput assertion.
func TestEvalTasksFunctionFairnessBreaksBacklogStarvation(t *testing.T) {
	for _, fair := range []bool{false, true} {
		runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			settings := DefaultTaskWorkerSettings()
			settings.FairClaimFunctions = fair
			worker := NewTaskWorker(ctx, settings)
			defer worker.Close()
			producer := &claimProfileTarget{Target: NewTaskTarget(claimProfileAllowed)}
			consumer := &claimProfileTarget{Target: NewTaskTarget(claimProfileExcluded)}
			worker.AddTargets(producer, consumer)
			past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			for range 140 {
				ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(past))
			}
			ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, owner, RunAt(past.Add(time.Hour)))
			for range 2 * len(worker.claimFunctionNames) {
				finished, retried, postRetried, err := worker.EvalTasks(1)
				if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
					t.Fatal("claim/evaluation failed", fair, err, finished, retried, postRetried)
				}
			}
			if want := int32(0); fair {
				want = 1
				if consumer.runs.Load() != want || consumer.posts.Load() != want {
					t.Fatal("downstream task starved despite enabled fairness", consumer.runs.Load(), consumer.posts.Load())
				}
			} else if consumer.runs.Load() != want {
				t.Fatal("negative control did not reproduce the bounded-window starvation")
			}
			if producer.runs.Load() == 0 || producer.runs.Load() >= 140 {
				t.Fatal("test did not preserve an active older backlog")
			}
		})
	}
}
