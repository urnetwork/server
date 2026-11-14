// Genuine worker-loop barriers exercise independent idle ownership with only
// synthetic stores, authenticated-report shapes and contexts; no native/DB I/O.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// Registration events publish exact acknowledged bytes and the callback edge.
type competitionIdleTestRegistration struct {
	Check    HostSelfCheck
	At       time.Time
	Canceled bool
}

// All callbacks are installed before start. Counters/clock are independently
// atomic; channels publish observations without timing or unsynchronized slices.
type competitionIdleTestStore struct {
	Store
	clock         *atomic.Int64
	registrations chan competitionIdleTestRegistration
	registerCalls atomic.Int64
	claimCalls    atomic.Int64
	onRegister    func(context.Context, int, HostSelfCheck) error
	onClaim       func(context.Context) (*queuedJob, error)
	onRound       func(context.Context) (*roundRecord, error)
	onRuntime     func(context.Context) (*roundRecord, error)
	onReserve     func(context.Context) (*roundBaselinePreparation, error)
	onRelease     func(context.Context) error
	baseline      roundBaselinePreparationStore
}

// The public registration route still receives the actual fresh report.
func (self *competitionIdleTestStore) RegisterHost(ctx context.Context, _ *Settings, check HostSelfCheck) error {
	call := int(self.registerCalls.Add(1))
	if self.onRegister != nil {
		if err := self.onRegister(ctx, call, check); err != nil {
			return err
		}
	}
	self.registrations <- competitionIdleTestRegistration{Check: check, At: time.Unix(0, self.clock.Load()).UTC(), Canceled: ctx.Err() != nil}
	return nil
}

// A blocked claim is independent of the host registration owner.
func (self *competitionIdleTestStore) Claim(ctx context.Context, _ *Settings, _, image string) (*queuedJob, error) {
	self.claimCalls.Add(1)
	if self.onClaim == nil {
		return nil, nil
	}
	job, err := self.onClaim(ctx)
	if job != nil {
		job.WorkerImageDigest = image
	}
	return job, err
}

// Current-round reads are the genuine finishEpoch entry point.
func (self *competitionIdleTestStore) CurrentRound(ctx context.Context, settings *Settings) (*roundRecord, error) {
	if self.onRound != nil {
		return self.onRound(ctx)
	}
	return self.Store.CurrentRound(ctx, settings)
}

// The exact optional runtime interface is preserved, never stubbed as ready.
func (self *competitionIdleTestStore) CurrentAutomationRound(ctx context.Context, settings *Settings) (*roundRecord, error) {
	if self.onRuntime != nil {
		return self.onRuntime(ctx)
	}
	return self.Store.(competitionAutomationCurrentRoundStore).CurrentAutomationRound(ctx, settings)
}

// Activated-source loading still executes the canonical frozen policy helper.
func (self *competitionIdleTestStore) SourceActivation(ctx context.Context, settings *Settings, generation int64) (*model.CompetitionSourceActivation, error) {
	return self.Store.(competitionAutomationSourceIntentReader).SourceActivation(ctx, settings, generation)
}

// Only a real nonnil reservation transfers producer heartbeat ownership.
func (self *competitionIdleTestStore) ReserveRoundBaseline(ctx context.Context, settings *Settings, worker, image string) (*roundBaselinePreparation, error) {
	if self.onReserve != nil {
		return self.onReserve(ctx)
	}
	if self.baseline == nil {
		return nil, nil
	}
	return self.baseline.ReserveRoundBaseline(ctx, settings, worker, image)
}

// Existing preparation checks retain their original caller-owned context.
func (self *competitionIdleTestStore) CheckRoundBaselinePreparation(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation) error {
	return self.baseline.CheckRoundBaselinePreparation(ctx, settings, preparation)
}

// Failed production still uses the original preparation hold, never job state.
func (self *competitionIdleTestStore) HoldRoundBaselinePreparation(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation, hold roundBaselinePreparationHold) error {
	return self.baseline.HoldRoundBaselinePreparation(ctx, settings, preparation, hold)
}

// Only the baseline's own publication path can hand a job to evaluation.
func (self *competitionIdleTestStore) ClaimPreparedRoundBaseline(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation, prepared *preparedRoundBaseline) (*queuedJob, error) {
	return self.baseline.ClaimPreparedRoundBaseline(ctx, settings, preparation, prepared)
}

// Reservation cleanup remains mandatory even if the transfer callback fails.
func (self *competitionIdleTestStore) ReleaseRoundBaselinePreparation(ctx context.Context, preparation *roundBaselinePreparation) error {
	if self.onRelease != nil {
		return self.onRelease(ctx)
	}
	return self.baseline.ReleaseRoundBaselinePreparation(ctx, preparation)
}

