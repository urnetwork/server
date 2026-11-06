// Natural scoring expiry retains one evaluator's joined outcome while its
// unchanged ownership metadata remains fenced. No provider or database runs.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Public store decorators keep actual dispatches distinct from retry-adapter
// returns; the optional handback records only the existing safe private DTO.
type competitionEvaluationBudgetStore struct {
	*competitionCompletionStore
	heartbeatCalls, hostCalls int
	heartbeat, hostHeartbeat  func(context.Context, int) error
	reason                    string
	failure                   competitionHeartbeatFailure
	diagnosticCalls           int
}

// Counting occurs only after the worker really enters the public store method.
func (self *competitionEvaluationBudgetStore) Heartbeat(ctx context.Context, _ *Settings, _ string, _ server.Id) error {
	self.heartbeatCalls++
	if self.heartbeat != nil {
		return self.heartbeat(ctx, self.heartbeatCalls)
	}
	return nil
}

// Host renewal has its own independently counted public boundary.
func (self *competitionEvaluationBudgetStore) RegisterHost(ctx context.Context, _ *Settings, _ HostSelfCheck) error {
	self.hostCalls++
	if self.hostHeartbeat != nil {
		return self.hostHeartbeat(ctx, self.hostCalls)
	}
	return nil
}

// Ordinary cancellation and integrity loss retain their existing reason.
func (self *competitionEvaluationBudgetStore) HandBack(ctx context.Context, workerId string, jobId server.Id, reason string) error {
	self.reason = reason
	return self.fakeStore.HandBack(ctx, workerId, jobId, reason)
}

// The synthetic ledger can distinguish a real failed heartbeat from shutdown.
func (self *competitionEvaluationBudgetStore) handBackWithFailure(ctx context.Context, workerId string, jobId server.Id, reason string, failure competitionHeartbeatFailure) error {
	self.diagnosticCalls++
	self.failure = failure
	return self.HandBack(ctx, workerId, jobId, reason)
}

// Explicit barriers own every phase, including cleanup and final persistence.
type competitionEvaluationBudgetFixture struct {
	*competitionCompletionFixture
	ctx                        context.Context
	cancel                     context.CancelCauseFunc
	store                      *competitionEvaluationBudgetStore
	execution                  *competitionLeaseRetryDeadlineContext
	contexts                   chan *competitionLeaseRetryDeadlineContext
	started, canceled, release chan struct{}
	heartbeatAcks              chan struct{}
	ticks                      chan time.Time
	done                       chan error
	workerJoined               chan struct{}
	workerStarted              bool
	joined                     atomic.Bool
	releaseOnce                sync.Once
}

