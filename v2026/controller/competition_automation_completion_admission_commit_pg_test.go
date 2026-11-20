// Admission regressions fail actual commits after a held callback. A released
// hold must let the real retried path succeed without carrying old stateErr.
package controller

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// One serial test owns the fault, retry hook and actual connection census.
// Deferred faults reuse the current connection; hold release follows its return.
type competitionCompletionAdmissionCommitFault struct {
	ctx        context.Context
	scope      *server.TestPgQueryScope
	fault      *competitionSupervisorReadCommitFault
	trace      *competitionCompletionConnectionTrace
	retryCount int
}

// Each test gets its own guarded hold and one-shot deferred constraint. The
// retry hook runs only after the failed transaction has returned pool custody.
func newCompetitionCompletionAdmissionCommitFault(t testing.TB, store PostgresStore, settings *Settings, code string) *competitionCompletionAdmissionCommitFault {
	t.Helper()
	first, err := store.SetAutomationHold(t.Context(), settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-admission-commit-held", true, 0))
	if err != nil {
		t.Fatal(err)
	}
	prepareCompetitionSupervisorReadCommitFault(t.Context(), code)
	self := &competitionCompletionAdmissionCommitFault{ctx: t.Context(), fault: &competitionSupervisorReadCommitFault{},
		trace: &competitionCompletionConnectionTrace{held: map[*pgx.Conn]bool{}, transactions: map[*pgx.Conn]bool{}}}
	self.scope, err = server.NewTestPgQueryScope(t.Context(), multitracer.New(self.fault, self.trace))
	if err != nil {
		t.Fatal(err)
	}
	if code == "40001" {
		self.ctx = server.Testing_WithTxRerunHook(self.ctx, func() {
			self.retryCount++
			held := func() int {
				self.trace.lock.Lock()
				defer self.trace.lock.Unlock()
				return len(self.trace.held)
			}()
			if held != 0 {
				t.Fatal("retry attempted hold release while retaining a connection", held)
			}
			if _, err := store.SetAutomationHold(context.Background(), settings, "synthetic-operator", competitionCompletionHoldArgs("synthetic-admission-commit-released", false, first.Revision)); err != nil {
				t.Fatal(err)
			}
		})
	}
	return self
}

// Close while still inside the disposable environment, after every user joined.
func (self *competitionCompletionAdmissionCommitFault) close(t testing.TB) {
	t.Helper()
	if err := self.scope.Close(); err != nil {
		t.Error(err)
	}
}

// Error identity and the actual server reply distinguish a failed commit from
// expected hold. A successful retry must have spent precisely that one fault.
func (self *competitionCompletionAdmissionCommitFault) assert(t testing.TB, code string, err error) {
	t.Helper()
	if self.fault.commitFailures.Load() != 1 || errors.Is(err, ErrAutomationHeld) {
		t.Fatal("admission did not preserve the actual commit fault boundary", code, self.fault.commitFailures.Load(), err)
	}
	if code == "23514" {
		var postgres *pgconn.PgError
		if !errors.As(err, &postgres) || postgres.Code != code || self.retryCount != 0 {
			t.Fatal("admission commit failure lost precedence over held state", code, self.retryCount, err)
		}
	} else if code != "40001" || err != nil || self.retryCount != 1 {
		t.Fatal("released-hold admission did not successfully rerun", code, self.retryCount, err)
	}
	func() {
		self.trace.lock.Lock()
		defer self.trace.lock.Unlock()
		if self.trace.nested != 0 || self.trace.unknownRelease != 0 || len(self.trace.held) != 0 || self.trace.acquired == 0 || self.trace.acquired != self.trace.released {
			t.Fatal("commit/retry retained or reacquired owned connection custody", self.trace.acquired, self.trace.released, self.trace.nested)
		}
	}()
}

// Genuine source/bootstrap authority precedes the two singleton admission
// methods. Only the optional producer profile differs between their callers.
func newCompetitionCompletionSingletonAdmission(t testing.TB) (*competitionRankingFixture, server.Id) {
	t.Helper()
	fixture := newCompetitionAutomationFixture(t)
	fixture.store.dequeueCompetitionSignal = func(context.Context, *Settings) (*server.Id, error) { return nil, nil }
	lease, err := fixture.store.AcquireAutomationLease(t.Context(), fixture.settings, "synthetic-admission-commit-owner", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
	return fixture, queueCompetitionAutomationPollJob(t, fixture)
}

// A held singleton callback must not conceal a real non-retryable commit error.
func TestCompetitionCompletionPgSingletonClaimCommitFailureOutranksHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, jobId := newCompetitionCompletionSingletonAdmission(tb)
		fault := newCompetitionCompletionAdmissionCommitFault(tb, fixture.store, fixture.settings, "23514")
		defer fault.close(tb)
		before := readCompetitionPreparedCompletionSnapshot(tb.Context(), &queuedJob{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: fixture.round.RoundId}}, false)
		job, err := fixture.store.Claim(fault.ctx, fixture.settings, "synthetic-singleton-commit-worker", testWorkerImageDigest())
		fault.assert(tb, "23514", err)
		if job != nil || !reflect.DeepEqual(before, readCompetitionPreparedCompletionSnapshot(tb.Context(), &queuedJob{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: fixture.round.RoundId}}, false)) {
			tb.Fatal("failed singleton held commit changed result, clock, events or slot")
		}
	})
}

