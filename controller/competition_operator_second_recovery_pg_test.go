// A real first recovered claim/failure precedes every synthetic second grant.
// All source, audit, hold, slot, native-signature and claim guards stay active.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
	"github.com/urnetwork/server/model"
)

// Only the migration-compatibility fixture may request the exact predecessor;
// it remains a checked schema identity, not a permission to omit migrations.
type competitionSecondRecoveryFixtureMigrationHeadKey struct{}

// Embed the real owner so cleanup, cancellation and assertions stay attached
// to the isolated TestEnv while fixture descendants receive the same context.
type competitionSecondRecoveryFixtureContext struct {
	testing.TB
	ctx context.Context
}

// No production code consumes this test-only context value.
func (self *competitionSecondRecoveryFixtureContext) Context() context.Context {
	return self.ctx
}

// No production identity enters a fixture. The private Go seam and isolated
// schema's immutable scope predicate both name only this generated test job.
type competitionSecondRecoveryPgFixture struct {
	*competitionDistributedRecoveryPgFixture
	firstRequest model.CompetitionJobReevaluationRequest
	firstJob     *queuedJob
	firstAudit   []byte
}

// The prior recovery is authorized, normally claimed and genuinely completed
// through retained evaluator handback; no failed state or clock is fabricated.
func newCompetitionSecondRecoveryPgFixture(t *testing.T, tb testing.TB) *competitionSecondRecoveryPgFixture {
	tb.Helper()
	base := newCompetitionDistributedRecoveryPgFixtureWithPatchRetry(t, tb, true)
	fixture := &competitionSecondRecoveryPgFixture{competitionDistributedRecoveryPgFixture: base,
		firstRequest: competitionRecoveryTestCopy(tb, base.request)}
	if _, err := base.authorize(tb, tb.Context()); err != nil {
		tb.Fatal("first recovery could not be authorized", err)
	}
	base.releaseHold(tb)
	job, err := base.store.Claim(tb.Context(), base.selected(0), "synthetic-first-recovery-worker", base.runtime.WorkerImageDigest)
	if err != nil || job == nil || job.EvaluationCycle != 1 || job.CycleStartedAt == nil {
		tb.Fatal("first recovery did not acquire its actual claim clock", err)
	}
	base.observeCurrentDatabaseClock(tb)
	outcome, status := competitionOperatorPatchRetryRetainedFailureForTest(tb, base.selected(0), job)
	if retry, err := base.store.Complete(withCompetitionJobAuthority(tb.Context(), job), base.selected(0), "synthetic-first-recovery-worker", job.JobId, outcome); err != nil || retry {
		tb.Fatal("first recovery could not retain its actual generic failure", retry, err)
	}
	outcome.closeReceiveCustody()
	fixture.firstJob, base.originalStatus = job, status
	args := base.hold.Request
	args.Active, args.ExpectedRevision, args.IdempotencyKey = true, base.hold.Revision, "synthetic-second-recovery-hold"
	base.hold, err = base.store.SetAutomationHold(tb.Context(), base.settings, base.principal.Id, args)
	if err != nil {
		tb.Fatal(err)
	}
	now := base.observeCurrentDatabaseClock(tb)
	base.settings.secondRecoveryJobIdForTest = job.JobId
	server.Tx(tb.Context(), func(tx server.PgTx) {
		// TestEnv owns this disposable schema; the production predicate is never
		// widened or parameterized by a request, token, runtime or settings file.
		server.RaisePgResult(tx.Exec(tb.Context(), `CREATE OR REPLACE FUNCTION competition_second_recovery_job_allowed(target uuid) RETURNS boolean LANGUAGE sql IMMUTABLE AS $synthetic_scope$ SELECT target='`+job.JobId.String()+`'::uuid $synthetic_scope$`))
	})
	base.runtime.Schema = 3
	runtimeDigest := competitionRecoveryTestDigest(tb, base.runtime)
	if err := BindCompetitionJobReevaluationRuntime(base.settings, base.runtime, runtimeDigest); err != nil {
		tb.Fatal(err)
	}
	snapshot := readCompetitionReevaluationPgSnapshot(tb.Context(), job)
	fixture.firstAudit = append([]byte(nil), snapshot.audit...)
	prior, err := base.store.GetJob(tb.Context(), base.settings, job.JobId, base.principal)
	if err != nil || prior == nil || prior.Reevaluation == nil || prior.Reevaluation.StartedAt == nil {
		tb.Fatal("first audit cannot supply its immutable public projection", err)
	}
	a := competitionRecoveryTestCopy(tb, fixture.firstRequest.Authorization)
	a.Schema, a.ExpectedCycle, a.ExpectedAttemptCount, a.IncidentAttempt = 3, 1, job.AttemptCount, job.AttemptCount
	a.ExpectedTerminalSnapshot = snapshot.job
	a.ExpectedTerminalSha256, err = CompetitionJobReevaluationSnapshotDigest(snapshot.job)
	if err != nil {
		tb.Fatal(err)
	}
	a.RuntimeQualificationSha256, a.AuthorizedAt, a.ExpiresAt = runtimeDigest, now.Add(-time.Second), now.Add(time.Hour)
	a.SecondRecovery = &model.CompetitionOperatorSecondRecovery{Schema: 1, JobId: job.JobId,
		PreviousReceiptSha256: prior.Reevaluation.ReceiptSha256, ApprovalEvidenceSha256: strings.Repeat("8", 64)}
	a.Distributed.OriginalRunId = competitionagent.RunId(job.JobId.String(), 1, job.AttemptCount)
	a.Distributed.OriginalStartedAt, a.Distributed.OriginalDeadlineAt = *job.CycleStartedAt, *job.ExecutionDeadline
	a.Distributed.OriginalHostId, a.Distributed.OriginalSlotId, a.Distributed.OriginalFencingToken = job.ExecutorHostId, job.SlotId, job.SlotFencingToken
	a.Distributed.NativeFencingToken = status.FencingToken
	a.Distributed.OriginalManifestSha256, a.Distributed.OriginalRequestSha256 = status.ManifestSha256, status.RequestSha256
	server.Db(tb.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(tb.Context(), `SELECT event_id,payload_sha256 FROM competition_job_event WHERE job_id=$1 AND event_type='failed' ORDER BY event_id DESC LIMIT 1`, job.JobId).Scan(&a.IncidentEventId, &a.IncidentEventSha256))
	})
	base.request = model.CompetitionJobReevaluationRequest{IdempotencyKey: "synthetic-second-recovery", Authorization: a}
	base.refreshIdleCut(tb)
	base.calls = 0
	return fixture
}

