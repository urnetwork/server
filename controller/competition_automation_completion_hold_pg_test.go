// Real guarded PostgreSQL transactions prove that exact expected hold states
// commit without panic, while commit/database/ownership failures retain force.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

func TestCompetitionCompletionPgHeldSealClaimAndInitialReservationAreExpected(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		jobId := queueCompetitionAutomationPollJob(tb, fixture)
		lease, err := fixture.store.AcquireAutomationLease(tb.Context(), fixture.settings, "synthetic-completion-owner", time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
		fixture.settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		if _, err := fixture.store.SetAutomationHold(tb.Context(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-held-admission", true, 0)); err != nil {
			tb.Fatal(err)
		}
		trace := &competitionSealPollTracer{}
		scope, err := server.NewTestPgQueryScope(tb.Context(), trace)
		if err != nil {
			tb.Fatal(err)
		}
		defer scope.Close()
		for range 3 {
			if result, err := fixture.store.SealAndElect(tb.Context(), fixture.settings, fixture.round.RoundId); result != nil || err != ErrAutomationHeld {
				tb.Fatal("hold seal sentinel changed", err)
			}
			if job, err := fixture.store.Claim(tb.Context(), fixture.settings, "synthetic-held-worker", testWorkerImageDigest()); job != nil || err != ErrAutomationHeld {
				tb.Fatal("hold claim sentinel changed", err)
			}
			if preparation, err := fixture.store.ReserveRoundBaseline(tb.Context(), fixture.settings, "synthetic-held-worker", testWorkerImageDigest()); preparation != nil || err != ErrAutomationHeld {
				tb.Fatal("hold reservation sentinel changed", err)
			}
		}
		trace.stateLock.Lock()
		commits, rollbacks := trace.commits, trace.rollbacks
		trace.stateLock.Unlock()
		if commits != 9 || rollbacks != 0 {
			tb.Fatal("expected hold used panic/rollback instead of a returned read transaction", commits, rollbacks)
		}
		assertCompetitionCompletionUnchanged(tb, fixture.store, fixture, jobId)
		// Positive stale authority still wins before the now-normal hold return.
		stale := *fixture.settings
		staleLease := *lease
		staleLease.FencingToken++
		stale.automationLease = &staleLease
		if job, err := fixture.store.Claim(tb.Context(), &stale, "synthetic-stale-worker", testWorkerImageDigest()); job != nil || !errors.Is(err, ErrLeaseLost) {
			tb.Fatal("hold normalization masked lost supervisor ownership", err)
		}
	})
}

func TestCompetitionCompletionPgDistributedHeldClaimStillChecksRealSupervisor(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, true)
		settings := fixture.selected(0)
		if _, err := fixture.store.SetAutomationHold(tb.Context(), settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-distributed-held-poll", true, 0)); err != nil {
			tb.Fatal(err)
		}
		trace := &competitionSealPollTracer{}
		scope, err := server.NewTestPgQueryScope(tb.Context(), trace)
		if err != nil {
			tb.Fatal(err)
		}
		defer scope.Close()
		for range 3 {
			if job, err := fixture.store.Claim(tb.Context(), settings, "synthetic-held-executor", testWorkerImageDigest()); job != nil || err != ErrAutomationHeld {
				tb.Fatal("held distributed claim changed authority", err)
			}
		}
		trace.stateLock.Lock()
		commits, rollbacks := trace.commits, trace.rollbacks
		trace.stateLock.Unlock()
		if commits != 3 || rollbacks != 0 {
			tb.Fatal("held distributed claim used panic/rollback", commits, rollbacks)
		}
		stale := *settings
		staleLease := *settings.automationLease
		staleLease.FencingToken++
		stale.automationLease = &staleLease
		if job, err := fixture.store.Claim(tb.Context(), &stale, "synthetic-stale-executor", testWorkerImageDigest()); job != nil || !errors.Is(err, ErrLeaseLost) {
			tb.Fatal("held distributed claim masked a positive fence mismatch", err)
		}
	})
}