// Each fresh check gets distinct report bytes; observations cannot be cached.
type competitionIdleTestEvaluator struct {
	check         HostSelfCheck
	checkCalls    atomic.Int64
	evaluateCalls atomic.Int64
	prepareCalls  atomic.Int64
	onCheck       func(context.Context, int) (HostSelfCheck, error)
	onAvailable   func(context.Context) (bool, error)
	onEvaluate    func(context.Context, *queuedJob) EvaluationOutcome
	onPrepare     func(context.Context, *roundBaselinePreparation) (*preparedRoundBaseline, error)
}

// Metadata changes identify a genuinely executed new self-check per refresh.
func (self *competitionIdleTestEvaluator) SelfCheck(ctx context.Context, _ *Settings) (HostSelfCheck, error) {
	call := int(self.checkCalls.Add(1))
	if self.onCheck != nil {
		return self.onCheck(ctx, call)
	}
	check := self.check
	check.MicrocodeRevision = fmt.Sprintf("synthetic-check-%d", call)
	return check, nil
}

// The distributed physical admission capability never starts a native run.
func (self *competitionIdleTestEvaluator) Available(ctx context.Context) (bool, error) {
	if self.onAvailable != nil {
		return self.onAvailable(ctx)
	}
	return true, nil
}

// Evaluation remains a separate owner and may only begin after idle join.
func (self *competitionIdleTestEvaluator) Evaluate(ctx context.Context, _ *Settings, job *queuedJob) EvaluationOutcome {
	self.evaluateCalls.Add(1)
	if self.onEvaluate != nil {
		return self.onEvaluate(ctx, job)
	}
	return EvaluationOutcome{Error: terminalInfrastructureError("synthetic_terminal", "synthetic completed evaluator")}
}

// A configured-but-warm baseline must never invoke an unused producer.
func (self *competitionIdleTestEvaluator) PrepareRoundBaseline(ctx context.Context, _ *Settings, preparation *roundBaselinePreparation) (*preparedRoundBaseline, error) {
	self.prepareCalls.Add(1)
	if self.onPrepare != nil {
		return self.onPrepare(ctx, preparation)
	}
	return nil, errors.New("unexpected synthetic baseline producer")
}

// One fixture owns every child, including cancellation after an assertion fails.
type competitionIdleTestFixture struct {
	worker    *Worker
	store     *competitionIdleTestStore
	evaluator *competitionIdleTestEvaluator
	clock     atomic.Int64
	ctx       context.Context
	cancel    context.CancelFunc
	watch     context.Context
	ticks     chan time.Time
	polls     chan time.Time
	done      chan struct{}
	resultErr error
	joined    atomic.Bool
	started   bool
}

// A finite watchdog is only a deadlock failsafe; positive channel transitions
// and synctest quiescence establish all ordering and negative assertions.
func newCompetitionIdleTestFixture(t *testing.T, settings *Settings, store Store) *competitionIdleTestFixture {
	t.Helper()
	if settings == nil {
		settings = validSettings()
	}
	if store == nil {
		store = &fakeStore{}
	}
	settings.WorkerHeartbeatSeconds, settings.HostHeartbeatMaxAgeSeconds = 20, 60
	ctx, cancel := context.WithCancel(t.Context())
	// testing cancels t.Context before cleanup; that cancellation stops the worker,
	// but must not expire the separate watchdog that joins its physical return.
	watch, stopWatch := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Minute)
	fixture := &competitionIdleTestFixture{ctx: ctx, cancel: cancel, watch: watch, ticks: make(chan time.Time), polls: make(chan time.Time), done: make(chan struct{})}
	fixture.clock.Store(time.Now().UnixNano())
	fixture.store = &competitionIdleTestStore{Store: store, clock: &fixture.clock, registrations: make(chan competitionIdleTestRegistration, 32)}
	fixture.evaluator = &competitionIdleTestEvaluator{check: passingHostCheck(settings)}
	fixture.evaluator.check.HostId = "synthetic-idle.example"
	var err error
	fixture.worker, err = newWorkerWithImageDigest(settings, fixture.store, fixture.evaluator, "synthetic-idle-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	fixture.worker.hostTickForTest, fixture.worker.pollTickForTest = fixture.ticks, fixture.polls
	fixture.worker.heartbeatSettings.LeaseRetry.Now = func() time.Time { return time.Unix(0, fixture.clock.Load()).UTC() }
	t.Cleanup(func() {
		if err := fixture.stopAndJoin(); err != nil {
			t.Error(err)
		}
		stopWatch()
	})
	return fixture
}

