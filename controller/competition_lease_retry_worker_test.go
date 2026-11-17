// Actual worker heartbeat edges run against public Store decorators and
// explicit cleanup barriers. No database, native evaluator or timer race runs.
package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// The public methods deliberately decorate an embedded store, so promoted
// methods cannot bypass the injected heartbeat failures or observations.
type competitionLeaseRetryWorkerStore struct {
	heartbeatFailureStore
	heartbeatCalls, hostCalls int
	heartbeat                 func(context.Context, int) error
	hostHeartbeat             func(context.Context, int) error
}

// One call is one whole public store operation, including its failure result.
func (self *competitionLeaseRetryWorkerStore) Heartbeat(ctx context.Context, _ *Settings, _ string, _ server.Id) error {
	self.heartbeatCalls++
	if self.heartbeat != nil {
		return self.heartbeat(ctx, self.heartbeatCalls)
	}
	return nil
}

// Host metadata does not grant or extend evaluation authority.
func (self *competitionLeaseRetryWorkerStore) RegisterHost(ctx context.Context, _ *Settings, _ HostSelfCheck) error {
	self.hostCalls++
	if self.hostHeartbeat != nil {
		return self.hostHeartbeat(ctx, self.hostCalls)
	}
	return nil
}

// The unchanged parent-stop path must also join candidate cleanup first.
func (self *competitionLeaseRetryWorkerStore) HandBack(ctx context.Context, workerId string, jobId server.Id, reason string) error {
	self.reason, self.joined = reason, self.evaluator.joined.Load()
	return self.fakeStore.HandBack(ctx, workerId, jobId, reason)
}

// Capturing the real evaluator context proves the original deadline survives.
type competitionLeaseRetryEvaluator struct {
	heartbeatFailureEvaluator
	evaluationCtx context.Context
	calls         int
}

// Existing explicit cancellation/release barriers keep cleanup deterministic.
func (self *competitionLeaseRetryEvaluator) Evaluate(ctx context.Context, settings *Settings, job *queuedJob) EvaluationOutcome {
	self.evaluationCtx, self.calls = ctx, self.calls+1
	return self.heartbeatFailureEvaluator.Evaluate(ctx, settings, job)
}

// All identities are generated synthetic values and all execution is in Go.
type competitionLeaseRetryWorkerFixture struct {
	ctx       context.Context
	watchdog  context.Context
	cancel    context.CancelFunc
	clock     *competitionLeaseRetryClock
	worker    *Worker
	store     *competitionLeaseRetryWorkerStore
	evaluator *competitionLeaseRetryEvaluator
	job       *queuedJob
	ticks     chan time.Time
	done      chan error
	release   sync.Once
}

// The timeout is only a deadlock watchdog; tests drive every success/failure
// through a tick, returned store error or acknowledged barrier.
func newCompetitionLeaseRetryWorkerFixture(t *testing.T) *competitionLeaseRetryWorkerFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), 5*time.Second)
	clock := &competitionLeaseRetryClock{now: server.NowUtc()}
	settings := validSettings()
	settings.CompetitionId = "synthetic-lease-retry-competition"
	evaluator := &competitionLeaseRetryEvaluator{heartbeatFailureEvaluator: heartbeatFailureEvaluator{
		started: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}),
	}}
	store := &competitionLeaseRetryWorkerStore{heartbeatFailureStore: heartbeatFailureStore{evaluator: &evaluator.heartbeatFailureEvaluator}}
	worker, err := newWorkerWithImageDigest(settings, store, evaluator, "synthetic-lease-retry-worker", testWorkerImageDigest())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	worker.heartbeatSettings.LeaseRetry.Now = clock.Now
	worker.heartbeatSettings.LeaseRetry.Wait = func(_ context.Context, delay time.Duration) error { clock.Advance(delay); return nil }
	ticks := make(chan time.Time, 1)
	worker.evaluationTickForTest = ticks
	started, expires := clock.Now().Add(-time.Minute), clock.Now().Add(time.Minute)
	fixture := &competitionLeaseRetryWorkerFixture{ctx: ctx, watchdog: watchdog, cancel: cancel, clock: clock, worker: worker, store: store, evaluator: evaluator,
		job:   &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), StartedAt: &started}, AttemptCount: 1, LeaseExpiresAt: &expires},
		ticks: ticks, done: make(chan error, 1)}
	t.Cleanup(func() { cancel(); stopWatchdog(); fixture.release.Do(func() { close(evaluator.release) }) })
	return fixture
}