// The inherited exact deadline is stopped at an explicit edge; lease and
// completion policy use the real clock (virtual inside a synctest bubble).
func newCompetitionEvaluationBudgetFixture(t *testing.T) *competitionEvaluationBudgetFixture {
	t.Helper()
	base := newCompetitionCompletionFixture(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	fixture := &competitionEvaluationBudgetFixture{
		competitionCompletionFixture: base, ctx: ctx, cancel: cancel,
		store:    &competitionEvaluationBudgetStore{competitionCompletionStore: base.store},
		contexts: make(chan *competitionLeaseRetryDeadlineContext, 1), started: make(chan struct{}),
		canceled: make(chan struct{}), release: make(chan struct{}), heartbeatAcks: make(chan struct{}, 8), ticks: make(chan time.Time), done: make(chan error, 1), workerJoined: make(chan struct{}),
	}
	base.worker.store = fixture.store
	base.worker.heartbeatSettings = defaultCompetitionHeartbeatSettings()
	base.worker.evaluationTickForTest = fixture.ticks
	base.worker.evaluationHeartbeatForTest = func() { fixture.heartbeatAcks <- struct{}{} }
	base.worker.evaluationContextForTest = func(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
		execution := &competitionLeaseRetryDeadlineContext{Context: parent, deadline: deadline, done: make(chan struct{})}
		stopParent := context.AfterFunc(parent, func() { execution.Stop(parent.Err()) })
		fixture.contexts <- execution
		return execution, func() { stopParent(); execution.Stop(context.Canceled) }
	}
	base.evaluator.evaluate = func(ctx context.Context) EvaluationOutcome {
		close(fixture.started)
		<-ctx.Done()
		close(fixture.canceled)
		<-fixture.release
		fixture.joined.Store(true)
		return base.evaluator.outcome
	}
	base.store.complete = func(ctx context.Context, _ int, _ EvaluationOutcome) (bool, error) {
		if !fixture.joined.Load() || ctx.Err() != nil {
			t.Error("completion preceded joined cleanup or reused a canceled scoring context")
			return false, ErrLeaseLost
		}
		return false, nil
	}
	t.Cleanup(func() {
		cancel(context.Canceled)
		fixture.releaseOnce.Do(func() { close(fixture.release) })
		if fixture.workerStarted {
			cleanup, stopCleanup := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
			defer stopCleanup()
			select {
			case <-fixture.workerJoined:
			case <-cleanup.Done():
				t.Error("fixture did not join its worker after release and cancellation")
			}
		}
	})
	return fixture
}

// The start barriers publish the exact evaluation context before any tick.
func (self *competitionEvaluationBudgetFixture) Start(t *testing.T) {
	t.Helper()
	self.workerStarted = true
	go func() {
		defer close(self.workerJoined)
		self.done <- self.worker.evaluateOne(self.ctx, self.job, self.host)
	}()
	select {
	case self.execution = <-self.contexts:
	case <-time.After(5 * time.Minute):
		t.Fatal("worker did not create its original evaluation context")
	}
	select {
	case <-self.started:
	case <-time.After(5 * time.Minute):
		t.Fatal("evaluator did not start")
	}
}

// A held store or observation boundary is released even after a fatal check,
// before the owning fixture's cleanup waits for its worker to join.
func newCompetitionEvaluationBudgetBarrier(t *testing.T) (chan struct{}, func()) {
	t.Helper()
	barrier := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(barrier) }) }
	t.Cleanup(release)
	return barrier, release
}

// The parent remains live, and canceled scoring cannot execute another phase.
func (self *competitionEvaluationBudgetFixture) Expire(t *testing.T) {
	t.Helper()
	self.execution.Stop(context.DeadlineExceeded)
	select {
	case <-self.canceled:
	case <-time.After(5 * time.Minute):
		t.Fatal("evaluator did not observe the original scoring expiry")
	}
	if self.ctx.Err() != nil {
		t.Fatal("scoring expiry canceled the ownership parent")
	}
}

// An unbuffered tick is an acknowledged selection, never a scheduling guess.
func (self *competitionEvaluationBudgetFixture) Tick(t *testing.T) {
	t.Helper()
	select {
	case self.ticks <- server.NowUtc():
	case err := <-self.done:
		t.Fatal("worker stopped before the requested heartbeat edge", err)
	case <-time.After(5 * time.Minute):
		t.Fatal("worker did not select the requested heartbeat edge")
	}
}

// A completed cycle has returned both actual store calls and the host ACK.
func (self *competitionEvaluationBudgetFixture) TickAndWait(t *testing.T) {
	t.Helper()
	self.Tick(t)
	select {
	case <-self.heartbeatAcks:
	case err := <-self.done:
		t.Fatal("ownership heartbeat did not finish", err)
	case <-time.After(5 * time.Minute):
		t.Fatal("ownership heartbeat did not acknowledge its completed cycle")
	}
}

// Completion or handback is observed only after the held evaluator joins.
func (self *competitionEvaluationBudgetFixture) Finish(t *testing.T) error {
	t.Helper()
	self.releaseOnce.Do(func() { close(self.release) })
	select {
	case err := <-self.done:
		return err
	case <-time.After(5 * time.Minute):
		t.Fatal("worker did not join released cleanup")
		return nil
	}
}

// A natural deadline is terminal, with no retry, fabricated success or recovery
// producer. Any original archived bytes and private failure facts stay intact.
func requireCompetitionEvaluationBudgetOutcome(t *testing.T, fixture *competitionEvaluationBudgetFixture, wanted EvaluationOutcome) {
	t.Helper()
	outcomes := fixture.store.outcomes()
	if len(outcomes) != 1 || outcomes[0].Score != nil || outcomes[0].Error == nil ||
		outcomes[0].Error.Code != "evaluation_time_budget_exhausted" || outcomes[0].Error.Retriable ||
		!bytes.Equal(outcomes[0].ArtifactManifest, wanted.ArtifactManifest) ||
		!bytes.Equal(outcomes[0].RoundBaselineJson, wanted.RoundBaselineJson) || outcomes[0].RoundBaselineSha256 != wanted.RoundBaselineSha256 ||
		!reflect.DeepEqual(outcomes[0].archiveFailure, wanted.archiveFailure) ||
		fixture.store.handbacks != 0 || fixture.store.diagnosticCalls != 0 || fixture.store.claims != 0 || fixture.evaluator.calls != 1 {
		t.Fatal("natural budget expiry lost evidence, gained score/retry authority or became a heartbeat recovery producer")
	}
}

