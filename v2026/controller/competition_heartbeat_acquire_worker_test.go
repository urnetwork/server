// The real evaluateOne loop receives controlled public store failures while
// an explicit evaluator cleanup barrier proves no duplicate or premature stop.
package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A virtual minute watchdog accommodates real eight-second virtual attempts.
// The original job clock, ticker edge and cleanup barrier remain unchanged.
func newCompetitionAcquireWorkerFixture(t *testing.T) *competitionLeaseRetryWorkerFixture {
	t.Helper()
	fixture := newCompetitionLeaseRetryWorkerFixture(t)
	watchdog, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	fixture.watchdog = watchdog
	fixture.worker.heartbeatSettings.LeaseRetry.Now = server.NowUtc
	fixture.worker.heartbeatSettings.LeaseRetry.Wait = defaultCompetitionLeaseRetrySettings().Wait
	return fixture
}

// A fully returned first acquisition deadline retains the same evaluator and
// original start/attempt/deadline when a second heartbeat actually succeeds.
func TestCompetitionWorkerAcquireRetryKeepsOriginalEvaluationAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionAcquireWorkerFixture(t)
		startedAt := *fixture.job.StartedAt
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
			t.Fatal("proven pre-acquisition deadline canceled the original evaluator")
		case <-fixture.watchdog.Done():
			t.Fatal("worker heartbeat did not reach its retry acknowledgement")
		}
		deadline, ok := fixture.evaluator.evaluationCtx.Deadline()
		if !ok || !deadline.Equal(startedAt.Add(time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second)) ||
			fixture.evaluator.evaluationCtx.Err() != nil || fixture.evaluator.calls != 1 || fixture.store.heartbeatCalls != 2 || fixture.store.hostCalls != 1 ||
			fixture.job.AttemptCount != 1 || !fixture.job.StartedAt.Equal(startedAt) || fixture.store.diagnosticCalls != 0 {
			t.Fatal("acquisition recovery changed the job budget, evaluation or claim")
		}
		fixture.cancel()
		if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || !fixture.store.joined || fixture.store.reason != "worker_shutdown" {
			t.Fatal("parent shutdown lost joined evaluator cleanup", err)
		}
	})
}

// Host acquisition uses its own acknowledged freshness, never the newly
// extended job lease. Successful recovery still leaves the evaluator running.
func TestCompetitionWorkerHostAcquireRetryUsesItsOwnConfirmedWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionAcquireWorkerFixture(t)
		fixture.worker.confirmedHostLeaseExpiresAt = server.NowUtc().Add(time.Minute)
		acknowledged := make(chan struct{})
		fixture.store.hostHeartbeat = func(ctx context.Context, call int) error {
			if call == 1 {
				<-ctx.Done()
				return competitionHostAcquireFailure(ctx.Err())
			}
			return nil
		}
		fixture.worker.evaluationHeartbeatForTest = func() { close(acknowledged) }
		fixture.Start(t)
		fixture.ticks <- server.NowUtc()
		select {
		case <-acknowledged:
		case <-fixture.evaluator.canceled:
			t.Fatal("confirmed host window did not permit safe acquisition recovery")
		case <-fixture.watchdog.Done():
			t.Fatal("host acquisition recovery did not acknowledge")
		}
		if fixture.store.hostCalls != 2 || fixture.store.heartbeatCalls != 1 || fixture.evaluator.calls != 1 || fixture.evaluator.evaluationCtx.Err() != nil {
			t.Fatal("host acquisition recovery duplicated or canceled evaluation")
		}
		fixture.cancel()
		if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || !fixture.store.joined {
			t.Fatal("host recovery did not retain ordinary joined shutdown", err)
		}
	})
}

// A successful job heartbeat cannot extend an expired host admission. No
// second host call may run after its independently confirmed window elapsed.
func TestCompetitionWorkerHostAcquireRetryCannotBorrowFreshJobAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionAcquireWorkerFixture(t)
		originalHostExpiry := server.NowUtc().Add(8 * time.Second)
		fixture.worker.confirmedHostLeaseExpiresAt = originalHostExpiry
		fixture.store.hostHeartbeat = func(ctx context.Context, _ int) error {
			<-ctx.Done()
			return competitionHostAcquireFailure(ctx.Err())
		}
		fixture.Start(t)
		fixture.ticks <- server.NowUtc()
		if err := fixture.Finish(t); !errors.Is(err, ErrLeaseLost) || fixture.store.heartbeatCalls != 1 || fixture.store.hostCalls != 1 || !fixture.store.joined ||
			fixture.store.reason != "host_heartbeat_failed" || !fixture.worker.confirmedHostLeaseExpiresAt.Equal(originalHostExpiry) {
			t.Fatal("fresh job acknowledgement invented a new host window", err)
		}
	})
}

// An unknown host window retains the historical lock-only refresh. A job's
// valid authority alone cannot authenticate another host acquisition attempt.
func TestCompetitionWorkerHostAcquireRetryUnknownHostWindowCallsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionAcquireWorkerFixture(t)
		fixture.store.hostHeartbeat = func(ctx context.Context, _ int) error {
			<-ctx.Done()
			return competitionHostAcquireFailure(ctx.Err())
		}
		fixture.Start(t)
		fixture.ticks <- server.NowUtc()
		if err := fixture.Finish(t); !errors.Is(err, context.DeadlineExceeded) || fixture.store.hostCalls != 1 || !fixture.worker.confirmedHostLeaseExpiresAt.IsZero() || !fixture.store.joined {
			t.Fatal("unknown host window invented acquisition authority", err)
		}
	})
}

// A store's nil return after the owned deadline does not acknowledge fresh host
// eligibility, whether or not the old confirmed lease has also expired.
func TestCompetitionWorkerHostAcquireRetryRejectsLateNilAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionAcquireWorkerFixture(t)
		original := server.NowUtc().Add(time.Minute)
		fixture.worker.confirmedHostLeaseExpiresAt = original
		fixture.store.hostHeartbeat = func(ctx context.Context, _ int) error { <-ctx.Done(); return nil }
		fixture.Start(t)
		fixture.ticks <- server.NowUtc()
		if err := fixture.Finish(t); !errors.Is(err, context.DeadlineExceeded) || fixture.store.hostCalls != 1 || !fixture.worker.confirmedHostLeaseExpiresAt.Equal(original) || !fixture.store.joined {
			t.Fatal("late nil result was adopted as a fresh host acknowledgement", err)
		}
	})
}