// The evaluator's start barrier precedes every forced heartbeat edge.
func (self *competitionLeaseRetryWorkerFixture) Start(t *testing.T) {
	t.Helper()
	host := passingHostCheck(self.worker.settings)
	host.RebaselinePassed, host.RebaselineRoundId = true, &self.job.RoundId
	go func() { self.done <- self.worker.evaluateOne(self.ctx, self.job, host) }()
	select {
	case <-self.evaluator.started:
	case <-self.watchdog.Done():
		t.Fatal("worker did not reach the evaluator start barrier")
	}
}

// No terminal handback is permitted before the explicit cleanup release.
func (self *competitionLeaseRetryWorkerFixture) Finish(t *testing.T) error {
	t.Helper()
	select {
	case <-self.evaluator.canceled:
	case <-self.watchdog.Done():
		t.Fatal("worker did not cancel its evaluator")
	}
	self.release.Do(func() { close(self.evaluator.release) })
	select {
	case err := <-self.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not join released cleanup")
		return nil
	}
}

// Transient lock failures keep one evaluator and its original submission-wide
// budget alive. A later heartbeat uses its rolling ack, not the initial expiry.
func TestCompetitionWorkerLeaseRetryRecoversWithoutCancelOrFreshJobBudget(t *testing.T) {
	fixture := newCompetitionLeaseRetryWorkerFixture(t)
	fixture.worker.settings.WorkerLeaseSeconds = 1800
	fixture.worker.settings.HostHeartbeatMaxAgeSeconds = 1800
	initialExpiry := fixture.clock.Now().Add(10 * time.Minute)
	fixture.job.LeaseExpiresAt = &initialExpiry
	startedAt := *fixture.job.StartedAt
	hostAcks := make(chan int, 2)
	fixture.store.heartbeat = func(_ context.Context, call int) error {
		if call%2 == 1 {
			return &pgconn.PgError{Code: "55P03"}
		}
		return nil
	}
	fixture.worker.evaluationHeartbeatForTest = func() { hostAcks <- fixture.store.hostCalls }
	fixture.Start(t)
	for expected := 1; expected <= 2; expected++ {
		fixture.ticks <- fixture.clock.Now()
		select {
		case actual := <-hostAcks:
			if actual != expected || fixture.evaluator.evaluationCtx.Err() != nil {
				t.Fatal("lock retry canceled or changed the live evaluation")
			}
		case <-fixture.evaluator.canceled:
			t.Fatal("transient lock timeout canceled the evaluator instead of retrying")
		case <-fixture.watchdog.Done():
			t.Fatal("heartbeat did not recover through the public Store boundary")
		}
		fixture.clock.Advance(11 * time.Minute)
	}
	deadline, ok := fixture.evaluator.evaluationCtx.Deadline()
	if !ok || !deadline.Equal(startedAt.Add(time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second)) ||
		!fixture.job.StartedAt.Equal(startedAt) || fixture.job.AttemptCount != 1 || fixture.evaluator.calls != 1 || fixture.store.heartbeatCalls != 4 {
		t.Fatal("heartbeat retry refreshed the job budget, claim or evaluator")
	}
	fixture.cancel()
	if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || !fixture.store.joined || fixture.store.reason != "worker_shutdown" {
		t.Fatalf("parent stop lost its joined handback: %v", err)
	}
}