// The actual failure-archive authenticator and bounded detached archive run
// inside the same evaluator. Renewals cross the old claim expiry, not scoring.
func TestCompetitionWorkerEvaluationBudgetPreservesAuthenticatedArchive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionEvaluationBudgetFixture(t)
		fixture.job.Patch = []byte("synthetic canonical budget patch\n")
		patchDigest := sha256.Sum256(fixture.job.Patch)
		fixture.job.PatchSha256 = hex.EncodeToString(patchDigest[:])
		directory := failureEvidenceTestTempDir(t)
		writeArchiveTestFile(t, directory, "canonical.patch", fixture.job.Patch)
		writeArchiveTestFile(t, directory, "worker.stderr.log", []byte("synthetic retained cleanup evidence\n"))
		archiveEntered := make(chan struct{})
		archive := &failureArchiveTestArchive{onArchive: func(ctx context.Context, _ string, _ artifactManifest) {
			close(archiveEntered)
			<-fixture.release
			if ctx.Err() != nil {
				t.Error("ownership cleanup restarted or expired the original bounded archive")
			}
		}}
		fixture.worker.settings.artifactArchive = archive
		originalFailure := infrastructureError("evaluator_process_failed", "synthetic command canceled at its scoring deadline")
		fixture.evaluator.evaluate = func(ctx context.Context) EvaluationOutcome {
			close(fixture.started)
			<-ctx.Done()
			close(fixture.canceled)
			outcome := archiveFailedEvaluation(ctx, fixture.worker.settings, fixture.job, directory, hex.EncodeToString(patchDigest[:]), originalFailure, false)
			fixture.evaluator.outcome = outcome
			fixture.joined.Store(true)
			return outcome
		}
		started := server.NowUtc().Add(-time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second + time.Second)
		fixture.job.StartedAt = &started
		expires := server.NowUtc().Add(30 * time.Second)
		fixture.job.LeaseExpiresAt = &expires
		fixture.worker.confirmedHostLeaseExpiresAt = expires
		fixture.store.heartbeat = func(ctx context.Context, _ int) error {
			if ctx.Err() != nil || fixture.execution.Err() != context.DeadlineExceeded {
				t.Error("metadata renewal reused scoring authority")
			}
			return nil
		}
		fixture.store.hostHeartbeat = fixture.store.heartbeat
		fixture.Start(t)
		time.Sleep(time.Second)
		fixture.Expire(t)
		<-archiveEntered
		archiveDeadline := archive.deadline
		for range 5 {
			time.Sleep(20 * time.Second)
			fixture.TickAndWait(t)
			if archive.calls != 1 || !archive.deadline.Equal(archiveDeadline) || fixture.joined.Load() || fixture.store.completedCount() != 0 {
				t.Fatal("ownership renewal restarted cleanup or completed before retained evidence joined")
			}
		}
		if !server.NowUtc().After(expires) || fixture.execution.Err() != context.DeadlineExceeded {
			t.Fatal("fixture did not cross the old claim expiry with scoring still stopped")
		}
		if err := fixture.Finish(t); err != nil {
			t.Fatal("same-attempt archive completion failed", err)
		}
		requireCompetitionEvaluationBudgetOutcome(t, fixture, fixture.evaluator.outcome)
		if archived := failureArchiveOutcomeFailure(t, fixture.store.outcomes()[0]); archived == nil || *archived != *originalFailure || archive.calls != 1 || archive.contextErr != nil {
			t.Fatal("budget terminalization rewrote or lost the authenticated original-cause archive")
		}
	})
}