// The worker result is retained for assertions and cleanup independently.
func (self *competitionIdleTestFixture) start() {
	self.started = true
	go func() {
		defer close(self.done)
		self.resultErr = self.worker.Run(self.ctx)
		self.joined.Store(true)
	}()
}

// Closed completion is reusable by result readers and cleanup. The retained
// error is read only after close publishes the worker's physical return.
func (self *competitionIdleTestFixture) awaitJoined(watch context.Context) error {
	if !self.started {
		return nil
	}
	select {
	case <-self.done:
		return nil
	default:
	}
	select {
	case <-self.done:
		return nil
	case <-watch.Done():
		// Completion is positive evidence even at the watchdog boundary; a
		// still-running child never gains join credit from cancellation alone.
		select {
		case <-self.done:
			return nil
		default:
			return fmt.Errorf("synthetic idle worker did not physically join: %w", watch.Err())
		}
	}
}

// Every cleanup cancels only its own worker, then joins the same completion.
func (self *competitionIdleTestFixture) stopAndJoin() error {
	self.cancel()
	return self.awaitJoined(self.watch)
}

// Reading the original result never consumes cleanup's completion witness.
func (self *competitionIdleTestFixture) result(t *testing.T) error {
	t.Helper()
	if err := self.awaitJoined(self.watch); err != nil {
		t.Fatal(err)
	}
	return self.resultErr
}

// No wall-clock sleeps or short negative polls establish a lifecycle event.
func competitionIdleReceive[T any](t *testing.T, fixture *competitionIdleTestFixture, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-fixture.watch.Done():
		t.Fatal("synthetic idle lifecycle event missing")
		var zero T
		return zero
	}
}

// Every injected heartbeat has a positive fresh-registration acknowledgement.
func (self *competitionIdleTestFixture) refresh(t *testing.T) competitionIdleTestRegistration {
	t.Helper()
	now := time.Unix(0, self.clock.Add(int64(20*time.Second))).UTC()
	select {
	case self.ticks <- now:
	case <-self.watch.Done():
		t.Fatal("synthetic idle ticker was not consumed")
	}
	return competitionIdleReceive(t, self, self.store.registrations)
}

// The blocked actual availability route cannot starve a fresh full self-check.
func TestCompetitionIdleHostHeartbeatProgressesDuringBlockedAvailability(t *testing.T) {
	fixture := newCompetitionIdleTestFixture(t, nil, nil)
	fixture.worker.settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	fixture.worker.settings.distributedRoundEpoch = 2
	host := fixture.worker.settings.DistributedEvaluationPolicy.Hosts[0]
	fixture.worker.settings.executorHostId, fixture.worker.settings.executorSlotId = host.HostId, host.SlotId
	fixture.evaluator.check.HostId = host.HostId
	entered, canceled := make(chan struct{}), make(chan struct{})
	fixture.evaluator.onAvailable = func(ctx context.Context) (bool, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return false, ctx.Err()
	}
	fixture.start()
	first := competitionIdleReceive(t, fixture, fixture.store.registrations)
	competitionIdleReceive(t, fixture, entered)
	for index := 0; index < 4; index++ {
		fresh := fixture.refresh(t)
		if fresh.Check.MicrocodeRevision == first.Check.MicrocodeRevision || fresh.Canceled || fresh.At.Sub(first.At) != time.Duration(index+1)*20*time.Second {
			t.Fatal("idle registration reused stale proof or lost the original cadence")
		}
	}
	fixture.cancel()
	competitionIdleReceive(t, fixture, canceled)
	if !errors.Is(fixture.result(t), context.Canceled) || fixture.store.claimCalls.Load() != 0 || fixture.evaluator.evaluateCalls.Load() != 0 || fixture.evaluator.checkCalls.Load() != 5 {
		t.Fatal("blocked physical admission borrowed evaluation/claim authority")
	}
}

