// Natural scoring expiry joins the same evaluator's retained cleanup outcome;
// it is not a database heartbeat incident or authority for another claim.
package controller

import (
	"context"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// The real evaluation context reaches its original deadline while cleanup is
// held. An acknowledged unbuffered heartbeat edge necessarily wins over the
// unavailable result; only ownership metadata may renew on the live parent.
func TestCompetitionWorkerDeadlineJoinsSameAttemptWithoutHeartbeatIncident(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetryWorkerFixture(t)
		policy := fixture.worker.settings.EvaluationPolicy
		originalStart := server.NowUtc().Add(-time.Duration(policy.ScoreTimeoutSeconds)*time.Second + time.Second)
		fixture.job.StartedAt = &originalStart
		originalDeadline := originalStart.Add(time.Duration(policy.ScoreTimeoutSeconds) * time.Second)
		originalAttempt := fixture.job.AttemptCount
		ticks := make(chan time.Time)
		fixture.worker.evaluationTickForTest = ticks
		host := passingHostCheck(fixture.worker.settings)
		host.RebaselinePassed, host.RebaselineRoundId = true, &fixture.job.RoundId
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			fixture.done <- fixture.worker.evaluateOne(fixture.ctx, fixture.job, host)
		}()
		t.Cleanup(func() {
			fixture.cancel()
			fixture.release.Do(func() { close(fixture.evaluator.release) })
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
			defer cancel()
			select {
			case <-finished:
			case <-cleanup.Done():
				t.Error("deadline handback worker did not join cleanup")
			}
		})
		select {
		case <-fixture.evaluator.started:
		case <-fixture.watchdog.Done():
			t.Fatal("evaluator did not reach its start boundary")
		}
		select {
		case <-fixture.evaluator.canceled:
		case <-fixture.watchdog.Done():
			t.Fatal("original one-second remaining allowance did not expire")
		}
		deadline, present := fixture.evaluator.evaluationCtx.Deadline()
		if !present || !deadline.Equal(originalDeadline) || fixture.evaluator.evaluationCtx.Err() != context.DeadlineExceeded || fixture.ctx.Err() != nil {
			t.Fatal("fixture did not stop only the original evaluation deadline")
		}
		select {
		case ticks <- server.NowUtc():
		case <-fixture.watchdog.Done():
			t.Fatal("worker did not select the forced post-deadline heartbeat")
		}
		// No result can be sent until release. Reading these fields precedes
		// that channel close, so no handback writer can race this assertion.
		if fixture.store.diagnosticCalls != 0 || fixture.store.completedCount() != 0 || fixture.evaluator.joined.Load() {
			t.Fatal("deadline handback or completion preceded joined evaluator cleanup")
		}
		if err := fixture.Finish(t); err != nil || fixture.ctx.Err() != nil ||
			fixture.store.heartbeatCalls > 1 || fixture.store.hostCalls > fixture.store.heartbeatCalls || fixture.store.diagnosticCalls != 0 ||
			fixture.store.handbacks != 0 || fixture.store.claims != 0 || fixture.store.completedCount() != 1 ||
			fixture.evaluator.calls != 1 || !fixture.evaluator.joined.Load() || !fixture.job.StartedAt.Equal(originalStart) || fixture.job.AttemptCount != originalAttempt {
			t.Fatal("natural deadline must complete the joined same attempt without a heartbeat incident or reclaim", err)
		}
		outcomes := fixture.store.outcomes()
		if len(outcomes) != 1 || outcomes[0].Score != nil || outcomes[0].Error == nil ||
			outcomes[0].Error.Code != "evaluation_time_budget_exhausted" || outcomes[0].Error.Retriable ||
			!fixture.job.StartedAt.Add(time.Duration(policy.ScoreTimeoutSeconds)*time.Second).Equal(originalDeadline) ||
			!reflect.DeepEqual(fixture.worker.settings.EvaluationPolicy, policy) {
			t.Fatal("deadline completion gained score authority, a new clock or another infrastructure retry")
		}
	})
}