// A failed retention proof remains failed evidence, not synthetic success or a
// heartbeat incident, even when the budget is the primary terminal code.
func TestCompetitionWorkerEvaluationBudgetPreservesArchiveFailure(t *testing.T) {
	fixture := newCompetitionEvaluationBudgetFixture(t)
	cause := errors.New("synthetic private retention failure")
	fixture.evaluator.outcome = EvaluationOutcome{
		Error:          infrastructureError("artifact_archive_failed", "synthetic archive failure"),
		archiveFailure: competitionArtifactArchiveFailureDetails("archive_attempt", cause),
	}
	wanted := fixture.evaluator.outcome
	fixture.Start(t)
	fixture.Expire(t)
	fixture.TickAndWait(t)
	if err := fixture.Finish(t); err != nil {
		t.Fatal(err)
	}
	requireCompetitionEvaluationBudgetOutcome(t, fixture, wanted)
}

// The done branch has already won when both a deadline and a heartbeat become
// ready. The result cannot escape the same natural-budget policy by selection.
func TestCompetitionWorkerEvaluationBudgetDoneSelectionRejectsLateScore(t *testing.T) {
	fixture := newCompetitionEvaluationBudgetFixture(t)
	wanted := fixture.evaluator.outcome
	fixture.evaluator.evaluate = func(context.Context) EvaluationOutcome {
		close(fixture.started)
		fixture.joined.Store(true)
		return wanted
	}
	ticks := make(chan time.Time, 1)
	fixture.worker.evaluationTickForTest = ticks
	fixture.worker.evaluationResultForTest = func() {
		execution := <-fixture.contexts
		execution.Stop(context.DeadlineExceeded)
		ticks <- server.NowUtc()
	}
	if err := fixture.worker.evaluateOne(fixture.ctx, fixture.job, fixture.host); err != nil {
		t.Fatal(err)
	}
	requireCompetitionEvaluationBudgetOutcome(t, fixture, wanted)
	if fixture.store.heartbeatCalls != 0 || fixture.store.hostCalls != 0 || wanted.Score == nil {
		t.Fatal("selected late result dispatched another heartbeat or mutated its original scored outcome")
	}
}

// A result selected just before expiry cannot retain scoring authority merely
// because validation or scheduling crosses the deadline before persistence.
func TestCompetitionWorkerEvaluationBudgetRechecksDeadlineBeforeCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionEvaluationBudgetFixture(t)
		wanted := fixture.evaluator.outcome
		started := server.NowUtc().Add(-time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second + time.Second)
		fixture.job.StartedAt = &started
		fixture.evaluator.evaluate = func(context.Context) EvaluationOutcome {
			fixture.joined.Store(true)
			return wanted
		}
		fixture.worker.evaluationCompletionForTest = func() { time.Sleep(time.Second) }
		if err := fixture.worker.evaluateOne(fixture.ctx, fixture.job, fixture.host); err != nil {
			t.Fatal(err)
		}
		requireCompetitionEvaluationBudgetOutcome(t, fixture, wanted)
	})
}

// An invalid score never erases independently retained evidence, baseline or
// coordinator facts merely because its primary terminal error is replaced.
func TestCompetitionWorkerEvaluationBudgetInvalidScorePreservesEvidence(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	fixture.evaluator.outcome.Score = &ScoreResult{}
	fixture.evaluator.outcome.archiveFailure = competitionArtifactArchiveFailureDetails("archive_attempt", errors.New("synthetic private archive fact"))
	fixture.evaluator.outcome.distributedArchiveVerified = true
	wanted := fixture.evaluator.outcome
	if err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host); err != nil {
		t.Fatal(err)
	}
	outcomes := fixture.store.outcomes()
	if len(outcomes) != 1 || outcomes[0].Score != nil || outcomes[0].Error == nil || outcomes[0].Error.Code != "score_result_invalid" || outcomes[0].Error.Retriable ||
		!bytes.Equal(outcomes[0].ArtifactManifest, wanted.ArtifactManifest) || !bytes.Equal(outcomes[0].RoundBaselineJson, wanted.RoundBaselineJson) || outcomes[0].RoundBaselineSha256 != wanted.RoundBaselineSha256 ||
		!reflect.DeepEqual(outcomes[0].archiveFailure, wanted.archiveFailure) || !outcomes[0].distributedArchiveVerified || fixture.evaluator.outcome.Score == nil ||
		fixture.store.handbacks != 0 || fixture.evaluator.calls != 1 {
		t.Fatal("invalid-score normalization erased retained evidence or mutated the original outcome")
	}
}

