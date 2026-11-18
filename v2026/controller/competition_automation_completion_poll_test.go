// Composed worker regressions use only disposable PostgreSQL, synthetic host
// reports and explicit barriers. No native evaluator or production state runs.
package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type competitionCompletionPollResult struct {
	Ready bool
	Err   error
}
type competitionCompletionSealResult struct {
	Result *model.CompetitionV2RoundResult
	Err    error
}

// Only entry/return barriers and a synthetic clock wrap the real store. All
// completion polls, admission holds and final seal transactions remain real.
type competitionCompletionWorkerPgStore struct {
	PostgresStore
	clock         atomic.Int64
	reportedClose atomic.Int64
	seals         atomic.Int32
	polls         chan competitionCompletionPollResult
	claimed       chan error
	sealed        chan competitionCompletionSealResult
	registered    chan struct{}
	beforeSeal    func(context.Context) error
	beforeClaim   func(context.Context) error
}

func newCompetitionCompletionWorkerPgStore(fixture *competitionRankingFixture) *competitionCompletionWorkerPgStore {
	self := &competitionCompletionWorkerPgStore{PostgresStore: fixture.store,
		polls: make(chan competitionCompletionPollResult, 16), claimed: make(chan error, 16), sealed: make(chan competitionCompletionSealResult, 16), registered: make(chan struct{}, 16)}
	self.clock.Store(fixture.now.UnixNano())
	self.PostgresStore.now = func() time.Time { return time.Unix(0, self.clock.Load()).UTC() }
	self.PostgresStore.dequeueCompetitionSignal = func(context.Context, *Settings) (*server.Id, error) { return nil, nil }
	return self
}

func (self *competitionCompletionWorkerPgStore) CurrentRound(ctx context.Context, settings *Settings) (*roundRecord, error) {
	round, err := self.PostgresStore.CurrentAutomationRound(ctx, settings)
	if round != nil && self.reportedClose.Load() != 0 {
		round.ClosesAt = time.Unix(0, self.reportedClose.Load()).UTC()
	}
	return round, err
}

func (self *competitionCompletionWorkerPgStore) CurrentStagingRound(context.Context, *Settings) (*roundRecord, error) {
	return nil, nil
}

func (self *competitionCompletionWorkerPgStore) automationEpochSealReady(ctx context.Context, settings *Settings, roundId server.Id) (bool, error) {
	ready, err := self.PostgresStore.automationEpochSealReady(ctx, settings, roundId)
	self.polls <- competitionCompletionPollResult{Ready: ready, Err: err}
	return ready, err
}

func (self *competitionCompletionWorkerPgStore) SealAndElect(ctx context.Context, settings *Settings, roundId server.Id) (*model.CompetitionV2RoundResult, error) {
	self.seals.Add(1)
	if self.beforeSeal != nil {
		if err := self.beforeSeal(ctx); err != nil {
			return nil, err
		}
	}
	result, err := self.PostgresStore.SealAndElect(ctx, settings, roundId)
	self.sealed <- competitionCompletionSealResult{Result: result, Err: err}
	return result, err
}

func (self *competitionCompletionWorkerPgStore) Claim(ctx context.Context, settings *Settings, workerId, image string) (*queuedJob, error) {
	if self.beforeClaim != nil {
		if err := self.beforeClaim(ctx); err != nil {
			return nil, err
		}
	}
	job, err := self.PostgresStore.Claim(ctx, settings, workerId, image)
	self.claimed <- err
	return job, err
}

func (self *competitionCompletionWorkerPgStore) RegisterHost(ctx context.Context, settings *Settings, check HostSelfCheck) error {
	err := self.PostgresStore.RegisterHost(ctx, settings, check)
	if err == nil {
		self.registered <- struct{}{}
	}
	return err
}

// Every execution or producer entry is a tripwire; only real self-check shape
// and real host registration may run while the tested store says idle/held.
type competitionCompletionNoStartEvaluator struct {
	check     HostSelfCheck
	forbidden atomic.Int32
}

func (self *competitionCompletionNoStartEvaluator) SelfCheck(context.Context, *Settings) (HostSelfCheck, error) {
	return self.check, nil
}
func (self *competitionCompletionNoStartEvaluator) Evaluate(context.Context, *Settings, *queuedJob) EvaluationOutcome {
	self.forbidden.Add(1)
	return EvaluationOutcome{Error: terminalInfrastructureError("synthetic_forbidden_start", "Synthetic idle completion test cannot start work.")}
}
func (self *competitionCompletionNoStartEvaluator) PrepareRoundBaseline(context.Context, *Settings, *roundBaselinePreparation) (*preparedRoundBaseline, error) {
	self.forbidden.Add(1)
	return nil, errors.New("synthetic preparation tripwire")
}

