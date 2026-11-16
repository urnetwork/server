// The actual worker and reconciler retain one execution across positively
// closed liveness failures. Original claims, deadlines and cleanup stay intact.
package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The job heartbeat's own BEGIN-refusal proof cannot restart measurement or
// silently acquire a new three-hour deadline when a healthy retry succeeds.
func TestCompetitionWorkerPoolerRefusalKeepsOriginalEvaluation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionAcquireWorkerFixture(t)
		startedAt := *fixture.job.StartedAt
		accepted := make(chan struct{})
		fixture.store.heartbeat = func(_ context.Context, call int) error {
			if call == 1 {
				proof := competitionPoolerRefusalFailure(true)
				return competitionHeartbeatDatabaseError{cause: proof.cause, snapshot: competitionHeartbeatDatabaseSnapshot{Acquire: proof.acquire, Transaction: proof.transaction}}
			}
			return nil
		}
		fixture.worker.evaluationHeartbeatForTest = func() { close(accepted) }
		fixture.Start(t)
		fixture.ticks <- server.NowUtc()
		select {
		case <-accepted:
		case <-fixture.evaluator.canceled:
			t.Fatal("pre-callback pooler refusal canceled the original evaluator")
		case <-fixture.watchdog.Done():
			t.Fatal("pooler refusal did not reach a healthy heartbeat acknowledgment")
		}
		deadline, present := fixture.evaluator.evaluationCtx.Deadline()
		if !present || !deadline.Equal(startedAt.Add(time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second)) ||
			fixture.evaluator.calls != 1 || fixture.store.heartbeatCalls != 2 || fixture.store.hostCalls != 1 || fixture.job.AttemptCount != 1 || !fixture.job.StartedAt.Equal(startedAt) {
			t.Fatal("pooler refusal changed evaluation count, original claim or deadline")
		}
		fixture.cancel()
		if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || !fixture.store.joined {
			t.Fatal("recovered pooler refusal lost joined ordinary shutdown", err)
		}
	})
}

// Host renewal supplies its own no-admission proof and confirmed freshness.
// A job acknowledgment never substitutes for that separate authority window.
func TestCompetitionWorkerHostPoolerRefusalKeepsOriginalEvaluation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionAcquireWorkerFixture(t)
		fixture.worker.confirmedHostLeaseExpiresAt = server.NowUtc().Add(time.Minute)
		accepted := make(chan struct{})
		fixture.store.hostHeartbeat = func(_ context.Context, call int) error {
			if call == 1 {
				proof := competitionPoolerRefusalFailure(false)
				return competitionHostHeartbeatAcquireError{cause: proof.cause, acquire: proof.acquire, read: proof.read}
			}
			return nil
		}
		fixture.worker.evaluationHeartbeatForTest = func() { close(accepted) }
		fixture.Start(t)
		fixture.ticks <- server.NowUtc()
		select {
		case <-accepted:
		case <-fixture.evaluator.canceled:
			t.Fatal("unadmitted host pooler refusal canceled the original evaluator")
		case <-fixture.watchdog.Done():
			t.Fatal("host pooler refusal did not reach its own acknowledgment")
		}
		if fixture.store.hostCalls != 2 || fixture.store.heartbeatCalls != 1 || fixture.evaluator.calls != 1 || fixture.job.AttemptCount != 1 {
			t.Fatal("host pooler recovery duplicated job heartbeat, claim or evaluator")
		}
		fixture.cancel()
		if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || !fixture.store.joined {
			t.Fatal("host pooler recovery lost joined ordinary shutdown", err)
		}
	})
}

// The incident-shaped Authority deadline must keep the already-running worker
// alive after acknowledged rollback; reconciliation may not launch a successor.
func TestCompetitionSupervisorReadPrefixRetryKeepsOriginalWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionLeaseRetrySupervisorFixture(t)
		watchdog, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture.watchdog = watchdog
		fixture.driver.renewWithContext = func(ctx context.Context, call int) error {
			if call == 1 {
				<-ctx.Done()
				proof := competitionSupervisorReadLockFailure()
				proof.step = competitionSupervisorRenewalAuthority
				proof.readPrefix.count, proof.readPrefix.returned, proof.readPrefix.succeeded = 3, 3, 2
				return proof
			}
			return nil
		}
		fixture.Start(t)
		fixture.ticks <- fixture.clock.Now()
		select {
		case <-fixture.accepted:
		case <-fixture.driver.canceled:
			t.Fatal("closed authority-prefix deadline canceled the original worker")
		case <-watchdog.Done():
			t.Fatal("authority-prefix recovery did not reach its healthy acknowledgment")
		}
		fixture.finish.Do(func() { close(fixture.driver.finished) })
		result := <-fixture.done
		if result.state != "evaluating" || result.err != nil || !fixture.driver.joined || fixture.driver.renewCalls != 2 {
			t.Fatal("read-prefix recovery restarted or failed the original worker", result.state, result.err)
		}
	})
}