// The second actual callback commits one genuine claim. Keeping old stateErr
// would return a false held error even though that claim became durable.
func TestCompetitionCompletionPgSingletonClaimRetryClearsHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, jobId := newCompetitionCompletionSingletonAdmission(tb)
		fault := newCompetitionCompletionAdmissionCommitFault(tb, fixture.store, fixture.settings, "40001")
		defer fault.close(tb)
		job, err := fixture.store.Claim(fault.ctx, fixture.settings, "synthetic-singleton-commit-worker", testWorkerImageDigest())
		fault.assert(tb, "40001", err)
		if job == nil || job.JobId != jobId || job.State != "running" || job.AttemptCount != 1 || job.StartedAt == nil || !job.StartedAt.Equal(fixture.now) || !job.SubmittedAt.Equal(fixture.now) || job.WorkerImageDigest != testWorkerImageDigest() {
			tb.Fatal("successful singleton retry lost its original queued identity or fresh single claim")
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var worker string
			var slotJob server.Id
			var claims int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT worker_id,job_id FROM competition_worker_slot WHERE slot_id=1`).Scan(&worker, &slotJob))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='claimed'`, jobId).Scan(&claims))
			if worker != "synthetic-singleton-commit-worker" || slotJob != jobId || claims != 1 {
				tb.Fatal("singleton retry did not durably claim exactly once")
			}
		})
	})
}

// All source, intake, baseline and qualification guards stay enabled while the
// held distributed callback encounters its actual deferred commit rejection.
func TestCompetitionCompletionPgDistributedClaimCommitFailureOutranksHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, true)
		settings := fixture.selected(0)
		fault := newCompetitionCompletionAdmissionCommitFault(tb, fixture.store, settings, "23514")
		defer fault.close(tb)
		before := readCompetitionPreparedCompletionSnapshot(tb.Context(), fixture.sourceJob, true)
		job, err := fixture.store.Claim(fault.ctx, settings, "synthetic-distributed-commit-worker", testWorkerImageDigest())
		fault.assert(tb, "23514", err)
		if job != nil || !reflect.DeepEqual(before, readCompetitionPreparedCompletionSnapshot(tb.Context(), fixture.sourceJob, true)) {
			tb.Fatal("failed distributed held commit changed original job, baseline, slot or events")
		}
	})
}

// A released hold must produce a real first distributed attempt and exact slot
// fence, not another refusal that could hide sticky callback result state.
func TestCompetitionCompletionPgDistributedClaimRetryClearsHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, true)
		settings := fixture.selected(0)
		fault := newCompetitionCompletionAdmissionCommitFault(tb, fixture.store, settings, "40001")
		defer fault.close(tb)
		before := readCompetitionPreparedCompletionSnapshot(tb.Context(), fixture.sourceJob, true)
		job, err := fixture.store.Claim(fault.ctx, settings, "synthetic-distributed-commit-worker", testWorkerImageDigest())
		fault.assert(tb, "40001", err)
		if job == nil || job.JobId != fixture.sourceJob.JobId || job.RoundId != fixture.round.RoundId || job.State != "running" || job.AttemptCount != 1 || job.StartedAt == nil || job.ExecutionDeadline == nil || job.SlotFencingToken != 1 || job.ExecutorHostId != settings.executorHostId || job.SlotId != settings.executorSlotId || !job.SubmittedAt.Equal(fixture.sourceJob.SubmittedAt) || job.WorkerImageDigest != testWorkerImageDigest() {
			tb.Fatal("released-hold distributed retry did not return its genuine first claim")
		}
		after := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, true)
		if !bytes.Equal(before.baselineJson, after.baselineJson) || after.events != before.events+1 {
			tb.Fatal("distributed retry rewrote its prepared baseline or duplicated claim events")
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var slotJob server.Id
			var worker string
			var fence int64
			server.Raise(conn.QueryRow(tb.Context(), `SELECT job_id,worker_id,fencing_token FROM competition_execution_slot WHERE slot_id=$1 AND host_id=$2`, job.SlotId, job.ExecutorHostId).Scan(&slotJob, &worker, &fence))
			if slotJob != job.JobId || worker != "synthetic-distributed-commit-worker" || fence != job.SlotFencingToken {
				tb.Fatal("distributed retry returned custody different from its committed slot")
			}
		})
	})
}

