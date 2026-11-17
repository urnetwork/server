// A recovered cycle retains its own original clock while a wholly unadmitted
// acquisition recovers. No grant, database, image or runtime policy is changed.
package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
)

// An expired original execution cannot become a new three-hour window; only
// the already-established cycle start supplies this evaluator's deadline.
func TestCompetitionWorkerAcquireRetryPreservesRecoveredCycleDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionAcquireWorkerFixture(t)
		originalStartedAt, cycleStartedAt := server.NowUtc().Add(-24*time.Hour), server.NowUtc().Add(-time.Minute)
		fixture.job.StartedAt, fixture.job.CycleStartedAt = &originalStartedAt, &cycleStartedAt
		fixture.job.EvaluationCycle, fixture.job.CycleAttemptOffset, fixture.job.AttemptCount = 1, 2, 3
		acknowledged := make(chan struct{})
		fixture.store.heartbeat = func(ctx context.Context, call int) error {
			if call == 1 {
				<-ctx.Done()
				return competitionHeartbeatAcquireFailure(ctx.Err())
			}
			return nil
		}
		fixture.worker.evaluationHeartbeatForTest = func() { close(acknowledged) }
		fixture.Start(t)
		fixture.ticks <- server.NowUtc()
		select {
		case <-acknowledged:
		case <-fixture.evaluator.canceled:
			t.Fatal("proven recovered-cycle acquisition deadline canceled the original evaluator")
		case <-fixture.watchdog.Done():
			t.Fatal("recovered-cycle heartbeat did not reach its acknowledgement")
		}
		deadline, present := fixture.evaluator.evaluationCtx.Deadline()
		expectedDeadline := cycleStartedAt.Add(time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
		authority, scoped := fixture.evaluator.evaluationCtx.Value(competitionJobAuthorityKey{}).(competitionJobAuthority)
		if !present || !deadline.Equal(expectedDeadline) || !scoped || authority.Cycle != 1 || authority.Attempt != 3 || authority.JobId != fixture.job.JobId ||
			fixture.evaluator.evaluationCtx.Err() != nil || fixture.evaluator.calls != 1 || fixture.store.heartbeatCalls != 2 || fixture.store.hostCalls != 1 ||
			!fixture.job.StartedAt.Equal(originalStartedAt) || !fixture.job.CycleStartedAt.Equal(cycleStartedAt) ||
			fixture.job.EvaluationCycle != 1 || fixture.job.CycleAttemptOffset != 2 || fixture.job.AttemptCount != 3 || fixture.store.diagnosticCalls != 0 {
			t.Fatal("acquisition recovery changed existing cycle authority, clock or evaluator")
		}
		fixture.cancel()
		if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || !fixture.store.joined || fixture.store.reason != "worker_shutdown" {
			t.Fatal("recovered cycle lost joined parent shutdown", err)
		}
	})
}