// The real runtime helper uses an activated-source-shaped store and retained
// envelope; only its post-preflight round read is held, never forged as ready.
func TestCompetitionIdleHostHeartbeatProgressesDuringBlockedRuntimeRead(t *testing.T) {
	settings, source, archive, _ := newCompetitionRuntimeBlobFixture(t)
	if _, err := archive.publishRuntimeManifest(t.Context(), settings, source.intent); err != nil {
		t.Fatal(err)
	}
	fixture := newCompetitionIdleTestFixture(t, settings, source)
	entered, canceled := make(chan struct{}), make(chan struct{})
	fixture.store.onRuntime = func(ctx context.Context) (*roundRecord, error) {
		if fixture.store.registerCalls.Load() == 0 {
			return source.CurrentAutomationRound(ctx, settings)
		}
		close(entered)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}
	fixture.start()
	competitionIdleReceive(t, fixture, fixture.store.registrations)
	competitionIdleReceive(t, fixture, entered)
	for index := 0; index < 4; index++ {
		if fixture.refresh(t).Canceled {
			t.Fatal("runtime read canceled unrelated idle health")
		}
	}
	fixture.cancel()
	competitionIdleReceive(t, fixture, canceled)
	if !errors.Is(fixture.result(t), context.Canceled) || fixture.evaluator.evaluateCalls.Load() != 0 {
		t.Fatal("runtime read gained evaluation authority")
	}
}

// The genuine finishEpoch database entry remains caller-cancelable while
// independent health publication advances without shortening its healthy budget.
func TestCompetitionIdleHostHeartbeatProgressesDuringBlockedFinishRead(t *testing.T) {
	fixture := newCompetitionIdleTestFixture(t, nil, nil)
	entered, canceled := make(chan struct{}), make(chan struct{})
	fixture.store.onRound = func(ctx context.Context) (*roundRecord, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return nil, ctx.Err()
	}
	fixture.start()
	competitionIdleReceive(t, fixture, fixture.store.registrations)
	competitionIdleReceive(t, fixture, entered)
	for index := 0; index < 4; index++ {
		fixture.refresh(t)
	}
	fixture.cancel()
	competitionIdleReceive(t, fixture, canceled)
	if !errors.Is(fixture.result(t), context.Canceled) || fixture.store.claimCalls.Load() != 0 {
		t.Fatal("unfinished round read reached claim")
	}
}

// The failed owner cancels its real blocked claim and writes only a genuine
// negative report. An independently healthy worker retains its original context.
func TestCompetitionIdleHostHeartbeatFailureIsolatedFromHealthySibling(t *testing.T) {
	failed, sibling := newCompetitionIdleTestFixture(t, nil, nil), newCompetitionIdleTestFixture(t, nil, nil)
	failedEntered, siblingEntered := make(chan struct{}), make(chan struct{})
	failed.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
		close(failedEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sibling.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
		close(siblingEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cause := errors.New("synthetic fresh containment failure")
	failed.evaluator.onCheck = func(_ context.Context, call int) (HostSelfCheck, error) {
		check := failed.evaluator.check
		if call == 1 {
			return check, nil
		}
		check.ArtifactStorage = false
		return check, cause
	}
	failed.start()
	sibling.start()
	competitionIdleReceive(t, failed, failed.store.registrations)
	competitionIdleReceive(t, sibling, sibling.store.registrations)
	competitionIdleReceive(t, failed, failedEntered)
	competitionIdleReceive(t, sibling, siblingEntered)
	negative := failed.refresh(t)
	if negative.Check.Eligible(failed.worker.settings) || !errors.Is(failed.result(t), cause) || sibling.ctx.Err() != nil {
		t.Fatal("failed idle check forged positive eligibility or canceled a sibling")
	}
	if sibling.refresh(t).Canceled {
		t.Fatal("healthy sibling lost its independent heartbeat")
	}
	sibling.cancel()
	if !errors.Is(sibling.result(t), context.Canceled) {
		t.Fatal("healthy sibling failed its explicit cancellation")
	}
}

// Eligible report flags are not proof of a private containment provenance. The
// actual distributed checker rejects this report; startup/idle/post-job must
// never republish those same bytes as a positive host heartbeat after failure.
func TestCompetitionIdleHostHeartbeatPrivateProbeFailureNeverWritesPositive(t *testing.T) {
	for _, stage := range []string{"startup", "idle", "postjob"} {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		check := fixture.evaluator.check
		check.NamespaceProbeSource, check.NamespaceProbeFresh, check.PrivateRuntimeBoundarySha256 = "retained_native_containment", false, strings.Repeat("a", 64)
		fixture.evaluator.check = check
		entered := make(chan struct{})
		fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }
		if stage == "postjob" {
			started, expires := server.NowUtc(), server.NowUtc().Add(time.Minute)
			job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: *check.RebaselineRoundId, StartedAt: &started}, AttemptCount: 1, LeaseExpiresAt: &expires}
			fixture.store.onClaim = func(context.Context) (*queuedJob, error) { return job, nil }
		}
		fixture.evaluator.onCheck = func(ctx context.Context, call int) (HostSelfCheck, error) {
			report := check
			if stage == "startup" || call > 1 {
				report.NamespaceProbeSource = "synthetic-unqualified-probe"
			}
			if !report.Eligible(fixture.worker.settings) {
				t.Error("private-probe regression lost its ordinary-eligible causal control")
			}
			raw, _ := json.Marshal(report)
			transport := &competitionAvailabilityTransport{response: competitionagent.Response{State: "ready", SelfCheck: raw}}
			return (&competitionAgentEvaluator{transport: transport}).SelfCheck(ctx, fixture.worker.settings)
		}
		fixture.start()
		if stage == "idle" {
			competitionIdleReceive(t, fixture, fixture.store.registrations)
			competitionIdleReceive(t, fixture, entered)
			select {
			case fixture.ticks <- time.Now():
			case <-fixture.watch.Done():
				t.Fatal("idle check never began")
			}
		}
		if err := fixture.result(t); err == nil || !strings.Contains(err.Error(), "authenticated executor did not pass") {
			t.Fatal("private-probe gate was weakened", stage, err)
		}
		want := int64(1)
		if stage == "startup" {
			want = 0
		}
		if fixture.store.registerCalls.Load() != want {
			t.Fatal("failed private probe published positive eligibility", stage, fixture.store.registerCalls.Load())
		}
	}
}