// Scoring expiry does not extend metadata's own attempt deadline. A nil reply
// after that deadline remains ambiguous and cannot become an adopted ACK.
func TestCompetitionWorkerEvaluationBudgetMetadataLateNilDoesNotAdoptAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionEvaluationBudgetFixture(t)
		entered, expired := make(chan struct{}), make(chan struct{})
		fixture.store.heartbeat = func(ctx context.Context, _ int) error {
			close(entered)
			<-ctx.Done()
			close(expired)
			return nil
		}
		fixture.Start(t)
		fixture.Tick(t)
		<-entered
		fixture.Expire(t)
		<-expired
		if err := fixture.Finish(t); !errors.Is(err, context.DeadlineExceeded) || fixture.store.heartbeatCalls != 1 || fixture.store.hostCalls != 0 ||
			fixture.store.reason != "heartbeat_failed" || fixture.store.failure.Category != "deadline_exceeded" || fixture.store.completedCount() != 0 {
			t.Fatal("metadata-own late nil adopted a rejected ACK or became synthetic success", err)
		}
		owner := fixture.store.failure.HeartbeatOwner
		if owner == nil || owner.StoreCalls != 1 || owner.LastStoreOutcome != competitionHeartbeatStoreSucceeded || owner.Boundary != competitionHeartbeatOwnerAttemptAfterSuccess {
			t.Fatal("metadata-own late success lost its actual return boundary")
		}
	})
}

// The RPC begins one millisecond before scoring ends, then succeeds after it
// within the original metadata allowance. Only scoring stops, with no replay.
func TestCompetitionWorkerEvaluationBudgetHealthyRpcSpansScoreDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionEvaluationBudgetFixture(t)
		wanted := fixture.evaluator.outcome
		started := server.NowUtc().Add(-time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second + time.Second)
		fixture.job.StartedAt = &started
		originalDeadline := started.Add(time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
		entered := make(chan struct{})
		returned, releaseStore := newCompetitionEvaluationBudgetBarrier(t)
		fixture.store.heartbeat = func(ctx context.Context, _ int) error {
			deadline, present := ctx.Deadline()
			if !present || !deadline.After(originalDeadline) || ctx.Err() != nil {
				t.Error("healthy metadata RPC inherited the almost-expired scoring deadline")
			}
			close(entered)
			<-returned
			if ctx.Err() != nil {
				t.Error("scoring expiry canceled a healthy in-flight metadata RPC")
			}
			return nil
		}
		fixture.Start(t)
		time.Sleep(time.Second - time.Millisecond)
		fixture.Tick(t)
		<-entered
		time.Sleep(2 * time.Millisecond)
		fixture.Expire(t)
		releaseStore()
		<-fixture.heartbeatAcks
		if err := fixture.Finish(t); err != nil {
			t.Fatal("healthy metadata became a scoring-caused heartbeat incident", err)
		}
		requireCompetitionEvaluationBudgetOutcome(t, fixture, wanted)
		if fixture.store.heartbeatCalls != 1 || fixture.store.hostCalls != 1 ||
			!fixture.execution.deadline.Equal(originalDeadline) || !fixture.job.StartedAt.Equal(started) || fixture.job.AttemptCount != 1 {
			t.Fatal("healthy metadata renewal changed scoring time, evaluator or claim")
		}
	})
}

// A real failed operation began under live scoring authority. Natural expiry
// during that operation cannot replace its PostgreSQL cause with a bare timeout.
func TestCompetitionWorkerEvaluationBudgetPreservesLiveStoreFailure(t *testing.T) {
	fixture := newCompetitionEvaluationBudgetFixture(t)
	entered := make(chan struct{})
	returned, releaseStore := newCompetitionEvaluationBudgetBarrier(t)
	cause := &pgconn.PgError{Code: "08006", Message: "synthetic private heartbeat transport"}
	fixture.store.heartbeat = func(ctx context.Context, _ int) error {
		if ctx.Err() != nil {
			t.Error("real failure was not dispatched while scoring was live")
		}
		close(entered)
		<-returned
		return cause
	}
	fixture.Start(t)
	fixture.Tick(t)
	<-entered
	fixture.Expire(t)
	releaseStore()
	if err := fixture.Finish(t); !errors.Is(err, cause) || fixture.store.heartbeatCalls != 1 || fixture.store.hostCalls != 0 ||
		fixture.store.reason != "heartbeat_failed" || fixture.store.failure.Category != "postgresql" || fixture.store.failure.SqlState != "08006" ||
		fixture.store.diagnosticCalls != 1 || fixture.store.completedCount() != 0 {
		t.Fatal("natural deadline erased a real predeadline heartbeat failure", err)
	}
}

