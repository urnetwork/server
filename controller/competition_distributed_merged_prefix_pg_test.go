// The complete published control prefix must compose with prospective slots.
// These private-Pg fixtures retain real guards and the original job clocks.
package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// This literal singleton update shape models the preceding release, not an
// old binary execution. It runs before the new executor columns even exist.
func claimCompetitionDistributedMergeLegacyJob(t testing.TB, fixture *competitionRankingFixture, jobId server.Id) time.Time {
	t.Helper()
	ctx := t.Context()
	now := fixture.now
	leaseUntil := now.Add(time.Duration(fixture.settings.WorkerLeaseSeconds) * time.Second)
	var startedAt time.Time
	server.Tx(ctx, func(tx server.PgTx) {
		automationScopeLockInTx(ctx, tx, fixture.settings)
		server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *fixture.settings.automationLease))
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.competition_v2_worker='on'`))
		var priorJob *server.Id
		server.Raise(tx.QueryRow(ctx, `SELECT job_id FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&priorJob))
		if priorJob != nil {
			t.Fatal("synthetic old worker did not own an idle singleton")
		}
		server.Raise(tx.QueryRow(ctx, `UPDATE competition_job SET state='running',started_at=COALESCE(started_at,$2),
		 lease_owner=$3,lease_expires_at=$4,attempt_count=attempt_count+1,worker_image_digest=$5
		 WHERE job_id=$1 AND state='queued' RETURNING started_at`, jobId, now, "synthetic-merged-legacy-worker", leaseUntil, testWorkerImageDigest()).Scan(&startedAt))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=$1,job_id=$2,lease_expires_at=$3,heartbeat_at=$4 WHERE slot_id=1`,
			"synthetic-merged-legacy-worker", jobId, leaseUntil, now))
	})
	return startedAt
}

// An epoch-one job starts on the published pre-executor schema, survives the
// additive migration while running, then renews/reclaims/completes unchanged.
// The independently published finalization hold remains operative throughout.
func TestCompetitionDistributedMergedMigrationKeepsRunningEpochOne(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		server.ApplyDbMigrationsUpTo(ctx, server.CompetitionFinalizationHoldMigrationVersion())
		fixture := newCompetitionAutomationFixture(tb)
		fixture.store.dequeueCompetitionSignal = func(context.Context, *Settings) (*server.Id, error) { return nil, nil }
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-merged-legacy-supervisor", 5*time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
		args := competitionFinalizationHoldArgs("synthetic-merged-prefix-finalization")
		hold, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args)
		if err != nil {
			tb.Fatal(err)
		}
		jobId := queueCompetitionAutomationPollJob(tb, fixture)
		originalStart := claimCompetitionDistributedMergeLegacyJob(tb, fixture, jobId)
		fixture.now = fixture.now.Add(time.Second)
		if err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-merged-legacy-worker", jobId); err != nil {
			tb.Fatal("published singleton heartbeat failed before executor migration", err)
		}
		before := readCompetitionLeasePgSnapshot(ctx, jobId)
		server.ApplyDbMigrationsUpTo(ctx, server.CompetitionDistributedEvaluationMigrationVersion())
		if after := readCompetitionLeasePgSnapshot(ctx, jobId); !reflect.DeepEqual(before, after) {
			tb.Fatal("executor migration mutated active singleton custody")
		}
		fixture.now = fixture.now.Add(time.Second)
		if err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-merged-legacy-worker", jobId); err != nil {
			tb.Fatal("new schema rejected old singleton heartbeat shape", err)
		}
		if err := fixture.store.HandBack(ctx, "synthetic-merged-legacy-worker", jobId, "synthetic-merged-restart"); err != nil {
			tb.Fatal(err)
		}
		recovered, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-merged-legacy-worker", testWorkerImageDigest())
		if err != nil || recovered == nil || recovered.JobId != jobId || recovered.AttemptCount != 2 || recovered.StartedAt == nil || !recovered.StartedAt.Equal(originalStart) || recovered.SlotFencingToken != 0 || recovered.ExecutionDeadline != nil {
			tb.Fatal("merged prefix reset or distributed an epoch-one recovery", recovered, err)
		}
		if retry, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-merged-legacy-worker", jobId,
			EvaluationOutcome{Error: terminalInfrastructureError("synthetic-merged-terminal", "Synthetic joined terminal outcome.")}); err != nil || retry {
			tb.Fatal("new schema rejected singleton completion", retry, err)
		}
		status, err := fixture.store.AutomationHoldStatus(ctx, fixture.settings)
		if err != nil || !status.Finalization || status.Evaluation {
			tb.Fatal("executor migration changed independent finalization authority", status, err)
		}
		if result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); result != nil || !errors.Is(err, ErrFinalizationHeld) {
			tb.Fatal("executor migration bypassed the published finalization hold", result, err)
		}
		replay, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args)
		if err != nil || replay == nil || replay.HoldId != hold.HoldId {
			tb.Fatal("executor migration changed finalization CAS identity", replay, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var executorColumnsNull bool
			var occupied, physicalSlots int
			server.Raise(conn.QueryRow(ctx, `SELECT executor_host_id IS NULL AND executor_slot_id IS NULL AND executor_fencing_token IS NULL AND execution_deadline_at IS NULL FROM competition_job WHERE job_id=$1`, jobId).Scan(&executorColumnsNull))
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM competition_worker_slot WHERE job_id IS NOT NULL),(SELECT count(*) FROM competition_execution_slot)`).Scan(&occupied, &physicalSlots))
			if !executorColumnsNull || occupied != 0 || physicalSlots != 0 {
				tb.Fatal("inactive prospective policy reinterpreted singleton ownership")
			}
		})
	})
}