// The independent append-only row is included in every atomicity comparison.
func (self *competitionSecondRecoveryPgFixture) secondAudit(t testing.TB) []byte {
	t.Helper()
	var value []byte
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(a)),'[]'::jsonb) FROM competition_job_second_reevaluation a WHERE job_id=$1`, self.original.JobId).Scan(&value))
	})
	return value
}

// Original order, principal, events and first receipt survive the exact second
// audit. Exact replays are receipt-only, while another key grants no third cycle.
func TestCompetitionSecondRecoveryPgAppendOnlyLifecycleAndFreshClaim(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionSecondRecoveryPgFixture(t, tb)
		before := fixture.snapshot(tb.Context())
		receipt, err := fixture.authorize(tb, tb.Context())
		if err != nil || receipt == nil || receipt.Schema != 3 || receipt.Cycle != 2 || receipt.StartedAt != nil || fixture.calls != 3 ||
			receipt.SecondRecovery == nil || receipt.SecondRecovery.PreviousRecovery == nil || receipt.SecondRecovery.PreviousRecovery.StartedAt == nil {
			tb.Fatal("exact second recovery did not seal its predecessor", err, fixture.calls)
		}
		after, secondAudit := fixture.snapshot(tb.Context()), fixture.secondAudit(tb)
		requireCompetitionReevaluationOriginalEvidence(tb, before.job, after.job)
		if !bytes.Equal(fixture.firstAudit, after.job.audit) {
			tb.Fatal("second recovery rewrote the first immutable audit")
		}
		var terminal []byte
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT original_terminal_json FROM competition_job_second_reevaluation WHERE job_id=$1`, fixture.original.JobId).Scan(&terminal))
		})
		if !bytes.Equal(terminal, before.job.job) {
			tb.Fatal("second audit did not retain exact failed cycle-one terminal")
		}
		queued, err := fixture.store.GetJob(tb.Context(), fixture.settings, fixture.original.JobId, fixture.principal)
		if err != nil || queued == nil || queued.EvaluationCycle != 2 || queued.ExecutionDeadline != nil || queued.CycleStartedAt != nil || queued.ExecutorHostId != "" ||
			!queued.SubmittedAt.Equal(fixture.original.SubmittedAt) || !queued.StartedAt.Equal(*fixture.original.StartedAt) {
			tb.Fatal("second grant changed original admission or allocated a clock", err)
		}
		poll := scoreJobView(queued, &Principal{Id: "synthetic-unprivileged-owner", Role: "submitter"}, fixture.clock.Now())
		public, err := json.Marshal(poll)
		if err != nil || poll.Reevaluation == nil || poll.Reevaluation.Cycle != 2 || poll.Score != nil || poll.EvalError != nil ||
			bytes.Contains(public, []byte("distributed_proof")) || bytes.Contains(public, []byte("expected_terminal_snapshot")) || bytes.Contains(public, []byte("second_recovery_receipt_json")) {
			tb.Fatal("owner projection leaked private audit or lost public successor", err)
		}
		if replay, err := fixture.authorize(tb, tb.Context()); err != nil || replay == nil || fixture.calls != 3 ||
			!reflect.DeepEqual(after, fixture.snapshot(tb.Context())) || !bytes.Equal(secondAudit, fixture.secondAudit(tb)) {
			tb.Fatal("second receipt replay mutated job/fences/audit or reobserved hosts", err)
		}
		secondRequest := competitionRecoveryTestCopy(tb, fixture.request)
		fixture.request = fixture.firstRequest
		if firstReplay, err := fixture.authorize(tb, tb.Context()); err != nil || firstReplay == nil || firstReplay.Cycle != 1 || fixture.calls != 3 || !bytes.Equal(fixture.firstAudit, fixture.snapshot(tb.Context()).job.audit) {
			tb.Fatal("second recovery changed replay of the original first receipt", err)
		}
		fixture.request = secondRequest
		fixture.request.IdempotencyKey = "synthetic-never-third-cycle"
		if next, err := fixture.authorize(tb, tb.Context()); err == nil || next != nil || !reflect.DeepEqual(after, fixture.snapshot(tb.Context())) || !bytes.Equal(secondAudit, fixture.secondAudit(tb)) {
			tb.Fatal("a different key granted an unauthorized further cycle", err)
		}
		fixture.request = secondRequest
		fixture.releaseHold(tb)
		var beforeClaim, afterClaim time.Time
		server.Db(tb.Context(), func(conn server.PgConn) {
			beforeClaim, err = competitionIntakeFirstDatabaseTime(tb.Context(), conn)
			server.Raise(err)
		})
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-second-recovery-worker", fixture.runtime.WorkerImageDigest)
		server.Db(tb.Context(), func(conn server.PgConn) {
			afterClaim, err = competitionIntakeFirstDatabaseTime(tb.Context(), conn)
			server.Raise(err)
		})
		if err != nil || job == nil || job.EvaluationCycle != 2 || job.AttemptCount != 3 || job.CycleAttemptOffset != 2 || job.CycleStartedAt == nil ||
			job.CycleStartedAt.Before(beforeClaim) || afterClaim.Before(*job.CycleStartedAt) || job.ExecutionDeadline == nil || !job.ExecutionDeadline.Equal(job.CycleStartedAt.Add(3*time.Hour)) ||
			!job.SubmittedAt.Equal(fixture.original.SubmittedAt) || !job.StartedAt.Equal(*fixture.original.StartedAt) {
			tb.Fatal("second claim did not allocate exactly its fresh three-hour budget", err)
		}
		if err := fixture.store.Heartbeat(withCompetitionJobAuthority(tb.Context(), fixture.firstJob), fixture.selected(0), "synthetic-first-recovery-worker", job.JobId); err == nil {
			tb.Fatal("stale first-recovery authority renewed cycle two")
		}
		oldOutcome, _ := competitionOperatorPatchRetryRetainedFailureForTest(tb, fixture.selected(0), fixture.firstJob)
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), fixture.firstJob), fixture.selected(0), "synthetic-first-recovery-worker", job.JobId, oldOutcome); err == nil || retry {
			tb.Fatal("stale cycle-one outcome changed cycle two", retry, err)
		}
		oldOutcome.closeReceiveCustody()
		fixture.observeCurrentDatabaseClock(tb)
		outcome, _ := competitionOperatorPatchRetryRetainedFailureForTest(tb, fixture.selected(0), job)
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), job), fixture.selected(0), "synthetic-second-recovery-worker", job.JobId, outcome); err != nil || retry {
			tb.Fatal("second recovery could not use ordinary guarded completion", retry, err)
		}
		outcome.closeReceiveCustody()
		fixture.request.Authorization.ExpectedCycle = 2
		fixture.request.AuthorizationSha256 = competitionRecoveryTestDigest(tb, fixture.request.Authorization)
		if next, err := fixture.authorize(tb, tb.Context()); err == nil || next != nil {
			tb.Fatal("completed cycle two acquired a third recovery")
		}
	})
}

