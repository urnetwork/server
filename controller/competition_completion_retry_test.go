// Finished evaluations keep their exact outcome across bounded metadata retries,
// without another evaluator, claim, attempt, execution budget or lease renewal.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Decorating the public method prevents a promoted store method bypassing failures.
type competitionCompletionStore struct {
	fakeStore
	calls    int
	observed []EvaluationOutcome
	complete func(context.Context, int, EvaluationOutcome) (bool, error)
}

// A refused transaction is not counted as durable completion by the embedded store.
func (self *competitionCompletionStore) Complete(ctx context.Context, settings *Settings, workerId string, jobId server.Id, outcome EvaluationOutcome) (bool, error) {
	self.calls++
	self.observed = append(self.observed, outcome)
	if self.complete != nil {
		if retry, err := self.complete(ctx, self.calls, outcome); err != nil {
			return retry, err
		}
	}
	return self.fakeStore.Complete(ctx, settings, workerId, jobId, outcome)
}

// Optional barriers control evaluation return and cancellation without wall timers.
type competitionCompletionEvaluator struct {
	calls    int
	outcome  EvaluationOutcome
	evaluate func(context.Context) EvaluationOutcome
}

// The real host gate is supplied directly to evaluateOne in these synthetic tests.
func (self *competitionCompletionEvaluator) SelfCheck(context.Context, *Settings) (HostSelfCheck, error) {
	return HostSelfCheck{}, nil
}

// The outcome is constructed once and never regenerated on a metadata retry.
func (self *competitionCompletionEvaluator) Evaluate(ctx context.Context, _ *Settings, _ *queuedJob) EvaluationOutcome {
	self.calls++
	if self.evaluate != nil {
		return self.evaluate(ctx)
	}
	return self.outcome
}

// All identities and contents are synthetic; the owner has a generous live lease.
type competitionCompletionFixture struct {
	worker    *Worker
	store     *competitionCompletionStore
	evaluator *competitionCompletionEvaluator
	job       *queuedJob
	host      HostSelfCheck
	clock     *competitionLeaseRetryClock
}

// Only injected time transitions consume retry budgets, not scheduler delays.
func newCompetitionCompletionFixture(t *testing.T) *competitionCompletionFixture {
	t.Helper()
	clock := &competitionLeaseRetryClock{now: server.NowUtc()}
	settings := validSettings()
	store := &competitionCompletionStore{}
	baseline := []byte(`{"schema":1,"synthetic":"unchanged-control"}`)
	digest := sha256.Sum256(baseline)
	evaluator := &competitionCompletionEvaluator{outcome: EvaluationOutcome{
		Score: competitionRankingScore(90, 110), ArtifactManifest: json.RawMessage(`{"schema":1,"synthetic":"unchanged-archive"}`),
		RoundBaselineJson: baseline, RoundBaselineSha256: hex.EncodeToString(digest[:]),
	}}
	worker, err := newWorkerWithImageDigest(settings, store, evaluator, "synthetic-completion-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	worker.heartbeatSettings.CompletionRetry.Now = clock.Now
	worker.heartbeatSettings.CompletionRetry.Wait = func(_ context.Context, delay time.Duration) error { clock.Advance(delay); return nil }
	worker.heartbeatSettings.LeaseRetry.Now = clock.Now
	started, expires := clock.Now().Add(-time.Minute), clock.Now().Add(10*time.Minute)
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId(), StartedAt: &started}, AttemptCount: 1, LeaseExpiresAt: &expires}
	host := passingHostCheck(settings)
	host.RebaselinePassed, host.RebaselineRoundId = true, &job.RoundId
	return &competitionCompletionFixture{worker: worker, store: store, evaluator: evaluator, job: job, host: host, clock: clock}
}

// One actual evaluation and one immutable outcome survive the first rolled-back lock timeout.
func TestCompetitionWorkerCompletionRetriesSameScoredOutcome(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	startedAt, attempt := *fixture.job.StartedAt, fixture.job.AttemptCount
	wanted := fixture.evaluator.outcome
	var persistenceDeadline time.Time
	fixture.store.complete = func(ctx context.Context, call int, outcome EvaluationOutcome) (bool, error) {
		deadline, ok := ctx.Deadline()
		if call == 1 {
			persistenceDeadline = deadline
		}
		if ctx.Err() != nil || !ok || !deadline.Equal(persistenceDeadline) || !reflect.DeepEqual(outcome, wanted) {
			t.Fatal("completion changed its live bounded context or immutable outcome")
		}
		if call == 1 {
			return true, errors.Join(&pgconn.PgError{Code: "55P03"}, &pgconn.PgError{Code: "55P03"})
		}
		return false, nil
	}
	if err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host); err != nil {
		t.Fatalf("scored outcome was lost instead of retrying metadata: %v", err)
	}
	if fixture.store.calls != 2 || fixture.store.completedCount() != 1 || fixture.evaluator.calls != 1 || fixture.store.claims != 0 ||
		!fixture.job.StartedAt.Equal(startedAt) || fixture.job.AttemptCount != attempt || fixture.store.handbacks != 0 {
		t.Fatal("completion retry changed evaluation, claim, attempt, budget or handback")
	}
}