// The adjacent host call has the same distinction, without borrowing the job's
// successful ACK as evidence that the failing host registration was successful.
func TestCompetitionWorkerEvaluationBudgetPreservesLiveHostFailure(t *testing.T) {
	fixture := newCompetitionEvaluationBudgetFixture(t)
	entered := make(chan struct{})
	returned, releaseStore := newCompetitionEvaluationBudgetBarrier(t)
	cause := &pgconn.PgError{Code: "08006", Message: "synthetic private host transport"}
	fixture.store.hostHeartbeat = func(context.Context, int) error { close(entered); <-returned; return cause }
	fixture.Start(t)
	fixture.Tick(t)
	<-entered
	fixture.Expire(t)
	releaseStore()
	if err := fixture.Finish(t); !errors.Is(err, cause) || fixture.store.heartbeatCalls != 1 || fixture.store.hostCalls != 1 ||
		fixture.store.reason != "host_heartbeat_failed" || fixture.store.failure.Category != "postgresql" || fixture.store.completedCount() != 0 {
		t.Fatal("natural deadline erased or misattributed a live host failure", err)
	}
}

// Canceling or fencing the live parent while cleanup is held withdraws final
// publication authority; natural budget cannot turn shutdown into completion.
func TestCompetitionWorkerEvaluationBudgetParentCancellationWinsAfterJoin(t *testing.T) {
	for _, cause := range []error{context.Canceled, ErrLeaseLost} {
		fixture := newCompetitionEvaluationBudgetFixture(t)
		fixture.Start(t)
		fixture.Expire(t)
		fixture.cancel(cause)
		if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || fixture.store.reason != "worker_shutdown" ||
			fixture.store.completedCount() != 0 || fixture.store.handbacks != 1 || !fixture.joined.Load() {
			t.Fatal("parent cancellation completed a timed-out evaluation or skipped joined cleanup", err)
		}
		if cause == ErrLeaseLost && fixture.store.failure.Category != "lease_lost" || cause == context.Canceled && fixture.store.diagnosticCalls != 0 {
			t.Fatal("shutdown lost its fencing cause or manufactured an ordinary cancellation incident")
		}
	}
}

// Force a selected result to race parent cancellation in singleton mode too;
// detached metadata retry must not resurrect explicitly withdrawn authority.
func TestCompetitionWorkerEvaluationBudgetParentCancellationWinsSelectedResult(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture.worker.evaluationResultForTest = cancel
	if err := fixture.worker.evaluateOne(ctx, fixture.job, fixture.host); !errors.Is(err, context.Canceled) ||
		fixture.store.calls != 0 || fixture.store.handbacks != 1 || fixture.evaluator.calls != 1 {
		t.Fatal("selected singleton result escaped parent cancellation", err)
	}
}

// A genuine cleanup ownership failure stays fenced; this phase is not a
// blanket permission to ignore host/job authority in order to retain a result.
func TestCompetitionWorkerEvaluationBudgetCleanupFailureStaysFenced(t *testing.T) {
	fixture := newCompetitionEvaluationBudgetFixture(t)
	fixture.store.heartbeat = func(ctx context.Context, _ int) error {
		if ctx.Err() != nil {
			t.Error("cleanup used an expired scoring context")
		}
		return ErrLeaseLost
	}
	fixture.Start(t)
	fixture.Expire(t)
	fixture.Tick(t)
	if err := fixture.Finish(t); !errors.Is(err, ErrLeaseLost) || fixture.store.heartbeatCalls != 1 || fixture.store.hostCalls != 0 ||
		fixture.store.failure.Category != "lease_lost" || fixture.store.completedCount() != 0 || !fixture.joined.Load() {
		t.Fatal("budget cleanup bypassed actual lost ownership", err)
	}
}