// Rehashed malformed requests cannot bypass the locked terminal, immutable
// predecessor or separately bound schema-three runtime qualification.
func TestCompetitionSecondRecoveryPgRejectsScopeProofRuntimeAndNativeDriftAtomically(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionSecondRecoveryPgFixture(t, tb)
		original := competitionRecoveryTestCopy(tb, fixture.request)
		before, audit := fixture.snapshot(tb.Context()), fixture.secondAudit(tb)
		for _, change := range []func(*model.CompetitionJobReevaluationAuthorization){
			func(a *model.CompetitionJobReevaluationAuthorization) {
				a.SecondRecovery.PreviousReceiptSha256 = strings.Repeat("f", 64)
			},
			func(a *model.CompetitionJobReevaluationAuthorization) { a.SecondRecovery.ApprovalEvidenceSha256 = "" },
			func(a *model.CompetitionJobReevaluationAuthorization) { a.SecondRecovery.JobId = server.NewId() },
			func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedCycle = 2 },
			func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedAttemptCount++ },
			func(a *model.CompetitionJobReevaluationAuthorization) {
				a.Distributed.OriginalRunId = competitionagent.RunId(a.JobId.String(), 0, a.ExpectedAttemptCount)
			},
			func(a *model.CompetitionJobReevaluationAuthorization) {
				a.Distributed.SourceLockSha256 = strings.Repeat("f", 64)
			},
			func(a *model.CompetitionJobReevaluationAuthorization) {
				a.ApiImageDigest = "sha256:" + strings.Repeat("f", 64)
			},
		} {
			fixture.request = competitionRecoveryTestCopy(tb, original)
			change(&fixture.request.Authorization)
			fixture.request.AuthorizationSha256 = competitionRecoveryTestDigest(tb, fixture.request.Authorization)
			if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) || !bytes.Equal(audit, fixture.secondAudit(tb)) {
				tb.Fatal("changed second-recovery pin partially changed durable state", err)
			}
		}
		fixture.request = original
		fixture.settings.secondRecoveryJobIdForTest = server.NewId()
		if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("wrong job scope acquired second recovery", err)
		}
		fixture.settings.secondRecoveryJobIdForTest = fixture.original.JobId
		fixture.mutateNative = func(response *competitionagent.Response) {
			if response.Identity != nil {
				response.Identity.Cycle = 0
			} else {
				response.State = "occupied"
			}
		}
		if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) || !bytes.Equal(audit, fixture.secondAudit(tb)) {
			tb.Fatal("stale native identity or occupied host partially granted recovery", err)
		}
		fixture.mutateNative = nil
		fixture.runtime.Schema = 2
		runtimeDigest := competitionRecoveryTestDigest(tb, fixture.runtime)
		if err := BindCompetitionJobReevaluationRuntime(fixture.settings, fixture.runtime, runtimeDigest); err != nil {
			tb.Fatal(err)
		}
		fixture.request.Authorization.RuntimeQualificationSha256 = runtimeDigest
		fixture.request.AuthorizationSha256 = competitionRecoveryTestDigest(tb, fixture.request.Authorization)
		if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) || !bytes.Equal(audit, fixture.secondAudit(tb)) {
			tb.Fatal("old qualification was borrowed for schema-three grant", err)
		}
	})
}