func startCompetitionCompletionWorkerPg(t testing.TB, fixture *competitionRankingFixture, store *competitionCompletionWorkerPgStore) (context.CancelFunc, <-chan error, chan time.Time, chan time.Time, *competitionCompletionNoStartEvaluator) {
	t.Helper()
	evaluator := &competitionCompletionNoStartEvaluator{check: passingHostCheck(fixture.settings)}
	evaluator.check.RebaselineRoundId = &fixture.round.RoundId
	worker, err := newWorkerWithImageDigest(fixture.settings, store, evaluator, "synthetic-completion-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	poll, heartbeat := make(chan time.Time), make(chan time.Time)
	worker.pollTickForTest, worker.hostTickForTest = poll, heartbeat
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second) // guard only; every observation has an explicit barrier.
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		for range done {
		}
	})
	return cancel, done, poll, heartbeat, evaluator
}

func receiveCompetitionCompletion[T any](t testing.TB, channel <-chan T, done <-chan error) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case err := <-done:
		t.Fatal("worker exited before its explicit completion barrier", err)
	}
	var zero T
	return zero
}

func tickCompetitionCompletion(t testing.TB, tick chan<- time.Time, done <-chan error) {
	t.Helper()
	select {
	case tick <- time.Time{}:
	case err := <-done:
		t.Fatal("worker exited before explicit idle tick", err)
	}
}

func competitionCompletionHoldArgs(key string, active bool, revision int) model.CompetitionAutomationHoldArgs {
	reason := "OPERATOR_HOLD"
	if !active {
		reason = "RECOVERY_APPROVED"
	}
	return model.CompetitionAutomationHoldArgs{Kind: "evaluation", Active: active, ExpectedRevision: revision, IdempotencyKey: key,
		ReasonCode: reason, PublicExplanation: "Synthetic completion polling regression.", EvidenceSha256: strings.Repeat("a", 64)}
}

// The real Worker.Run must not enter SealAndElect before the latest durable
// close. Exactly at close it still takes the real guarded seal transaction.
func TestCompetitionCompletionWorkerPgWaitsUntilExactClose(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		store := newCompetitionCompletionWorkerPgStore(fixture)
		store.reportedClose.Store(fixture.now.Add(-time.Hour).UnixNano()) // stale early launch projection must not authorize a seal.
		_, done, poll, _, evaluator := startCompetitionCompletionWorkerPg(tb, fixture, store)
		if result := receiveCompetitionCompletion(tb, store.polls, done); result.Ready || result.Err != nil {
			tb.Fatal("open poll granted sealing", result)
		}
		if err := receiveCompetitionCompletion(tb, store.claimed, done); err != nil {
			tb.Fatal(err)
		}
		if store.seals.Load() != 0 {
			tb.Fatal("open worker called the expensive sealing transaction")
		}
		store.clock.Store(fixture.round.ClosesAt.Add(-time.Nanosecond).UnixNano())
		tickCompetitionCompletion(tb, poll, done)
		if result := receiveCompetitionCompletion(tb, store.polls, done); result.Ready || result.Err != nil {
			tb.Fatal("pre-close poll granted sealing", result)
		}
		_ = receiveCompetitionCompletion(tb, store.claimed, done)
		if store.seals.Load() != 0 {
			tb.Fatal("pre-close worker attempted sealing")
		}
		store.clock.Store(fixture.round.ClosesAt.UnixNano())
		store.reportedClose.Store(fixture.round.ClosesAt.Add(time.Hour).UnixNano()) // stale late projection must not prevent genuine close.
		tickCompetitionCompletion(tb, poll, done)
		if err := <-done; err != nil {
			tb.Fatal("exact-close worker failed its real seal", err)
		}
		if store.seals.Load() != 1 || evaluator.forbidden.Load() != 0 {
			tb.Fatal("close boundary changed seal/execution count")
		}
	})
}

// After close, a queued job awaiting its own existing availability still
// drains: real SealAndElect refuses while real Claim returns no runnable row.
func TestCompetitionCompletionWorkerPgClosedInventoryStillDrains(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		jobId := queueCompetitionAutomationPollJob(tb, fixture)
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), `UPDATE competition_job SET available_at=$2 WHERE job_id=$1`, jobId, fixture.round.ClosesAt.Add(time.Hour)))
		})
		store := newCompetitionCompletionWorkerPgStore(fixture)
		store.clock.Store(fixture.round.ClosesAt.UnixNano())
		cancel, done, poll, _, evaluator := startCompetitionCompletionWorkerPg(tb, fixture, store)
		for index := 0; index < 2; index++ {
			if index != 0 {
				tickCompetitionCompletion(tb, poll, done)
			}
			if result := receiveCompetitionCompletion(tb, store.sealed, done); result.Result != nil || !errors.Is(result.Err, ErrReviewNotReady) {
				tb.Fatal("draining inventory was sealed", result)
			}
			if err := receiveCompetitionCompletion(tb, store.claimed, done); err != nil {
				tb.Fatal(err)
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || evaluator.forbidden.Load() != 0 {
			tb.Fatal("draining worker changed cancellation/execution", err)
		}
		assertCompetitionCompletionUnchanged(tb, store.PostgresStore, fixture, jobId)
	})
}