// Cancellation is not completion until the canceled self-check physically joins.
func TestCompetitionIdleHostHeartbeatCancellationJoinsSelfCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		entered, checking, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }
		fixture.evaluator.onCheck = func(ctx context.Context, call int) (HostSelfCheck, error) {
			if call == 1 {
				return fixture.evaluator.check, nil
			}
			close(checking)
			<-ctx.Done()
			close(canceled)
			<-release
			return HostSelfCheck{}, ctx.Err()
		}
		fixture.start()
		competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, entered)
		fixture.ticks <- time.Now()
		competitionIdleReceive(t, fixture, checking)
		fixture.cancel()
		competitionIdleReceive(t, fixture, canceled)
		synctest.Wait()
		if fixture.joined.Load() || fixture.store.registerCalls.Load() != 1 {
			t.Fatal("canceled unknown check escaped join or fabricated a heartbeat")
		}
		releaseOnce.Do(func() { close(release) })
		if !errors.Is(fixture.result(t), context.Canceled) {
			t.Fatal("canceled self-check lost its original cause")
		}
	})
}

// A nil database return after cancellation cannot advance local confirmation,
// and neither that database child nor its cleanup can escape the worker join.
func TestCompetitionIdleHostHeartbeatCanceledDatabaseAckIsNotAdopted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		entered, writing, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }
		fixture.store.onRegister = func(ctx context.Context, call int, _ HostSelfCheck) error {
			if call == 1 {
				return nil
			}
			close(writing)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil
		}
		fixture.start()
		first := competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, entered)
		fixture.clock.Add(int64(20 * time.Second))
		fixture.ticks <- time.Now()
		competitionIdleReceive(t, fixture, writing)
		fixture.cancel()
		competitionIdleReceive(t, fixture, canceled)
		synctest.Wait()
		if fixture.joined.Load() {
			t.Fatal("worker returned before the database call physically joined")
		}
		releaseOnce.Do(func() { close(release) })
		if !errors.Is(fixture.result(t), context.Canceled) || !fixture.worker.confirmedHostLeaseExpiresAt.Equal(first.At.Add(time.Minute)) {
			t.Fatal("late canceled database ACK extended confirmed host authority")
		}
	})
}