// Existing cycle-zero workers remain legal across the normal migration-first
// boundary; no second grant is possible until compatible code is deployed.
func TestCompetitionSecondRecoveryPgMigrationPreservesRunningReleaseQueries(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionSecondRecoveryMigrationVersion()
		server.ApplyDbMigrationsUpTo(tb.Context(), version-1)
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		worker := "synthetic-pre-second-recovery-worker"
		job, err := fixture.store.Claim(tb.Context(), settings, worker, testWorkerImageDigest())
		if err != nil || job == nil || job.EvaluationCycle != 0 {
			tb.Fatal("old release claim failed", err)
		}
		server.ApplyDbMigrationsUpTo(tb.Context(), version)
		fixture.clock.Advance(time.Second)
		ctx := withCompetitionJobAuthority(context.Background(), job)
		if err := fixture.store.Heartbeat(ctx, settings, worker, job.JobId); err != nil {
			tb.Fatal("migration fenced old heartbeat", err)
		}
		if retry, err := fixture.store.Complete(ctx, settings, worker, job.JobId, competitionPreparedIntakeCompletionOutcome(tb, settings, job)); err != nil || retry {
			tb.Fatal("migration rejected old completion", retry, err)
		}
		if _, err := fixture.store.GetJob(tb.Context(), settings, job.JobId, &Principal{Id: "synthetic-read-operator", Role: "operator"}); err != nil {
			tb.Fatal(fmt.Errorf("migration invalidated old polling shape: %w", err))
		}
	})
}