// The test holds the selected cleanup edge, queues the completed evaluator
// result, then releases the exact ready-result probe. No select luck is used.
func competitionEvaluationBudgetReadyProbeControl(t *testing.T, host, integrity bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionEvaluationBudgetFixture(t)
		if integrity {
			settings := fixture.worker.settings
			settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
			settings.distributedRoundEpoch = 2
			approvedHost := settings.DistributedEvaluationPolicy.Hosts[0]
			settings.executorHostId, settings.executorSlotId = approvedHost.HostId, approvedHost.SlotId
			fixture.job.ExecutorHostId, fixture.job.SlotId, fixture.job.SlotFencingToken = approvedHost.HostId, approvedHost.SlotId, 4
			fixture.job.executorSettings = settings
			fixture.evaluator.outcome.distributedIntegrityRejected = true
		}
		wanted := fixture.evaluator.outcome
		probeEntered := make(chan struct{})
		probeRelease, releaseProbe := newCompetitionEvaluationBudgetBarrier(t)
		fixture.worker.evaluationCleanupForTest = func(selectedHost bool) {
			if selectedHost == host {
				close(probeEntered)
				<-probeRelease
			}
		}
		fixture.store.heartbeat = func(context.Context, int) error {
			if !host {
				t.Error("already-ready evaluator result was discarded by unnecessary job I/O")
				return ErrLeaseLost
			}
			return nil
		}
		fixture.store.hostHeartbeat = func(context.Context, int) error {
			t.Error("already-ready evaluator result was discarded by unnecessary host I/O")
			return ErrLeaseLost
		}
		fixture.Start(t)
		fixture.Expire(t)
		fixture.Tick(t)
		<-probeEntered
		fixture.releaseOnce.Do(func() { close(fixture.release) })
		synctest.Wait()
		releaseProbe()
		err := fixture.Finish(t)
		if integrity {
			if !errors.Is(err, errCompetitionExecutorIntegrityRejected) || fixture.store.reason != "executor_integrity_rejected" || fixture.store.completedCount() != 0 || fixture.store.diagnosticCalls != 0 {
				t.Fatal("natural budget erased distributed integrity rejection", err)
			}
		} else {
			if err != nil {
				t.Fatal("ready retained result lost to another cleanup operation", err)
			}
			requireCompetitionEvaluationBudgetOutcome(t, fixture, wanted)
		}
		wantedJobCalls := 0
		if host {
			wantedJobCalls = 1
		}
		if fixture.store.heartbeatCalls != wantedJobCalls || fixture.store.hostCalls != 0 {
			t.Fatal("cleanup issued SQL after observing an already-ready evaluator result")
		}
	})
}

// The heartbeat tick won first, but a now-ready retained outcome wins the
// explicit observation boundary before the next job metadata dispatch.
func TestCompetitionWorkerEvaluationBudgetReadyResultPrecedesJobCleanup(t *testing.T) {
	competitionEvaluationBudgetReadyProbeControl(t, false, false)
}

// The evaluator can finish during job renewal; no unrelated host failure may
// discard that ready outcome before its terminal persistence.
func TestCompetitionWorkerEvaluationBudgetReadyResultPrecedesHostCleanup(t *testing.T) {
	competitionEvaluationBudgetReadyProbeControl(t, true, false)
}

// An integrity-rejected ready result retains its separate fail-closed custody,
// rather than becoming an unauthenticated ordinary budget result.
func TestCompetitionWorkerEvaluationBudgetReadyIntegrityResultStaysRejected(t *testing.T) {
	competitionEvaluationBudgetReadyProbeControl(t, false, true)
}