// Initial preparation still has no publication or candidate execution when its
// held callback's commit fails; a real server error outranks expected state.
func TestCompetitionCompletionPgInitialReservationCommitFailureOutranksHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, jobId := newCompetitionCompletionSingletonAdmission(tb)
		fixture.settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		fault := newCompetitionCompletionAdmissionCommitFault(tb, fixture.store, fixture.settings, "23514")
		defer fault.close(tb)
		identity := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: fixture.round.RoundId}}
		before := readCompetitionPreparedCompletionSnapshot(tb.Context(), identity, false)
		preparation, err := fixture.store.ReserveRoundBaseline(fault.ctx, fixture.settings, "synthetic-reservation-commit-worker", testWorkerImageDigest())
		fault.assert(tb, "23514", err)
		if preparation != nil || !reflect.DeepEqual(before, readCompetitionPreparedCompletionSnapshot(tb.Context(), identity, false)) {
			tb.Fatal("failed initial reservation commit changed queued source, control, slot or events")
		}
	})
}

// A successful rerun must return its new real reservation while the candidate
// remains clockless. Sticky held state cannot be accepted as success or absence.
func TestCompetitionCompletionPgInitialReservationRetryClearsHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, jobId := newCompetitionCompletionSingletonAdmission(tb)
		fixture.settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		fault := newCompetitionCompletionAdmissionCommitFault(tb, fixture.store, fixture.settings, "40001")
		defer fault.close(tb)
		preparation, err := fixture.store.ReserveRoundBaseline(fault.ctx, fixture.settings, "synthetic-reservation-commit-worker", testWorkerImageDigest())
		fault.assert(tb, "40001", err)
		if preparation == nil || preparation.JobId != jobId || preparation.RoundId != fixture.round.RoundId || preparation.WorkerId != "synthetic-reservation-commit-worker" || preparation.WorkerImage != testWorkerImageDigest() || preparation.SourceJob == nil || preparation.SourceJob.State != "queued" || preparation.SourceJob.StartedAt != nil || preparation.SourceJob.AttemptCount != 0 || !preparation.StartedAt.Equal(fixture.now) || !preparation.Deadline.Equal(fixture.now.Add(fixture.settings.roundBaselinePreparation.Settings.TotalTimeout)) {
			tb.Fatal("released-hold reservation retry did not return exact new preparation custody")
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var owner string
			var slotJob *server.Id
			var expires time.Time
			var reservations, claims, baselines, attempts int
			var started *time.Time
			server.Raise(conn.QueryRow(tb.Context(), `SELECT worker_id,job_id,lease_expires_at FROM competition_worker_slot WHERE slot_id=1`).Scan(&owner, &slotJob, &expires))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT attempt_count,started_at,
 (SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='round_baseline_preparation_reserved'),
 (SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='claimed'),
 (SELECT count(*) FROM competition_round_baseline WHERE round_id=$2)
 FROM competition_job WHERE job_id=$1`, jobId, fixture.round.RoundId).Scan(&attempts, &started, &reservations, &claims, &baselines))
			if owner != preparation.Owner || slotJob != nil || !expires.Equal(preparation.Deadline.Add(fixture.settings.roundBaselinePreparation.Settings.CleanupTimeout)) || reservations != 1 || claims != 0 || baselines != 0 || attempts != 0 || started != nil {
				tb.Fatal("reservation retry started/published candidate work or duplicated durable preparation")
			}
		})
	})
}