// A previously deployed cycle-one worker and receipt query keep working while
// the additive schema is installed. No second audit exists or is needed here.
func TestCompetitionSecondRecoveryPgMigrationPreservesRunningFirstRecovery(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionSecondRecoveryMigrationVersion()
		server.ApplyDbMigrationsUpTo(tb.Context(), version-1)
		predecessor := &competitionSecondRecoveryFixtureContext{TB: tb,
			ctx: context.WithValue(tb.Context(), competitionSecondRecoveryFixtureMigrationHeadKey{}, version-1)}
		fixture := newCompetitionDistributedRecoveryPgFixtureWithPatchRetry(t, predecessor, true)
		if _, err := fixture.authorize(tb, tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.releaseHold(tb)
		worker := "synthetic-running-first-recovery"
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), worker, fixture.runtime.WorkerImageDigest)
		if err != nil || job == nil || job.EvaluationCycle != 1 {
			tb.Fatal("previous release could not claim its first recovery", err)
		}
		server.ApplyDbMigrationsUpTo(tb.Context(), version)
		fixture.observeCurrentDatabaseClock(tb)
		ctx := withCompetitionJobAuthority(tb.Context(), job)
		if err := fixture.store.Heartbeat(ctx, fixture.selected(0), worker, job.JobId); err != nil {
			tb.Fatal("additive migration fenced old cycle-one heartbeat", err)
		}
		outcome, _ := competitionOperatorPatchRetryRetainedFailureForTest(tb, fixture.selected(0), job)
		if retry, err := fixture.store.Complete(ctx, fixture.selected(0), worker, job.JobId, outcome); err != nil || retry {
			tb.Fatal("additive migration rejected old cycle-one completion", retry, err)
		}
		outcome.closeReceiveCustody()
		currentProjection := "CASE WHEN j.evaluation_cycle=2 THEN to_jsonb(j)->'second_recovery_receipt_json'\n\t        ELSE (SELECT receipt_json FROM competition_job_reevaluation recovery WHERE recovery.job_id=j.job_id) END"
		if strings.Count(jobSelect, currentProjection) != 1 {
			tb.Fatal("old polling shape test lost the actual successor query")
		}
		oldQuery := strings.Replace(jobSelect, currentProjection, "(SELECT receipt_json FROM competition_job_reevaluation recovery WHERE recovery.job_id=j.job_id)", 1) + ` WHERE j.job_id=$1`
		server.Db(tb.Context(), func(conn server.PgConn) {
			old, err := scanJob(conn.QueryRow(tb.Context(), oldQuery, job.JobId), false, fixture.clock.Now())
			if err != nil || old == nil || old.EvaluationCycle != 1 || old.Reevaluation == nil || old.Reevaluation.Schema != 2 || old.State != "failed" {
				tb.Fatal("old API/worker receipt shape stopped working after migration", err)
			}
		})
	})
}