// Expired execution still records terminal metadata under its independent live claim.
func TestCompetitionWorkerCompletionPersistsExecutionTimeout(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	startedAt := fixture.clock.Now().Add(-time.Duration(fixture.worker.settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second - time.Minute)
	fixture.job.StartedAt = &startedAt
	fixture.store.complete = func(ctx context.Context, call int, outcome EvaluationOutcome) (bool, error) {
		if ctx.Err() != nil || outcome.Score != nil || outcome.Error == nil ||
			outcome.Error.Code != "evaluation_time_budget_exhausted" || outcome.Error.Retriable {
			t.Fatal("expired execution lost its live terminal persistence context or cause")
		}
		if call == 1 {
			return false, &pgconn.PgError{Code: "55P03"}
		}
		return false, nil
	}
	if err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host); err != nil || fixture.store.calls != 2 ||
		fixture.store.completedCount() != 1 || fixture.evaluator.calls != 0 || !fixture.job.StartedAt.Equal(startedAt) || fixture.job.AttemptCount != 1 {
		t.Fatalf("timeout metadata was discarded or granted another execution: %v", err)
	}
}

// Canceling the evaluation child does not cancel bounded terminal persistence.
func TestCompetitionWorkerCompletionPersistsCanceledEvaluation(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	fixture.evaluator.outcome = EvaluationOutcome{Error: terminalInfrastructureError("evaluator_process_failed", "synthetic canceled evaluation")}
	fixture.worker.evaluationContextForTest = func(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithDeadline(parent, deadline)
		cancel()
		return ctx, cancel
	}
	canceledChildObserved := false
	fixture.evaluator.evaluate = func(ctx context.Context) EvaluationOutcome {
		canceledChildObserved = errors.Is(ctx.Err(), context.Canceled)
		return fixture.evaluator.outcome
	}
	fixture.store.complete = func(ctx context.Context, call int, outcome EvaluationOutcome) (bool, error) {
		if ctx.Err() != nil || outcome.Error != fixture.evaluator.outcome.Error {
			t.Fatal("canceled child leaked into terminal persistence or changed its cause")
		}
		if call == 1 {
			return false, &pgconn.PgError{Code: "55P03"}
		}
		return false, nil
	}
	if err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host); err != nil || fixture.store.calls != 2 || fixture.evaluator.calls != 1 || !canceledChildObserved {
		t.Fatalf("canceled outcome was not retained: %v", err)
	}
}

// Last acknowledged job authority is never replaced with an invented or stale window.
func TestCompetitionWorkerCompletionStopsAtAuthorityBoundaries(t *testing.T) {
	for _, check := range []struct {
		name    string
		calls   int
		durable int
	}{
		{name: "expired before persistence", calls: 0},
		{name: "expired during wait", calls: 1},
		{name: "changed fence", calls: 2},
		{name: "unknown expiry", calls: 1},
		{name: "unknown expiry success", calls: 1, durable: 1},
	} {
		fixture := newCompetitionCompletionFixture(t)
		cause := &pgconn.PgError{Code: "55P03"}
		wanted := error(ErrLeaseLost)
		if check.name == "expired before persistence" {
			expires := fixture.clock.Now()
			fixture.job.LeaseExpiresAt = &expires
		}
		if check.name == "unknown expiry" || check.name == "unknown expiry success" {
			fixture.job.LeaseExpiresAt = nil
			wanted = cause
			if check.durable == 1 {
				wanted = nil
			}
		}
		fixture.worker.heartbeatSettings.CompletionRetry.Wait = func(_ context.Context, delay time.Duration) error {
			if check.name == "expired during wait" {
				fixture.clock.Advance(10 * time.Minute)
			} else {
				fixture.clock.Advance(delay)
			}
			return nil
		}
		fixture.store.complete = func(_ context.Context, call int, _ EvaluationOutcome) (bool, error) {
			if check.durable == 1 {
				return false, nil
			}
			if call == 2 {
				return false, ErrLeaseLost
			}
			return false, cause
		}
		err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host)
		if !errors.Is(err, wanted) || fixture.store.calls != check.calls || fixture.store.completedCount() != check.durable || fixture.evaluator.calls != 1 || fixture.store.claims != 0 {
			t.Errorf("%s: completion dispatched beyond authority: calls=%d, %v", check.name, fixture.store.calls, err)
		}
	}
}

