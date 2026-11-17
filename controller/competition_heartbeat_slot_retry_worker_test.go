// The actual worker adapters retain one evaluator and the original job clock.
// Virtual deadlines plus explicit cleanup barriers avoid native processes.
package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
)

// The live worker must actually select the new job-only predicate; testing
// its standalone helper would miss a stale call to the acquisition-only path.
func TestCompetitionWorkerSlotContinuationKeepsEvaluatorAndOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetryWorkerFixture(t)
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), time.Minute)
		defer stopWatchdog()
		fixture.watchdog = watchdog
		fixture.worker.confirmedHostLeaseExpiresAt = fixture.clock.Now().Add(time.Minute)
		originalStarted, originalAttempt := *fixture.job.StartedAt, fixture.job.AttemptCount
		acknowledged := make(chan struct{}, 1)
		fixture.worker.evaluationHeartbeatForTest = func() { acknowledged <- struct{}{} }
		fixture.store.heartbeat = func(ctx context.Context, call int) error {
			if call == 1 {
				<-ctx.Done()
				time.Sleep(5 * time.Second)
				return competitionHeartbeatSlotFailure(ctx.Err(), server.DbTxReleaseDestroyTransferred)
			}
			return nil
		}
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-acknowledged:
		case <-fixture.evaluator.canceled:
			t.Fatal("known no-COMMIT deadline canceled the original evaluator")
		case <-watchdog.Done():
			t.Fatal("worker did not select its bounded no-COMMIT continuation")
		}
		deadline, ok := fixture.evaluator.evaluationCtx.Deadline()
		if !ok || !deadline.Equal(originalStarted.Add(time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second)) ||
			fixture.evaluator.evaluationCtx.Err() != nil || fixture.evaluator.calls != 1 || fixture.store.heartbeatCalls != 2 || fixture.store.hostCalls != 1 ||
			fixture.job.AttemptCount != originalAttempt || !fixture.job.StartedAt.Equal(originalStarted) || fixture.store.reason != "" {
			t.Fatal("continuation recreated a claim/evaluator/clock or handed back healthy work")
		}
		fixture.cancel()
		if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || !fixture.store.joined || fixture.store.reason != "worker_shutdown" {
			t.Fatal("later normal shutdown did not join its original evaluator", err)
		}
	})
}

// Ambiguous commit remains a failure through the public worker path. Handback
// must wait for explicit evaluator cleanup, not merely for cancellation.
func TestCompetitionWorkerSlotContinuationRejectsCommitAndJoinsBeforeHandback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetryWorkerFixture(t)
		watchdog, stopWatchdog := context.WithTimeout(t.Context(), time.Minute)
		defer stopWatchdog()
		fixture.watchdog = watchdog
		fixture.worker.confirmedHostLeaseExpiresAt = fixture.clock.Now().Add(time.Minute)
		fixture.store.heartbeat = func(ctx context.Context, _ int) error {
			<-ctx.Done()
			proof := competitionHeartbeatSlotFailure(ctx.Err(), server.DbTxReleaseDestroyTransferred)
			proof.snapshot.Transaction.CommitAttempts = 1
			return proof
		}
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-fixture.evaluator.canceled:
		case <-watchdog.Done():
			t.Fatal("ambiguous COMMIT did not cancel its evaluator")
		}
		if fixture.store.reason != "" || fixture.evaluator.joined.Load() {
			t.Fatal("handback overtook the held evaluator cleanup")
		}
		if err := fixture.Finish(t); err == nil || fixture.store.heartbeatCalls != 1 || !fixture.store.joined || fixture.store.reason != "heartbeat_failed" {
			t.Fatal("ambiguous COMMIT replayed or skipped joined failure handback", err)
		}
	})
}