// Exhaustion preserves the original cancel, joined cleanup and safe handback.
func TestCompetitionWorkerLeaseRetryExhaustionCancelsAndJoins(t *testing.T) {
	fixture := newCompetitionLeaseRetryWorkerFixture(t)
	fixture.worker.heartbeatSettings.LeaseRetry.MaximumAttempts = 20
	cause := &pgconn.PgError{Code: "55P03"}
	fixture.store.heartbeat = func(context.Context, int) error { fixture.clock.Advance(4 * time.Second); return cause }
	fixture.Start(t)
	fixture.ticks <- fixture.clock.Now()
	select {
	case <-fixture.evaluator.canceled:
		if fixture.store.diagnosticCalls != 0 {
			t.Fatal("exhaustion handed back before joined cleanup")
		}
	case <-fixture.watchdog.Done():
		t.Fatal("exhausted heartbeat did not cancel evaluation")
	}
	err := fixture.Finish(t)
	if !errors.Is(err, cause) || fixture.store.heartbeatCalls != 2 || fixture.store.hostCalls != 0 || !fixture.store.joined ||
		fixture.store.reason != "heartbeat_failed" || fixture.store.diagnosticCalls != 1 || fixture.store.completedCount() != 0 {
		t.Fatalf("exhausted retry changed cancellation/handback: %v", err)
	}
}

// Cancellation is observed while backoff is held, not after another store call.
func TestCompetitionWorkerLeaseRetryParentCancellationStopsWait(t *testing.T) {
	fixture := newCompetitionLeaseRetryWorkerFixture(t)
	waiting := make(chan struct{})
	fixture.worker.heartbeatSettings.LeaseRetry.Wait = func(ctx context.Context, _ time.Duration) error { close(waiting); <-ctx.Done(); return ctx.Err() }
	fixture.store.heartbeat = func(context.Context, int) error { return &pgconn.PgError{Code: "55P03"} }
	fixture.Start(t)
	fixture.ticks <- fixture.clock.Now()
	select {
	case <-waiting:
		fixture.cancel()
	case <-fixture.watchdog.Done():
		t.Fatal("worker did not reach backoff")
	}
	if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || fixture.store.heartbeatCalls != 1 || !fixture.store.joined {
		t.Fatalf("parent stop dispatched another heartbeat or skipped cleanup: %v", err)
	}
}

// This context publishes the original deadline but expires at an explicit test
// edge; parent cancellation still propagates. It never waits on wall time.
type competitionLeaseRetryDeadlineContext struct {
	context.Context
	deadline  time.Time
	done      chan struct{}
	stateLock sync.Mutex
	err       error
}

// The deadline identity is the actual one computed by evaluateOne.
func (self *competitionLeaseRetryDeadlineContext) Deadline() (time.Time, bool) {
	return self.deadline, true
}

// A distinct channel makes context propagation observe this context's error.
func (self *competitionLeaseRetryDeadlineContext) Done() <-chan struct{} { return self.done }

// Reads pair with the explicit expiry/cancellation publication.
func (self *competitionLeaseRetryDeadlineContext) Err() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.err
}

// At most one stop wins; repeated cleanup cancellation preserves deadline cause.
func (self *competitionLeaseRetryDeadlineContext) Stop(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err == nil {
		self.err = err
		close(self.done)
	}
}