// Hold idleness cannot catch or convert a positive ownership loss. Compose the
// actual authority-read wrapper with the actual held Worker.Run/store path.
func TestCompetitionCompletionWorkerPgHeldStillStopsOnPositiveFenceLoss(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionAutomationFixture(tb)
		store := newCompetitionCompletionWorkerPgStore(fixture)
		lease, err := store.AcquireAutomationLease(tb.Context(), fixture.settings, "synthetic-held-fence-owner", time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
		if _, err := store.SetAutomationHold(tb.Context(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-positive-fence-hold", true, 0)); err != nil {
			tb.Fatal(err)
		}
		evaluator := &competitionCompletionNoStartEvaluator{check: passingHostCheck(fixture.settings)}
		worker, err := newWorkerWithImageDigest(fixture.settings, store, evaluator, "synthetic-held-fenced-worker", testWorkerImageDigest())
		if err != nil {
			tb.Fatal(err)
		}
		worker.pollTickForTest, worker.hostTickForTest = make(chan time.Time), make(chan time.Time)
		settings := defaultCompetitionSupervisorWorkerFenceSettings()
		fenceTick := make(chan time.Time)
		settings.tickForTest = fenceTick
		stale := *lease
		stale.FencingToken++
		ctx, cancel := context.WithTimeout(tb.Context(), 15*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- runCompetitionSupervisorWorkerFenced(ctx, lease.LeaseExpiresAt, settings,
				func(read context.Context) (time.Time, error) {
					return readCompetitionSupervisorWorkerFence(read, store.PostgresStore, fixture.settings, stale)
				}, worker.Run)
			close(done)
		}()
		tb.Cleanup(func() {
			cancel()
			for range done {
			}
		})
		if result := receiveCompetitionCompletion(tb, store.polls, done); result.Err != ErrAutomationHeld {
			tb.Fatal("worker did not reach real held idleness", result)
		}
		tickCompetitionCompletion(tb, fenceTick, done)
		if err := <-done; !errors.Is(err, ErrLeaseLost) || evaluator.forbidden.Load() != 0 {
			tb.Fatal("held worker masked positive fencing loss", err)
		}
	})
}

// A real deferred constraint fails COMMIT. Its error must outrank the callback's
// expected hold, and an actual serialization retry must clear that prior state.
func TestCompetitionCompletionPgHoldCommitErrorAndRetryPrecedence(t *testing.T) {
	for _, code := range []string{"23514", "40001"} {
		t.Run(code, func(t *testing.T) {
			(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
				fixture := newCompetitionAutomationFixture(tb)
				first, err := fixture.store.SetAutomationHold(tb.Context(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-held-commit-first", true, 0))
				if err != nil {
					tb.Fatal(err)
				}
				prepareCompetitionSupervisorReadCommitFault(tb.Context(), code)
				trace := &competitionSupervisorReadCommitFault{}
				scope, err := server.NewTestPgQueryScope(tb.Context(), trace)
				if err != nil {
					tb.Fatal(err)
				}
				defer scope.Close()
				ctx, reruns := tb.Context(), 0
				if code == "40001" {
					ctx = server.Testing_WithTxRerunHook(ctx, func() {
						reruns++
						if _, err := fixture.store.SetAutomationHold(context.Background(), fixture.settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-held-commit-release", false, first.Revision)); err != nil {
							tb.Fatal(err)
						}
					})
				}
				result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
				if result != nil || errors.Is(err, ErrAutomationHeld) || trace.commitFailures.Load() != 1 {
					tb.Fatal("failed/retried commit leaked prior expected hold", err)
				}
				if code == "40001" {
					if err != ErrReviewNotReady || reruns != 1 {
						tb.Fatal("retry did not reread the released hold and open clock", err, reruns)
					}
				} else {
					var postgres *pgconn.PgError
					if !errors.As(err, &postgres) || postgres.Code != code {
						tb.Fatal("genuine COMMIT error was masked by expected hold", err)
					}
				}
			})
		})
	}
}