// Durable evaluation hold remains idle, including a genuine host heartbeat.
// No claim/score/attempt/clock/winner/seal is manufactured by repeated polls.
func TestCompetitionCompletionWorkerPgOpenHoldKeepsHeartbeatAndQueuedIdentity(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		jobId := queueCompetitionAutomationPollJob(tb, fixture)
		store := newCompetitionCompletionWorkerPgStore(fixture)
		if _, err := store.SetAutomationHold(tb.Context(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-open-held-poll", true, 0)); err != nil {
			tb.Fatal(err)
		}
		cancel, done, poll, heartbeat, evaluator := startCompetitionCompletionWorkerPg(tb, fixture, store)
		_ = receiveCompetitionCompletion(tb, store.registered, done)
		for index := 0; index < 3; index++ {
			if index != 0 {
				tickCompetitionCompletion(tb, poll, done)
			}
			if result := receiveCompetitionCompletion(tb, store.polls, done); result.Ready || !errors.Is(result.Err, ErrAutomationHeld) {
				tb.Fatal("held poll lost its exact idle sentinel", result)
			}
		}
		tickCompetitionCompletion(tb, heartbeat, done)
		_ = receiveCompetitionCompletion(tb, store.registered, done)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			tb.Fatal("held worker failed to join on cancellation", err)
		}
		if store.seals.Load() != 0 || len(store.claimed) != 0 || evaluator.forbidden.Load() != 0 {
			tb.Fatal("held idle entered seal/claim/execution")
		}
		assertCompetitionCompletionUnchanged(tb, store.PostgresStore, fixture, jobId)
	})
}