// Scoring stops independently while a metadata backoff remains owned by the
// live parent and its unchanged bounds. Parent cancellation still stops it.
func TestCompetitionWorkerLeaseRetryScoringDeadlineDoesNotCancelMetadataWait(t *testing.T) {
	fixture := newCompetitionLeaseRetryWorkerFixture(t)
	var execution *competitionLeaseRetryDeadlineContext
	fixture.worker.evaluationContextForTest = func(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
		execution = &competitionLeaseRetryDeadlineContext{Context: parent, deadline: deadline, done: make(chan struct{})}
		stopParent := context.AfterFunc(parent, func() { execution.Stop(parent.Err()) })
		return execution, func() { stopParent(); execution.Stop(context.Canceled) }
	}
	waiting := make(chan context.Context, 1)
	fixture.worker.heartbeatSettings.LeaseRetry.Wait = func(ctx context.Context, _ time.Duration) error { waiting <- ctx; <-ctx.Done(); return ctx.Err() }
	fixture.store.heartbeat = func(context.Context, int) error { return &pgconn.PgError{Code: "55P03"} }
	fixture.Start(t)
	fixture.ticks <- fixture.clock.Now()
	select {
	case metadataCtx := <-waiting:
		execution.Stop(context.DeadlineExceeded)
		select {
		case <-fixture.evaluator.canceled:
		case <-fixture.watchdog.Done():
			t.Fatal("scoring did not stop at its original deadline")
		}
		if metadataCtx.Err() != nil || fixture.ctx.Err() != nil || fixture.store.diagnosticCalls != 0 {
			t.Fatal("scoring deadline canceled metadata ownership or manufactured a heartbeat incident")
		}
		fixture.cancel()
	case <-fixture.watchdog.Done():
		t.Fatal("worker did not reach its retry wait")
	}
	if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || execution.Err() != context.DeadlineExceeded || fixture.store.heartbeatCalls != 1 || !fixture.store.joined || fixture.store.reason != "worker_shutdown" {
		t.Fatalf("metadata retry ignored parent cancellation or lost joined scoring cleanup: %v", err)
	}
}

// Host registration may recover but cannot advance the last job-lease ack.
func TestCompetitionWorkerHostLeaseRetryDoesNotExtendJobAuthority(t *testing.T) {
	fixture := newCompetitionLeaseRetryWorkerFixture(t)
	hostAck := make(chan struct{})
	fixture.store.hostHeartbeat = func(_ context.Context, call int) error {
		if call == 1 {
			return &pgconn.PgError{Code: "55P03"}
		}
		return nil
	}
	fixture.worker.evaluationHeartbeatForTest = func() { close(hostAck) }
	fixture.Start(t)
	fixture.ticks <- fixture.clock.Now()
	select {
	case <-hostAck:
	case <-fixture.watchdog.Done():
		t.Fatal("host metadata did not recover")
	}
	policy := fixture.worker.heartbeatSettings.LeaseRetry
	fixture.clock.Advance(time.Duration(fixture.worker.settings.WorkerLeaseSeconds)*time.Second - policy.Interval/2)
	fixture.ticks <- fixture.clock.Now()
	err := fixture.Finish(t)
	if !errors.Is(err, ErrLeaseLost) || fixture.store.heartbeatCalls != 1 || fixture.store.hostCalls != 2 || !fixture.store.joined {
		t.Fatalf("host registration extended expired job authority: %v", err)
	}
}

// An absent initial expiry cannot manufacture authority for a second call.
func TestCompetitionWorkerLeaseRetryUnknownInitialExpiryCallsOnce(t *testing.T) {
	fixture := newCompetitionLeaseRetryWorkerFixture(t)
	fixture.job.LeaseExpiresAt = nil
	cause := &pgconn.PgError{Code: "55P03"}
	fixture.store.heartbeat = func(context.Context, int) error { return cause }
	fixture.worker.heartbeatSettings.LeaseRetry.Wait = func(context.Context, time.Duration) error { t.Fatal("unknown lease was retried"); return nil }
	fixture.Start(t)
	fixture.ticks <- fixture.clock.Now()
	if err := fixture.Finish(t); !errors.Is(err, cause) || fixture.store.heartbeatCalls != 1 || !fixture.store.joined {
		t.Fatalf("unknown lease changed first-call failure semantics: %v", err)
	}
}

// An expired known claim cannot dispatch even its first heartbeat operation.
func TestCompetitionWorkerLeaseRetryExpiredInitialAuthorityCallsNoStore(t *testing.T) {
	fixture := newCompetitionLeaseRetryWorkerFixture(t)
	expired := fixture.clock.Now()
	fixture.job.LeaseExpiresAt = &expired
	fixture.Start(t)
	fixture.ticks <- fixture.clock.Now()
	if err := fixture.Finish(t); !errors.Is(err, ErrLeaseLost) || fixture.store.heartbeatCalls != 0 || fixture.store.hostCalls != 0 || !fixture.store.joined {
		t.Fatalf("expired initial authority reached a store operation: %v", err)
	}
}