// The actual DDL's validating CHECK owns the table lock. A test-local gate
// records entry before blocking; only removing the new bound restores the old
// success. No polling, secondary connection or elapsed-time guess is required.
func TestCompetitionSecondRecoveryPgMigrationBoundsValidationLockHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionSecondRecoveryMigrationVersion()
		server.ApplyDbMigrationsUpTo(tb.Context(), version-1)
		newCompetitionDistributedPgFixture(t, tb, true, true)
		migration := server.CompetitionSecondRecoveryMigrationSql()
		const statementBound = "SET LOCAL statement_timeout='30s';\n"
		const validation = `ALTER TABLE competition_job ADD CONSTRAINT competition_job_evaluation_cycle_check CHECK (
 evaluation_cycle IN (0,1) OR (evaluation_cycle=2 AND competition_second_recovery_job_allowed(job_id)));`
		if strings.Count(migration, statementBound) != 1 || strings.Count(migration, validation) != 1 {
			tb.Fatal("actual migration lost its unique statement bound or validating CHECK")
		}
		instrumentedValidation := strings.TrimSuffix(strings.Replace(validation, "CHECK (", "CHECK (pg_temp.second_recovery_validation_gate() AND (", 1), ";") + ");"
		migration = strings.Replace(migration, validation, instrumentedValidation, 1)
		server.MaintenanceDb(tb.Context(), func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(tb.Context(), `CREATE TEMP SEQUENCE second_recovery_validation_calls;
CREATE FUNCTION pg_temp.second_recovery_validation_gate() RETURNS boolean LANGUAGE plpgsql AS $gate$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND relation='competition_job'::regclass
  AND mode='AccessExclusiveLock' AND granted) THEN
  RAISE EXCEPTION 'validation did not hold the actual table lock';
 END IF;
 PERFORM nextval('pg_temp.second_recovery_validation_calls');
 PERFORM pg_sleep(1);
 RETURN true;
END;
$gate$;`))
			var initialStatementTimeout, initialLockTimeout string
			server.Raise(conn.QueryRow(tb.Context(), `SELECT current_setting('statement_timeout'),current_setting('lock_timeout')`).Scan(&initialStatementTimeout, &initialLockTimeout))
			var previousCalls int64
			for _, control := range []struct {
				name        string
				sql         string
				wantTimeout bool
			}{
				{name: "old lock bound only", sql: strings.Replace(migration, statementBound, "", 1)},
				// The static test pins the full healthy 30s budget; shorten only
				// this disposable execution's exact bound to exercise cancellation.
				{name: "bounded validation", sql: strings.Replace(migration, statementBound, "SET LOCAL statement_timeout='100ms';\n", 1), wantTimeout: true},
			} {
				func() {
					tx, err := conn.Begin(tb.Context())
					if err != nil {
						tb.Fatal(err)
					}
					defer tx.Rollback(tb.Context())
					server.RaisePgResult(tx.Exec(tb.Context(), `SET LOCAL statement_timeout='0'; SET LOCAL lock_timeout='0';`))
					_, err = tx.Exec(tb.Context(), control.sql)
					var postgres *pgconn.PgError
					if control.wantTimeout {
						if !errors.As(err, &postgres) || postgres.Code != "57014" || !strings.Contains(postgres.Message, "statement timeout") || !strings.Contains(postgres.Where, "second_recovery_validation_gate") {
							tb.Fatal("validating DDL was not canceled by the statement bound", err)
						}
					} else if err != nil {
						tb.Fatal("lock-timeout-only counterfactual did not complete its validation", err)
					}
				}()
				var calls int64
				server.Raise(conn.QueryRow(tb.Context(), `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM pg_temp.second_recovery_validation_calls`).Scan(&calls))
				var statementTimeout, lockTimeout string
				var retainedTableLock, retainedColumn bool
				server.Raise(conn.QueryRow(tb.Context(), `SELECT current_setting('statement_timeout'),current_setting('lock_timeout'),
 EXISTS(SELECT 1 FROM pg_locks WHERE pid=pg_backend_pid() AND relation='competition_job'::regclass AND mode='AccessExclusiveLock' AND granted),
 EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='competition_job'::regclass AND attname='second_recovery_receipt_json' AND NOT attisdropped)`).Scan(&statementTimeout, &lockTimeout, &retainedTableLock, &retainedColumn))
				if calls <= previousCalls || retainedTableLock || retainedColumn || statementTimeout != initialStatementTimeout || lockTimeout != initialLockTimeout {
					tb.Fatal("validation did not enter its lock-held gate or rollback retained DDL, locks or local timeouts", control.name, calls, previousCalls, retainedTableLock, retainedColumn, statementTimeout, lockTimeout)
				}
				previousCalls = calls
				tb.Logf("%s reached actual lock-held CHECK validation; timeout=%t; rollback restored schema, locks and session limits", control.name, control.wantTimeout)
			}
		}, server.OptReadWrite(), server.OptNoRetry())
	})
}