// Total elapsed metadata time and the attempt cap are independent and never restart scoring.
func TestCompetitionWorkerCompletionExhaustsBoundedPersistence(t *testing.T) {
	for _, elapsedBudget := range []bool{true, false} {
		fixture := newCompetitionCompletionFixture(t)
		cause := &pgconn.PgError{Code: "55P03"}
		wantedCalls := 3
		if elapsedBudget {
			fixture.worker.heartbeatSettings.CompletionRetry.MaximumAttempts = 20
			wantedCalls = 2
		}
		fixture.store.complete = func(_ context.Context, _ int, _ EvaluationOutcome) (bool, error) {
			if elapsedBudget {
				fixture.clock.Advance(4 * time.Second)
			}
			return true, cause
		}
		if err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host); !errors.Is(err, cause) || fixture.store.calls != wantedCalls ||
			fixture.store.completedCount() != 0 || fixture.evaluator.calls != 1 || fixture.store.claims != 0 {
			t.Errorf("elapsed=%t: metadata budget restarted or returned rolled-back retry: calls=%d, %v", elapsedBudget, fixture.store.calls, err)
		}
	}
}

// Only completed rollback lock errors may replay; ambiguous or mixed causes remain terminal.
func TestCompetitionWorkerCompletionRejectsNonLockErrors(t *testing.T) {
	forgingLeaf := &competitionLeaseRetryForgingLeaf{}
	for _, cause := range []error{
		ErrLeaseLost, context.Canceled, context.DeadlineExceeded, &pgconn.PgError{Code: "40001"},
		&pgconn.PgError{Code: "08006"}, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("synthetic ambiguous completion")},
		errors.Join(&pgconn.PgError{Code: "55P03"}, errors.New("synthetic uncertain rollback")),
		errors.Join(&pgconn.PgError{Code: "55P03"}, server.DbContextDoneError),
		errors.Join(&pgconn.PgError{Code: "55P03"}, errors.New("Done")),
		errors.Join(&pgconn.PgError{Code: "55P03"}, errors.New("Done.")),
		server.NewErrorCauseBatch([]error{&pgconn.PgError{Code: "55P03"}, nil}),
		errors.Join(&pgconn.PgError{Code: "55P03"}, &competitionLeaseRetryCycleError{}),
		errors.Join(&pgconn.PgError{Code: "55P03"}, forgingLeaf),
	} {
		fixture := newCompetitionCompletionFixture(t)
		fixture.store.complete = func(context.Context, int, EvaluationOutcome) (bool, error) { return false, cause }
		if err := fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host); !errors.Is(err, cause) || fixture.store.calls != 1 || fixture.store.completedCount() != 0 {
			t.Errorf("non-lock completion was replayed or changed: calls=%d, %v", fixture.store.calls, err)
		}
	}
	if forgingLeaf.asCalls != 0 {
		t.Fatal("completion persistence invoked an arbitrary error As hook")
	}
}

// Completion consumes the successful heartbeat's rolling lower bound, not the original claim expiry.
func TestCompetitionWorkerCompletionUsesLastConfirmedAuthority(t *testing.T) {
	fixture := newCompetitionCompletionFixture(t)
	fixture.worker.settings.WorkerLeaseSeconds = 1800
	ticks, accepted := make(chan time.Time, 1), make(chan struct{}, 1)
	fixture.worker.evaluationTickForTest = ticks
	fixture.worker.evaluationHeartbeatForTest = func() { accepted <- struct{}{} }
	started, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	releaseEvaluation := func() { finishOnce.Do(func() { close(finish) }) }
	t.Cleanup(releaseEvaluation)
	fixture.evaluator.evaluate = func(context.Context) EvaluationOutcome { close(started); <-finish; return fixture.evaluator.outcome }
	fixture.store.complete = func(_ context.Context, call int, _ EvaluationOutcome) (bool, error) {
		if call == 1 {
			return false, &pgconn.PgError{Code: "55P03"}
		}
		return false, nil
	}
	watchdog, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.worker.evaluateOne(t.Context(), fixture.job, fixture.host) }()
	select {
	case <-started:
	case <-watchdog.Done():
		t.Fatal("evaluation did not reach its start barrier")
	}
	ticks <- fixture.clock.Now()
	select {
	case <-accepted:
	case err := <-done:
		t.Fatalf("heartbeat failed before completion barrier: %v", err)
	case <-watchdog.Done():
		t.Fatal("heartbeat did not acknowledge actual success")
	}
	fixture.clock.Advance(11 * time.Minute)
	releaseEvaluation()
	select {
	case err := <-done:
		if err != nil || fixture.store.calls != 2 || fixture.evaluator.calls != 1 {
			t.Fatalf("completion used initial expired authority instead of its confirmed heartbeat: %v", err)
		}
	case <-watchdog.Done():
		t.Fatal("completion did not leave its metadata retry")
	}
}