// Read both physical authorities and the fixed execution clock in one query.
type competitionDistributedMergedHeartbeatSnapshot struct {
	jobLease, slotLease, slotHeartbeat, startedAt, deadline time.Time
	fence                                                   int64
	attempt                                                 int
}

// The returned value is read only after an actual heartbeat invocation joins.
func readCompetitionDistributedMergedHeartbeat(ctx context.Context, jobId server.Id) (snapshot competitionDistributedMergedHeartbeatSnapshot) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT j.lease_expires_at,s.lease_expires_at,s.heartbeat_at,j.started_at,j.execution_deadline_at,j.executor_fencing_token,j.attempt_count
		 FROM competition_job j JOIN competition_execution_slot s ON s.job_id=j.job_id AND s.slot_id=j.executor_slot_id WHERE j.job_id=$1`, jobId).Scan(
			&snapshot.jobLease, &snapshot.slotLease, &snapshot.slotHeartbeat, &snapshot.startedAt, &snapshot.deadline, &snapshot.fence, &snapshot.attempt))
	})
	return snapshot
}

// The merged decorator must observe distributed job-before-slot writes, retain
// all three exact fence statements, and acknowledge rollback of a zero-row slot.
func TestCompetitionDistributedMergedHeartbeatKeepsDiagnosticsAndRollback(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		job, err := fixture.store.Claim(ctx, settings, "synthetic-merged-distributed-worker", testWorkerImageDigest())
		if err != nil || job == nil {
			tb.Fatal(err)
		}
		authority := withCompetitionJobAuthority(ctx, job)
		before := readCompetitionDistributedMergedHeartbeat(ctx, job.JobId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_merged_executor_slot_refusal() RETURNS trigger LANGUAGE plpgsql AS $body$
			 BEGIN
			  IF NOT EXISTS(SELECT 1 FROM competition_job WHERE job_id=NEW.job_id AND lease_expires_at=NEW.lease_expires_at) THEN
			   RAISE EXCEPTION 'synthetic distributed slot preceded its job lease update';
			  END IF;
			  RETURN NULL;
			 END $body$;
			 CREATE TRIGGER synthetic_merged_executor_slot_refusal BEFORE UPDATE OF lease_expires_at ON competition_execution_slot
			 FOR EACH ROW EXECUTE FUNCTION synthetic_merged_executor_slot_refusal();`))
		})
		fixture.clock.Advance(time.Second)
		err = fixture.store.Heartbeat(authority, settings, "synthetic-merged-distributed-worker", job.JobId)
		diagnostic := competitionHeartbeatFailureDetails(err).HeartbeatDatabase
		jobUpdate := competitionHeartbeatStatementAt(diagnostic, competitionHeartbeatDatabaseJobUpdate, 1)
		slotUpdate := competitionHeartbeatStatementAt(diagnostic, competitionHeartbeatDatabaseSlotUpdate, 1)
		if !errors.Is(err, ErrLeaseLost) || diagnostic == nil || diagnostic.Step != competitionHeartbeatDatabaseSlotUpdate || diagnostic.UnknownStatements != 0 ||
			jobUpdate == nil || !jobUpdate.Returned || !jobUpdate.Succeeded || slotUpdate == nil || !slotUpdate.Returned || !slotUpdate.Succeeded ||
			diagnostic.Transaction.CallbackEntered != 1 || diagnostic.Transaction.CommitAttempts != 0 || diagnostic.Transaction.RollbackAcknowledged != 1 {
			tb.Fatal("merged heartbeat lost real per-host failure provenance", diagnostic, err)
		}
		for ordinal := uint8(1); ordinal <= 3; ordinal++ {
			statement := competitionHeartbeatStatementAt(diagnostic, competitionHeartbeatDatabaseJobAuthority, ordinal)
			if statement == nil || !statement.Returned || !statement.Succeeded {
				tb.Fatal("distributed fence escaped the bounded job-authority observation", ordinal)
			}
		}
		if after := readCompetitionDistributedMergedHeartbeat(ctx, job.JobId); !reflect.DeepEqual(before, after) {
			tb.Fatal("failed per-host heartbeat committed partial authority")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_merged_executor_slot_refusal ON competition_execution_slot`))
		})
		if err := fixture.store.Heartbeat(authority, settings, "synthetic-merged-distributed-worker", job.JobId); err != nil {
			tb.Fatal("healthy per-host heartbeat did not recover", err)
		}
		after := readCompetitionDistributedMergedHeartbeat(ctx, job.JobId)
		if !after.jobLease.After(before.jobLease) || !after.jobLease.Equal(after.slotLease) || !after.startedAt.Equal(before.startedAt) || !after.deadline.Equal(before.deadline) || after.fence != before.fence || after.attempt != before.attempt {
			tb.Fatal("healthy renewal reset its execution clock or fence", before, after)
		}
	})
}

// Draining remains global; the final host reconciles its original deadline,
// while the separately published hold can still defer the first seal/election.
func TestCompetitionDistributedMergedFinalizeWaitsForReconciliationAndHoldRelease(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		fixture.addJob(tb, server.Id{15: 4}, fixture.clock.Now(), "merged-second")
		first, err := fixture.store.Claim(ctx, fixture.selected(0), "synthetic-merged-first", testWorkerImageDigest())
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		second, err := fixture.store.Claim(ctx, fixture.selected(1), "synthetic-merged-second", testWorkerImageDigest())
		if err != nil || second == nil {
			tb.Fatal(err)
		}
		if _, err := fixture.store.Complete(withCompetitionJobAuthority(ctx, first), fixture.selected(0), "synthetic-merged-first", first.JobId,
			EvaluationOutcome{Error: submissionError("candidate_build_failed", "Synthetic retained failure."), distributedArchiveVerified: true}); err != nil {
			tb.Fatal(err)
		}
		fixture.clock.Advance(fixture.round.ClosesAt.Add(time.Second).Sub(fixture.clock.Now()))
		if result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); result != nil || !errors.Is(err, ErrReviewNotReady) {
			tb.Fatal("first host sealed while its sibling retained execution custody", result, err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-merged-successor", 5*time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.settings.automationLease = lease
		settings := fixture.selected(1)
		settings.executorReconcileExpiredOnly = true
		recovered, err := fixture.store.Claim(ctx, settings, "synthetic-merged-reconciler", testWorkerImageDigest())
		if err != nil || recovered == nil || recovered.JobId != second.JobId || recovered.AttemptCount != second.AttemptCount || !recovered.StartedAt.Equal(*second.StartedAt) || !recovered.ExecutionDeadline.Equal(*second.ExecutionDeadline) || recovered.SlotFencingToken <= second.SlotFencingToken {
			tb.Fatal("merged deadline reconciliation restarted the remote run", recovered, err)
		}
		args := competitionFinalizationHoldArgs("synthetic-merged-drained-hold")
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			tb.Fatal(err)
		}
		if _, err := fixture.store.Complete(withCompetitionJobAuthority(ctx, recovered), settings, "synthetic-merged-reconciler", recovered.JobId,
			EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "Synthetic original deadline exhausted."), distributedArchiveVerified: true}); err != nil {
			tb.Fatal("finalization hold prevented terminal reconciliation", err)
		}
		if result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); result != nil || !errors.Is(err, ErrFinalizationHeld) {
			tb.Fatal("drained distributed round bypassed finalization authority", result, err)
		}
		args.Active, args.ExpectedRevision, args.ReasonCode, args.IdempotencyKey = false, 1, "RECOVERY_APPROVED", "synthetic-merged-drained-release"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			tb.Fatal(err)
		}
		result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || result == nil || len(result.Seal.Entries) != 2 || len(result.Decisions) != 1 {
			tb.Fatal("fully reconciled released round did not seal exactly once", result, err)
		}
	})
}

// Exercise the existing final deadline recheck with ordinary acknowledged
// renewals, not enlarged leases, direct timestamp edits or wall-clock sleeps.
// The baseline-return hook alone crosses the original absolute job boundary.
func TestCompetitionDistributedMergedCompletionRechecksDeadlineAfterBaseline(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		job, err := fixture.store.Claim(ctx, settings, "synthetic-merged-deadline-worker", testWorkerImageDigest())
		if err != nil || job == nil || job.ExecutionDeadline == nil || job.StartedAt == nil {
			tb.Fatal("original deadline fixture did not obtain execution custody", job, err)
		}
		authority := withCompetitionJobAuthority(ctx, job)
		target := job.ExecutionDeadline.Add(-time.Second)
		step := time.Duration(settings.WorkerLeaseSeconds)*time.Second - time.Second
		if step <= 0 {
			tb.Fatal("fixture requires the unchanged ordinary worker lease")
		}
		// Move the owned clock using finite actual heartbeat/renewal calls.
		// Both authority leases are healthy before and after the deadline
		// boundary; none of their duration policies is widened for this test.
		for fixture.clock.Now().Before(target) {
			next := fixture.clock.Now().Add(step)
			if target.Before(next) {
				next = target
			}
			if !next.Add(time.Second).Before(settings.automationLease.LeaseExpiresAt) {
				renewed, err := fixture.store.RenewAutomationLease(ctx, settings, *settings.automationLease, 5*time.Minute)
				if err != nil {
					tb.Fatal("ordinary supervisor renewal did not preserve deadline fixture authority", err)
				}
				settings.automationLease, fixture.settings.automationLease = renewed, renewed
			}
			fixture.clock.Advance(next.Sub(fixture.clock.Now()))
			if err := fixture.store.Heartbeat(authority, settings, "synthetic-merged-deadline-worker", job.JobId); err != nil {
				tb.Fatal("ordinary heartbeat did not preserve the original run", err)
			}
		}
		renewed, err := fixture.store.RenewAutomationLease(ctx, settings, *settings.automationLease, 5*time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		settings.automationLease, fixture.settings.automationLease = renewed, renewed
		before := readCompetitionDistributedMergedHeartbeat(ctx, job.JobId)
		if !job.ExecutionDeadline.Before(before.jobLease) || !job.ExecutionDeadline.Before(before.slotLease) || !job.ExecutionDeadline.Before(renewed.LeaseExpiresAt) {
			tb.Fatal("deadline fixture accidentally exhausted another authority")
		}
		outcome := testSharedBaselineOutcome(tb, settings, job, EvaluationOutcome{Score: competitionRankingScore(90, 110), distributedArchiveVerified: true})
		outcome.Score.Diagnostics["g2_volume_policy"] = string(settings.EvaluationPolicy.G2VolumePolicy)
		outcome.Score.Diagnostics["scorer_version"] = settings.scorerVersion()
		hookCalls := 0
		fixture.store.afterCompletionBaselineForTest = func() {
			hookCalls++
			if hookCalls == 1 {
				fixture.clock.Advance(time.Second)
			}
		}
		if retry, err := fixture.store.Complete(authority, settings, "synthetic-merged-deadline-worker", job.JobId, outcome); err != nil || retry {
			tb.Fatal("deadline crossing did not become a terminal timeout", retry, err)
		}
		stored, err := fixture.store.GetJob(ctx, settings, job.JobId, &Principal{Id: "synthetic-operator", Role: "operator"})
		if err != nil || stored == nil || hookCalls != 1 || stored.State != "failed" || stored.Score != nil || stored.Error == nil || stored.Error.Code != "evaluation_time_budget_exhausted" ||
			stored.StartedAt == nil || !stored.StartedAt.Equal(*job.StartedAt) || stored.ExecutionDeadline == nil || !stored.ExecutionDeadline.Equal(*job.ExecutionDeadline) ||
			stored.AttemptCount != job.AttemptCount || stored.CompletedAt == nil || !stored.CompletedAt.Equal(*job.ExecutionDeadline) {
			tb.Fatal("baseline wait published a late score or reset the original execution clock", stored, err)
		}
	})
}