// The actual production audit expression is parsed by PostgreSQL, not copied
// into a synthetic validator. Removing only its grouping reproduces SQL42725.
func TestCompetitionSecondRecoveryPgAuditProjectionSqlPrecedence(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		var expression string
		for _, line := range strings.Split(server.CompetitionSecondRecoveryBodiesSql().Audit, "\n") {
			if strings.Contains(line, "next_audit.receipt_json->'second_recovery'") && strings.HasSuffix(line, " IS DISTINCT FROM second") {
				if expression != "" {
					tb.Fatal("actual audit has multiple second-recovery projection comparisons")
				}
				expression = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "OR "), " IS DISTINCT FROM second")
			}
		}
		const grouped = "(next_audit.receipt_json->'second_recovery')"
		if expression == "" || strings.Count(expression, grouped) != 1 {
			tb.Fatal("actual audit lost its explicitly grouped JSON extraction")
		}
		const receipt = `{"second_recovery":{"synthetic_retained":"keep","previous_recovery":{"synthetic_prior":"omit"}}}`
		const expected = `{"synthetic_retained":"keep"}`
		query := func(value string) string {
			return "WITH next_audit(receipt_json) AS (VALUES($1::jsonb)) SELECT " + value + " = $2::jsonb FROM next_audit"
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var matches bool
			if err := conn.QueryRow(tb.Context(), query(expression), receipt, expected).Scan(&matches); err != nil || !matches {
				tb.Fatal("actual production projection did not preserve exactly the non-predecessor fields", err)
			}
			old := strings.Replace(expression, grouped, "next_audit.receipt_json->'second_recovery'", 1)
			err := conn.QueryRow(tb.Context(), query(old), receipt, expected).Scan(&matches)
			var postgres *pgconn.PgError
			if !errors.As(err, &postgres) || postgres.Code != "42725" {
				tb.Fatal("literal precedence counterfactual did not reproduce ambiguous unknown-minus-unknown", err)
			}
		})
	})
}

// Both callers pass the first held/idle gate before either is allowed to
// finish native observation. Final serialization must yield one audit/transition.
func competitionSecondRecoveryPgConcurrentGrants(t *testing.T, identical bool) {
	t.Helper()
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionSecondRecoveryPgFixture(t, tb)
		started, release := make(chan struct{}, 2), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		type result struct {
			receipt *model.CompetitionJobReevaluationReceipt
			err     error
		}
		results := make(chan result, 2)
		for index := range 2 {
			request := competitionRecoveryTestCopy(tb, fixture.request)
			if !identical {
				request.IdempotencyKey += fmt.Sprint(index)
			}
			go func() {
				var once sync.Once
				calls := 0
				receipt, err := fixture.store.authorizeDistributedJobReevaluation(tb.Context(), fixture.settings, fixture.principal, request,
					func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
						return competitionDistributedRecoveryTestTransport{t: tb, host: host, auth: request.Authorization, round: fixture.round, calls: &calls, originalStatus: fixture.originalStatus,
							before: func(competitionagent.Request) { once.Do(func() { started <- struct{}{}; <-release }) }}, nil
					})
				results <- result{receipt: receipt, err: err}
			}()
		}
		for range 2 {
			select {
			case <-started:
			case early := <-results:
				tb.Fatal("concurrent grant failed before the deterministic observation barrier", early.err)
			}
		}
		unblock()
		successes := 0
		var digest string
		for range 2 {
			value := <-results
			if value.err == nil && value.receipt != nil {
				successes++
				if digest != "" && digest != value.receipt.ReceiptSha256 {
					tb.Fatal("identical requests committed different receipts")
				}
				digest = value.receipt.ReceiptSha256
			} else if identical {
				tb.Fatal("identical concurrent replay failed", value.err)
			}
		}
		want := 1
		if identical {
			want = 2
		}
		if successes != want {
			tb.Fatal("concurrent requests granted an unexpected number of receipts", successes, want)
		}
		var auditCount, cycle int
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job_second_reevaluation WHERE job_id=$1`, fixture.original.JobId).Scan(&auditCount))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT evaluation_cycle FROM competition_job WHERE job_id=$1`, fixture.original.JobId).Scan(&cycle))
			for _, slot := range fixture.request.Authorization.Distributed.IdleSlots {
				var fence int64
				server.Raise(conn.QueryRow(tb.Context(), `SELECT fencing_token FROM competition_execution_slot WHERE slot_id=$1`, slot.SlotId).Scan(&fence))
				if fence != slot.FencingToken+1 {
					tb.Fatal("concurrent grant advanced an idle fence other than exactly once")
				}
			}
		})
		if auditCount != 1 || cycle != 2 || !bytes.Equal(fixture.firstAudit, fixture.snapshot(tb.Context()).job.audit) {
			tb.Fatal("concurrent grant changed original receipt or created multiple successors")
		}
	})
}

// Same-key races converge to the identical immutable second receipt.
func TestCompetitionSecondRecoveryPgConcurrentIdenticalRequestsAreIdempotent(t *testing.T) {
	competitionSecondRecoveryPgConcurrentGrants(t, true)
}

// A fresh key is not permission for another recovery or another fence advance.
func TestCompetitionSecondRecoveryPgConcurrentDifferentKeysGrantOnlyOnce(t *testing.T) {
	competitionSecondRecoveryPgConcurrentGrants(t, false)
}