// A later, still-running check is canceled/joined before the exact claimed job
// uses the latest already-acknowledged report and unchanged original clock.
func TestCompetitionIdleHostHeartbeatJoinsBeforeEvaluationAndUsesLatestCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		claiming, permitClaim, checking, canceled, releaseCheck, evaluating := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var claimOnce, releaseOnce sync.Once
		t.Cleanup(func() { claimOnce.Do(func() { close(permitClaim) }); releaseOnce.Do(func() { close(releaseCheck) }) })
		started, deadline, lease := server.NowUtc(), server.NowUtc().Add(3*time.Hour), server.NowUtc().Add(time.Minute)
		roundId := server.NewId()
		job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: roundId, StartedAt: &started}, AttemptCount: 1, ExecutionDeadline: &deadline, LeaseExpiresAt: &lease}
		fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
			close(claiming)
			select {
			case <-permitClaim:
				return job, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		var checkJoined atomic.Bool
		fixture.evaluator.onCheck = func(ctx context.Context, call int) (HostSelfCheck, error) {
			check := fixture.evaluator.check
			if call == 2 {
				check.RebaselineRoundId = &roundId
				check.MicrocodeRevision = "synthetic-latest-acknowledged"
				return check, nil
			}
			if call == 3 {
				close(checking)
				<-ctx.Done()
				close(canceled)
				<-releaseCheck
				checkJoined.Store(true)
				return HostSelfCheck{}, ctx.Err()
			}
			return check, nil
		}
		fixture.evaluator.onEvaluate = func(ctx context.Context, actual *queuedJob) EvaluationOutcome {
			if !checkJoined.Load() || actual != job || !actual.StartedAt.Equal(started) || !actual.ExecutionDeadline.Equal(deadline) || actual.AttemptCount != 1 {
				t.Error("idle ownership overlapped evaluation or replaced its original clock")
			}
			close(evaluating)
			<-ctx.Done()
			return EvaluationOutcome{Error: terminalInfrastructureError("synthetic_canceled", "synthetic explicit shutdown")}
		}
		fixture.start()
		competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, claiming)
		fresh := fixture.refresh(t)
		if fresh.Check.RebaselineRoundId == nil || *fresh.Check.RebaselineRoundId != roundId {
			t.Fatal("latest authenticated report missing")
		}
		fixture.ticks <- time.Now()
		competitionIdleReceive(t, fixture, checking)
		claimOnce.Do(func() { close(permitClaim) })
		competitionIdleReceive(t, fixture, canceled)
		synctest.Wait()
		if fixture.evaluator.evaluateCalls.Load() != 0 {
			t.Fatal("evaluation preceded physical idle join")
		}
		releaseOnce.Do(func() { close(releaseCheck) })
		competitionIdleReceive(t, fixture, evaluating)
		fixture.cancel()
		if !errors.Is(fixture.result(t), context.Canceled) || fixture.evaluator.evaluateCalls.Load() != 1 {
			t.Fatal("job ownership was duplicated or failed its original cancellation")
		}
	})
}

// Virtual time exercises the real20s ticker across twenty-one ordinary1s polls.
// A configured warm profile cannot reset that ticker forever without producing.
func TestCompetitionIdleHostHeartbeatWarmBaselineKeepsTickerAcrossPolls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		fixture.worker.settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		fixture.worker.hostTickForTest, fixture.worker.pollTickForTest = nil, nil
		fixture.worker.heartbeatSettings.LeaseRetry.Now = time.Now
		fixture.store.onReserve = func(context.Context) (*roundBaselinePreparation, error) { return nil, nil }
		fixture.start()
		competitionIdleReceive(t, fixture, fixture.store.registrations)
		synctest.Wait()
		time.Sleep(21 * time.Second)
		synctest.Wait()
		if fixture.store.claimCalls.Load() < 20 || fixture.store.registerCalls.Load() < 2 || fixture.evaluator.prepareCalls.Load() != 0 {
			t.Fatal("warm baseline profile reset or stole idle ticker ownership", fixture.store.claimCalls.Load(), fixture.store.registerCalls.Load())
		}
		fixture.cancel()
		if !errors.Is(fixture.result(t), context.Canceled) {
			t.Fatal("warm baseline idle loop failed cancellation")
		}
	})
}

// The hook is strictly private, only follows an owned reservation and cannot
// discard its existing release cleanup when physical heartbeat join fails.
func TestCompetitionIdleHostHeartbeatProducerTransferFailureReleasesReservation(t *testing.T) {
	settings, preparation, store, host := newRoundBaselinePreparationTest(t)
	cause := errors.New("synthetic idle owner stop failure")
	worker := &Worker{settings: settings, store: store, evaluator: roundBaselinePreparationTestProducer{run: func(context.Context, *roundBaselinePreparation) (*preparedRoundBaseline, error) {
		t.Fatal("producer ran after failed transfer")
		return nil, cause
	}}}
	calls := 0
	job, err := worker.claimWithPreparedRoundBaselineOwned(t.Context(), host, func() (HostSelfCheck, error) { calls++; return host, cause })
	if job != nil || !errors.Is(err, cause) || calls != 1 || !store.released || store.claimed || preparation.SourceJob.StartedAt != nil || preparation.SourceJob.AttemptCount != 0 {
		t.Fatal("producer transfer lost reservation cleanup or started a candidate")
	}
}