// A result may arrive after a rolled-back retryable cleanup operation. Every
// actual retry dispatch must probe again, not only the outer heartbeat cycle.
func TestCompetitionWorkerEvaluationBudgetReadyResultStopsCleanupRetryDispatch(t *testing.T) {
	for _, host := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			fixture := newCompetitionEvaluationBudgetFixture(t)
			wanted := fixture.evaluator.outcome
			waiting := make(chan struct{})
			waitRelease, releaseWait := newCompetitionEvaluationBudgetBarrier(t)
			fixture.worker.heartbeatSettings.LeaseRetry.Wait = func(ctx context.Context, _ time.Duration) error {
				close(waiting)
				select {
				case <-waitRelease:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			failFirst := func(_ context.Context, call int) error {
				if call != 1 {
					t.Error("ready evaluator result did not withdraw a retry's actual store dispatch")
					return ErrLeaseLost
				}
				return &pgconn.PgError{Code: "55P03"}
			}
			if host {
				fixture.store.hostHeartbeat = failFirst
			} else {
				fixture.store.heartbeat = failFirst
			}
			fixture.Start(t)
			fixture.Expire(t)
			fixture.Tick(t)
			<-waiting
			fixture.releaseOnce.Do(func() { close(fixture.release) })
			synctest.Wait()
			releaseWait()
			if err := fixture.Finish(t); err != nil {
				t.Fatal("finished archive was lost to another cleanup retry", err)
			}
			requireCompetitionEvaluationBudgetOutcome(t, fixture, wanted)
			wantedHostCalls := 0
			if host {
				wantedHostCalls = 1
			}
			if fixture.store.heartbeatCalls != 1 || fixture.store.hostCalls != wantedHostCalls {
				t.Fatal("cleanup retried public I/O after observing its finished evaluator")
			}
		})
	}
}

// A cancellation forced inside the probe seam must be seen before any actual
// store dispatch, even when no evaluator result has become ready yet.
func TestCompetitionWorkerEvaluationBudgetParentLossAtProbePreventsSql(t *testing.T) {
	fixture := newCompetitionEvaluationBudgetFixture(t)
	fixture.worker.evaluationCleanupForTest = func(bool) { fixture.cancel(ErrLeaseLost) }
	fixture.Start(t)
	fixture.Expire(t)
	fixture.Tick(t)
	if err := fixture.Finish(t); !errors.Is(err, context.Canceled) || fixture.store.heartbeatCalls != 0 || fixture.store.hostCalls != 0 ||
		fixture.store.reason != "worker_shutdown" || fixture.store.failure.Category != "lease_lost" || fixture.store.completedCount() != 0 {
		t.Fatal("parent loss at cleanup probe dispatched SQL or lost its fencing cause", err)
	}
}

// Parent loss after normalization still wins immediately before detached
// metadata persistence; the canceled scoring child cannot mask that withdrawal.
func TestCompetitionWorkerEvaluationBudgetParentLossBeforeCompletionPreventsSql(t *testing.T) {
	fixture := newCompetitionEvaluationBudgetFixture(t)
	fixture.evaluator.evaluate = func(context.Context) EvaluationOutcome {
		fixture.joined.Store(true)
		return fixture.evaluator.outcome
	}
	fixture.worker.evaluationResultForTest = func() { (<-fixture.contexts).Stop(context.DeadlineExceeded) }
	fixture.worker.evaluationCompletionForTest = func() { fixture.cancel(ErrLeaseLost) }
	if err := fixture.worker.evaluateOne(fixture.ctx, fixture.job, fixture.host); !errors.Is(err, context.Canceled) ||
		fixture.store.calls != 0 || fixture.store.handbacks != 1 || fixture.store.reason != "worker_shutdown" || fixture.store.failure.Category != "lease_lost" {
		t.Fatal("normalized result entered detached completion after parent fencing", err)
	}
}

// Normalization retains, but never creates, the coordinator verification bit.
// The fake store checks worker behavior only, not distributed SQL admission.
func TestCompetitionWorkerEvaluationBudgetCannotMintDistributedArchiveProof(t *testing.T) {
	for _, verified := range []bool{false, true} {
		fixture := newCompetitionEvaluationBudgetFixture(t)
		settings := fixture.worker.settings
		settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
		settings.distributedRoundEpoch = 2
		host := settings.DistributedEvaluationPolicy.Hosts[0]
		settings.executorHostId, settings.executorSlotId = host.HostId, host.SlotId
		fixture.job.ExecutorHostId, fixture.job.SlotId, fixture.job.SlotFencingToken = host.HostId, host.SlotId, 4
		fixture.job.executorSettings = settings
		fixture.evaluator.outcome.distributedArchiveVerified = verified
		wanted := fixture.evaluator.outcome
		fixture.Start(t)
		fixture.Expire(t)
		if err := fixture.Finish(t); err != nil {
			t.Fatal(err)
		}
		requireCompetitionEvaluationBudgetOutcome(t, fixture, wanted)
		outcome := fixture.store.outcomes()[0]
		if outcome.distributedArchiveVerified != verified || outcome.distributedIntegrityRejected {
			t.Fatal("budget cleanup minted or erased authenticated distributed flags")
		}
	}
}