// The projection is mechanically derived from the audit, then immutable. The
// real PostgreSQL scanner also refuses malformed, stale and wrong-job projections.
func TestCompetitionSecondRecoveryPgProjectionIsImmutableAndRejectsTampering(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionSecondRecoveryPgFixture(t, tb)
		receipt, err := fixture.authorize(tb, tb.Context())
		if err != nil {
			tb.Fatal(err)
		}
		before, audit := fixture.snapshot(tb.Context()), fixture.secondAudit(tb)
		server.Db(tb.Context(), func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(tb.Context(), `SELECT j.second_recovery_receipt_json=(a.receipt_json-'distributed_proof') FROM competition_job j JOIN competition_job_second_reevaluation a USING(job_id) WHERE job_id=$1`, fixture.original.JobId).Scan(&exact))
			if !exact {
				tb.Fatal("projection differs from the immutable public audit projection")
			}
			for _, statement := range []string{
				`UPDATE competition_job SET second_recovery_receipt_json=NULL WHERE job_id=$1`,
				`UPDATE competition_job SET second_recovery_receipt_json=jsonb_set(second_recovery_receipt_json,'{evidence_sha256}',to_jsonb(repeat('f',64))) WHERE job_id=$1`,
				`UPDATE competition_job_second_reevaluation SET receipt_json=receipt_json-'second_recovery' WHERE job_id=$1`,
				`DELETE FROM competition_job_second_reevaluation WHERE job_id=$1`,
				`DELETE FROM competition_job_reevaluation WHERE job_id=$1`,
			} {
				if _, err := conn.Exec(tb.Context(), statement, fixture.original.JobId); err == nil {
					tb.Fatal("immutable projection or audit could be changed", statement)
				}
			}
		})
		if !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) || !bytes.Equal(audit, fixture.secondAudit(tb)) {
			tb.Fatal("rejected projection changes mutated persisted history")
		}
		projection := competitionRecoveryTestCopy(tb, *receipt)
		projection.DistributedProof = nil
		needle := "to_jsonb(j)->'second_recovery_receipt_json'"
		if strings.Count(jobSelect, needle) != 1 {
			tb.Fatal("projection scanner test lost the actual query seam")
		}
		query := strings.Replace(jobSelect, needle, "$2::jsonb", 1) + ` WHERE j.job_id=$1`
		for _, change := range []func(*model.CompetitionJobReevaluationReceipt){
			func(r *model.CompetitionJobReevaluationReceipt) {
				r.JobId = server.NewId()
				sealApexReevaluationCycle(tb, &r.CompetitionJobReevaluationCycle)
			},
			func(r *model.CompetitionJobReevaluationReceipt) { r.Cycle = 1 },
			func(r *model.CompetitionJobReevaluationReceipt) { r.EvidenceSha256 = strings.Repeat("f", 64) },
			func(r *model.CompetitionJobReevaluationReceipt) {
				r.SecondRecovery.PreviousReceiptSha256 = strings.Repeat("f", 64)
				sealApexReevaluationCycle(tb, &r.CompetitionJobReevaluationCycle)
			},
			func(r *model.CompetitionJobReevaluationReceipt) {
				r.DistributedProof = json.RawMessage(`{"synthetic_private":true}`)
			},
		} {
			changed := competitionRecoveryTestCopy(tb, projection)
			change(&changed)
			encoded, err := json.Marshal(changed)
			if err != nil {
				tb.Fatal(err)
			}
			server.Db(tb.Context(), func(conn server.PgConn) {
				if job, err := scanJob(conn.QueryRow(tb.Context(), query, fixture.original.JobId, encoded), false, fixture.clock.Now()); err == nil || job != nil {
					tb.Fatal("actual scanner accepted a tampered/stale/wrong-job owner projection")
				}
			})
		}
	})
}

// The legacy and successor journals share the existing scope's request-key
// namespace; a reused first key cannot authorize a second transition.
func TestCompetitionSecondRecoveryPgRejectsFirstRecoveryIdempotencyKeyAtomically(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionSecondRecoveryPgFixture(t, tb)
		before, audit := fixture.snapshot(tb.Context()), fixture.secondAudit(tb)
		fixture.request.IdempotencyKey = fixture.firstRequest.IdempotencyKey
		if receipt, err := fixture.authorize(tb, tb.Context()); err == nil || receipt != nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) || !bytes.Equal(audit, fixture.secondAudit(tb)) {
			tb.Fatal("reused first-recovery request key partially granted a successor", err)
		}
	})
}