// A real producer reservation, not configuration or a warm poll, cancels and
// joins the in-flight full check before the baseline's own heartbeat starts.
func TestCompetitionIdleHostHeartbeatProducerStartsOnlyAfterPhysicalJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings, preparation, baseline, host := newRoundBaselinePreparationTest(t)
		fixture := newCompetitionIdleTestFixture(t, settings, nil)
		fixture.evaluator.check = host
		fixture.store.baseline = baseline
		var reserve atomic.Bool
		claiming, checking, canceled, release, producing := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		fixture.store.onReserve = func(context.Context) (*roundBaselinePreparation, error) {
			if reserve.Load() {
				return preparation, nil
			}
			return nil, nil
		}
		fixture.store.onClaim = func(context.Context) (*queuedJob, error) { close(claiming); return nil, nil }
		var checkJoined atomic.Bool
		fixture.evaluator.onCheck = func(ctx context.Context, call int) (HostSelfCheck, error) {
			if call == 1 {
				return host, nil
			}
			close(checking)
			<-ctx.Done()
			close(canceled)
			<-release
			checkJoined.Store(true)
			return HostSelfCheck{}, ctx.Err()
		}
		fixture.evaluator.onPrepare = func(ctx context.Context, actual *roundBaselinePreparation) (*preparedRoundBaseline, error) {
			if !checkJoined.Load() || actual != preparation || actual.SourceJob.StartedAt != nil || actual.SourceJob.AttemptCount != 0 {
				t.Error("producer overlapped idle check or started candidate authority")
			}
			close(producing)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		fixture.start()
		competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, claiming)
		fixture.ticks <- time.Now()
		competitionIdleReceive(t, fixture, checking)
		reserve.Store(true)
		fixture.polls <- time.Now()
		competitionIdleReceive(t, fixture, canceled)
		synctest.Wait()
		if fixture.evaluator.prepareCalls.Load() != 0 {
			t.Fatal("baseline producer started before physical idle join")
		}
		releaseOnce.Do(func() { close(release) })
		competitionIdleReceive(t, fixture, producing)
		fixture.cancel()
		if fixture.result(t) == nil || !baseline.released || fixture.evaluator.prepareCalls.Load() != 1 || preparation.SourceJob.StartedAt != nil {
			t.Fatal("producer cleanup or original candidate clock changed")
		}
	})
}

// A concrete negative-check cleanup has its own finite cancellation edge and is
// physically joined even after work cancellation. It can never publish positive
// eligibility or advance the prior acknowledged host bound.
func TestCompetitionIdleHostHeartbeatFailedCheckCleanupIsBoundedAndJoined(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		entered, claimCanceled, writing, writeCanceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		cleanupEdge := make(chan context.CancelFunc, 1)
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		policy := defaultCompetitionIdleHostHeartbeatSettings()
		policy.cleanupContextForTest = func(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
			if parent.Err() != nil || budget != defaultCompetitionIdleHostHeartbeatSettings().FailedCheckCleanupTimeout {
				t.Error("failed-check cleanup borrowed a canceled parent or changed its owned bound")
			}
			ctx, cancel := context.WithCancel(parent)
			cleanupEdge <- cancel
			return ctx, cancel
		}
		fixture.worker.idleHeartbeatSettings = &policy
		fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) {
			close(entered)
			<-ctx.Done()
			close(claimCanceled)
			return nil, ctx.Err()
		}
		cause := errors.New("synthetic negative fresh host report")
		fixture.evaluator.onCheck = func(_ context.Context, call int) (HostSelfCheck, error) {
			check := fixture.evaluator.check
			if call == 1 {
				return check, nil
			}
			check.ArtifactStorage = false
			return check, cause
		}
		fixture.store.onRegister = func(ctx context.Context, call int, check HostSelfCheck) error {
			if call == 1 {
				return nil
			}
			if check.Eligible(fixture.worker.settings) || ctx.Err() != nil {
				t.Error("negative cleanup manufactured positive eligibility or reused canceled work")
			}
			close(writing)
			<-ctx.Done()
			close(writeCanceled)
			<-release
			return ctx.Err()
		}
		fixture.start()
		first := competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, entered)
		fixture.ticks <- time.Now()
		cancelCleanup := competitionIdleReceive(t, fixture, cleanupEdge)
		competitionIdleReceive(t, fixture, writing)
		competitionIdleReceive(t, fixture, claimCanceled)
		cancelCleanup()
		competitionIdleReceive(t, fixture, writeCanceled)
		synctest.Wait()
		if fixture.joined.Load() {
			t.Fatal("failed-check database child escaped physical join")
		}
		releaseOnce.Do(func() { close(release) })
		if !errors.Is(fixture.result(t), cause) || fixture.store.registerCalls.Load() != 2 || !fixture.worker.confirmedHostLeaseExpiresAt.Equal(first.At.Add(time.Minute)) {
			t.Fatal("cleanup lost the original failure or advanced positive host authority")
		}
	})
}