func assertCompetitionCompletionUnchanged(t testing.TB, store PostgresStore, fixture *competitionRankingFixture, jobId server.Id) {
	t.Helper()
	server.Db(t.Context(), func(conn server.PgConn) {
		var state string
		var attempts, seals, decisions, releases int
		var started, completed *time.Time
		var score []byte
		server.Raise(conn.QueryRow(t.Context(), `SELECT state,attempt_count,started_at,completed_at,score_json FROM competition_job WHERE job_id=$1`, jobId).Scan(&state, &attempts, &started, &completed, &score))
		server.Raise(conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM competition_evaluation_seal WHERE round_id=$1),(SELECT count(*) FROM competition_reward_decision WHERE round_id=$1)`, fixture.round.RoundId).Scan(&seals, &decisions))
		server.Raise(conn.QueryRow(t.Context(), `SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='execution_custody_released'`, jobId).Scan(&releases))
		if state != "queued" || attempts != 0 || started != nil || completed != nil || score != nil || seals != 0 || decisions != 0 || releases != 0 {
			t.Fatal("completion polling changed queued or election history")
		}
	})
	round, err := store.GetRound(t.Context(), fixture.settings, fixture.round.RoundId)
	if err != nil || round.AutomationSealedAt != nil || round.FinalizedAt != nil || round.WinnerJobId != nil || !round.ClosesAt.Equal(fixture.round.ClosesAt) {
		t.Fatal("held completion changed original round clock/election", err)
	}
}

// Hold races happen AFTER the read-only connection has returned. The final
// source/season lock and hold read, not the optimization, own the decision.
func TestCompetitionCompletionWorkerPgHoldRaceIntoSealAndClaim(t *testing.T) {
	for _, boundary := range []string{"seal", "claim"} {
		t.Run(boundary, func(t *testing.T) {
			(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
				fixture := newCompetitionAutomationFixture(tb)
				jobId := queueCompetitionAutomationPollJob(tb, fixture)
				store := newCompetitionCompletionWorkerPgStore(fixture)
				if boundary == "seal" {
					store.clock.Store(fixture.round.ClosesAt.UnixNano())
				}
				var once sync.Once
				activate := func(ctx context.Context) (err error) {
					once.Do(func() {
						_, err = store.SetAutomationHold(ctx, fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-late-"+boundary, true, 0))
					})
					return err
				}
				if boundary == "seal" {
					store.beforeSeal = activate
				} else {
					store.beforeClaim = activate
				}
				trace := &competitionSealPollTracer{}
				scope, err := server.NewTestPgQueryScope(tb.Context(), trace)
				if err != nil {
					tb.Fatal(err)
				}
				defer scope.Close()
				cancel, done, poll, _, evaluator := startCompetitionCompletionWorkerPg(tb, fixture, store)
				if boundary == "seal" {
					if result := receiveCompetitionCompletion(tb, store.sealed, done); result.Result != nil || !errors.Is(result.Err, ErrAutomationHeld) {
						tb.Fatal("late seal hold escaped final SQL guard", result)
					}
				} else if err := receiveCompetitionCompletion(tb, store.claimed, done); !errors.Is(err, ErrAutomationHeld) {
					tb.Fatal("late claim hold escaped final SQL guard", err)
				}
				tickCompetitionCompletion(tb, poll, done)
				// The first poll was unheld; the next observes the durable hold.
				_ = receiveCompetitionCompletion(tb, store.polls, done)
				if result := receiveCompetitionCompletion(tb, store.polls, done); result.Ready || !errors.Is(result.Err, ErrAutomationHeld) {
					tb.Fatal("held successor poll changed control flow", result)
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					tb.Fatal(err)
				}
				trace.stateLock.Lock()
				rollbacks := trace.rollbacks
				trace.stateLock.Unlock()
				if rollbacks != 0 || evaluator.forbidden.Load() != 0 {
					tb.Fatal("expected held admission used panic/rollback or executed work", rollbacks)
				}
				assertCompetitionCompletionUnchanged(tb, store.PostgresStore, fixture, jobId)
			})
		})
	}
}

// Readiness is not final authority, including when the clock moves backward
// between the returned poll and final transaction. SQL must refuse the seal.
func TestCompetitionCompletionWorkerPgFinalCloseGuardWinsAfterReadyPoll(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		store := newCompetitionCompletionWorkerPgStore(fixture)
		store.clock.Store(fixture.round.ClosesAt.UnixNano())
		var once sync.Once
		store.beforeSeal = func(context.Context) error {
			once.Do(func() { store.clock.Store(fixture.round.ClosesAt.Add(-time.Microsecond).UnixNano()) })
			return nil
		}
		cancel, done, _, _, evaluator := startCompetitionCompletionWorkerPg(tb, fixture, store)
		if result := receiveCompetitionCompletion(tb, store.sealed, done); result.Result != nil || !errors.Is(result.Err, ErrReviewNotReady) {
			tb.Fatal("stale ready poll overruled final close guard", result)
		}
		_ = receiveCompetitionCompletion(tb, store.claimed, done)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || evaluator.forbidden.Load() != 0 {
			tb.Fatal("refused seal changed idle cancellation/execution", err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_evaluation_seal WHERE round_id=$1`, fixture.round.RoundId).Scan(&count))
			if count != 0 {
				tb.Fatal("pre-close seal was written")
			}
		})
	})
}

// The read uses the durable close rather than a stale request/CurrentRound
// projection. Legacy manual staging close stays on its original finalizer.
func TestCompetitionCompletionPgDurableClockScopeAndLegacyManualClose(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		store := newCompetitionCompletionWorkerPgStore(fixture)
		for _, now := range []time.Time{fixture.round.OpensAt.Add(-time.Nanosecond), fixture.round.ClosesAt.Add(-time.Nanosecond), fixture.round.ClosesAt} {
			store.clock.Store(now.UnixNano())
			ready, err := store.PostgresStore.automationEpochSealReady(tb.Context(), fixture.settings, fixture.round.RoundId)
			if err != nil || ready != !now.Before(fixture.round.ClosesAt) {
				tb.Fatal("fresh durable close was replaced by stale clock/status", ready, err)
			}
		}
		foreign := *fixture.settings
		foreign.AutomationPolicy.SeasonId = "synthetic-other-season"
		if ready, err := store.PostgresStore.automationEpochSealReady(tb.Context(), &foreign, fixture.round.RoundId); ready || !errors.Is(err, ErrNotFound) {
			tb.Fatal("foreign scope supplied completion authority", err)
		}
		legacy := newCompetitionRankingFixture(tb, true)
		legacy.now = legacy.round.OpensAt.Add(time.Second)
		closed, err := legacy.store.CloseStagingRound(tb.Context(), legacy.settings)
		if err != nil || closed.AdmissionClosedAt == nil || !legacy.now.Before(closed.ClosesAt) {
			tb.Fatal("synthetic manual close setup", err)
		}
		worker, err := newWorkerWithImageDigest(legacy.settings, legacy.store, &fakeEvaluator{}, "synthetic-manual-stage", testWorkerImageDigest())
		if err != nil {
			tb.Fatal(err)
		}
		if finished, err := worker.finishEpoch(tb.Context()); err != nil || !finished {
			tb.Fatal("automated close optimization suppressed legacy durable manual closure", err)
		}
	})
}
