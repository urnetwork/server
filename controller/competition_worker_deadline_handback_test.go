// This regression fixes the ordering of the existing deadline handback path;
// it grants no new retry, clock, recovery or production behavior.
package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// The real evaluation context reaches its original deadline while cleanup is
// held. An acknowledged unbuffered heartbeat edge then necessarily wins over
// the unavailable evaluator result; handback must wait for cleanup to join.
// The next Store claim is synthetic, while evaluateOne's expired-job decision
// and bounded terminal-persistence path are the actual production code.
func TestCompetitionWorkerDeadlineHeartbeatHandbackThenExpiredReclaim(t *testing.T) {
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
		if err := fixture.Finish(t); !errors.Is(err, context.DeadlineExceeded) || fixture.ctx.Err() != nil ||
			fixture.store.heartbeatCalls != 0 || fixture.store.hostCalls != 0 || fixture.store.diagnosticCalls != 1 ||
			fixture.store.reason != "heartbeat_failed" || fixture.store.detail.Category != "deadline_exceeded" || !fixture.store.joined ||
			fixture.store.completedCount() != 0 || fixture.evaluator.calls != 1 || !fixture.job.StartedAt.Equal(originalStart) || fixture.job.AttemptCount != originalAttempt {
			t.Fatal("deadline handback changed its existing join, cause, attempt or original clock", err)
		}

		// Model only the next ordinary Claim's fresh lease/attempt. Its start
		// remains the original COALESCE-preserved timestamp, not claim time.
		reclaimed := *fixture.job
		reclaimed.AttemptCount++
		freshExpiry := server.NowUtc().Add(time.Minute)
		reclaimed.LeaseExpiresAt = &freshExpiry
		store := &fakeStore{claimJobs: []*queuedJob{&reclaimed}}
		evaluator := &competitionCompletionEvaluator{evaluate: func(context.Context) EvaluationOutcome {
			t.Error("an expired ordinary reclaim invoked the evaluator")
			return EvaluationOutcome{}
		}}
		worker, err := newWorkerWithImageDigest(fixture.worker.settings, store, evaluator, fixture.worker.workerId, testWorkerImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.Claim(fixture.ctx, fixture.worker.settings, fixture.worker.workerId, testWorkerImageDigest())
		if err != nil || claimed == nil || claimed.JobId != fixture.job.JobId {
			t.Fatal("synthetic ordinary reclaim lost the original job")
		}
		if err := worker.evaluateOne(fixture.ctx, claimed, host); err != nil {
			t.Fatal("expired reclaim failed its independent terminal metadata path", err)
		}
		outcomes := store.outcomes()
		if evaluator.calls != 0 || len(outcomes) != 1 || outcomes[0].Score != nil || outcomes[0].Error == nil ||
			outcomes[0].Error.Code != "evaluation_time_budget_exhausted" || outcomes[0].Error.Retriable || store.handbacks != 0 ||
			!claimed.StartedAt.Equal(originalStart) || !claimed.StartedAt.Add(time.Duration(policy.ScoreTimeoutSeconds)*time.Second).Equal(originalDeadline) ||
			claimed.AttemptCount != originalAttempt+1 || !reflect.DeepEqual(fixture.worker.settings.EvaluationPolicy, policy) {
			t.Fatal("expired reclaim gained evaluation, a new deadline or another infrastructure retry")
		}
	})
}