// An unknown failed transport has no authenticated report to register at all.
func TestCompetitionIdleHostHeartbeatUnknownFailureCannotPublishHost(t *testing.T) {
	fixture := newCompetitionIdleTestFixture(t, nil, nil)
	entered := make(chan struct{})
	fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }
	cause := errors.New("synthetic unknown control transport outcome")
	fixture.evaluator.onCheck = func(_ context.Context, call int) (HostSelfCheck, error) {
		if call == 1 {
			return fixture.evaluator.check, nil
		}
		return HostSelfCheck{}, cause
	}
	fixture.start()
	competitionIdleReceive(t, fixture, fixture.store.registrations)
	competitionIdleReceive(t, fixture, entered)
	select {
	case fixture.ticks <- time.Now():
	case <-fixture.watch.Done():
		t.Fatal("unknown check did not start")
	}
	if !errors.Is(fixture.result(t), cause) || fixture.store.registerCalls.Load() != 1 || fixture.evaluator.evaluateCalls.Load() != 0 {
		t.Fatal("unknown failed transport manufactured a host report/evaluation")
	}
}

// Quiescence is a causal oracle, not a timeout: with the real admission call
// held and a queued heartbeat edge, the old serial Run has no eligible reader
// and remains at one registration. Independent ownership must acknowledge two.
func TestCompetitionIdleHostHeartbeatBlockedAdmissionCausalScheduling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		fixture.worker.settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
		fixture.worker.settings.distributedRoundEpoch = 2
		host := fixture.worker.settings.DistributedEvaluationPolicy.Hosts[0]
		fixture.worker.settings.executorHostId, fixture.worker.settings.executorSlotId = host.HostId, host.SlotId
		fixture.evaluator.check.HostId = host.HostId
		fixture.ticks = make(chan time.Time, 1)
		fixture.worker.hostTickForTest = fixture.ticks
		entered := make(chan struct{})
		fixture.evaluator.onAvailable = func(ctx context.Context) (bool, error) { close(entered); <-ctx.Done(); return false, ctx.Err() }
		fixture.start()
		first := competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, entered)
		fixture.clock.Add(int64(20 * time.Second))
		fixture.ticks <- time.Now()
		synctest.Wait()
		if fixture.store.registerCalls.Load() != 2 || fixture.evaluator.checkCalls.Load() != 2 {
			t.Fatal("blocked idle admission starved the queued fresh-check heartbeat")
		}
		fresh := competitionIdleReceive(t, fixture, fixture.store.registrations)
		if fresh.Check.MicrocodeRevision == first.Check.MicrocodeRevision || fixture.store.claimCalls.Load() != 0 {
			t.Fatal("causal scheduling control reused a report or escaped admission")
		}
		fixture.cancel()
		if !errors.Is(fixture.result(t), context.Canceled) {
			t.Fatal("causal idle owner did not join cancellation")
		}
	})
}

// A provider that returns a complete-looking positive report after its caller
// was canceled cannot cause a new registration, even if it returns nil error.
func TestCompetitionIdleHostHeartbeatCanceledPositiveCheckCannotRegister(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCompetitionIdleTestFixture(t, nil, nil)
		entered, checking, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		fixture.store.onClaim = func(ctx context.Context) (*queuedJob, error) { close(entered); <-ctx.Done(); return nil, ctx.Err() }
		fixture.evaluator.onCheck = func(ctx context.Context, call int) (HostSelfCheck, error) {
			if call == 1 {
				return fixture.evaluator.check, nil
			}
			close(checking)
			<-ctx.Done()
			close(canceled)
			<-release
			return fixture.evaluator.check, nil
		}
		fixture.start()
		competitionIdleReceive(t, fixture, fixture.store.registrations)
		competitionIdleReceive(t, fixture, entered)
		fixture.ticks <- time.Now()
		competitionIdleReceive(t, fixture, checking)
		fixture.cancel()
		competitionIdleReceive(t, fixture, canceled)
		synctest.Wait()
		if fixture.joined.Load() {
			t.Fatal("complete-looking late check escaped physical join")
		}
		releaseOnce.Do(func() { close(release) })
		if !errors.Is(fixture.result(t), context.Canceled) || fixture.store.registerCalls.Load() != 1 {
			t.Fatal("canceled positive report manufactured a fresh heartbeat")
		}
	})
}
