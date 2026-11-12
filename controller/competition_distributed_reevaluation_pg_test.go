// Disposable PostgreSQL lifecycle regressions retain real intake/source,
// baseline, host, immutable audit and distributed fencing guards throughout.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
	"github.com/urnetwork/server/model"
)

// Only the transport is synthetic, with real signatures from fixture-only
// keys. The original job actually claims, reconciles and expires via the store.
type competitionDistributedRecoveryPgFixture struct {
	*competitionDistributedPgFixture
	original     *queuedJob
	principal    *Principal
	runtime      model.CompetitionJobReevaluationRuntime
	request      model.CompetitionJobReevaluationRequest
	hold         *model.CompetitionAutomationHold
	calls        int
	beforeNative func(competitionagent.Request)
	mutateNative func(*competitionagent.Response)
}

// Backdated application history is separate from a live SQL supervisor lease
// and current host qualification. No production trigger or fence is disabled.
func newCompetitionDistributedRecoveryPgFixture(t *testing.T, tb testing.TB) *competitionDistributedRecoveryPgFixture {
	tb.Helper()
	ctx := tb.Context()
	var databaseNow time.Time
	server.Db(ctx, func(conn server.PgConn) {
		var err error
		databaseNow, err = competitionIntakeFirstDatabaseTime(ctx, conn)
		server.Raise(err)
	})
	// The complete original budget has expired, while the independent default
	// baseline-preparation cleanup lease remains live during synthetic setup.
	base := newCompetitionPreparedAnnouncedCompletionFixtureAt(t, tb, databaseNow.Add(-3*time.Hour-time.Second))
	fixture := &competitionDistributedRecoveryPgFixture{competitionDistributedPgFixture: base}
	selected := base.selected(0)
	job, err := base.store.Claim(ctx, selected, "synthetic-original-worker", testWorkerImageDigest())
	if err != nil || job == nil || job.ExecutionDeadline == nil {
		tb.Fatal("guarded historical original claim failed", err)
	}
	base.clock.Advance(3*time.Hour + time.Second)
	job, err = base.store.Claim(ctx, selected, "synthetic-original-successor", testWorkerImageDigest())
	if err != nil || job == nil || job.AttemptCount != 1 {
		tb.Fatal("same original attempt failed historical deadline reconciliation", err)
	}
	if retry, err := base.store.Complete(withCompetitionJobAuthority(ctx, job), selected, "synthetic-original-successor", job.JobId, EvaluationOutcome{
		Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic original infrastructure budget exhausted"),
	}); err != nil || retry {
		tb.Fatal("genuine distributed original did not reach infrastructure terminal", retry, err)
	}
	fixture.original = job
	server.Db(ctx, func(conn server.PgConn) {
		var err error
		databaseNow, err = competitionIntakeFirstDatabaseTime(ctx, conn)
		server.Raise(err)
	})
	base.clock.Advance(databaseNow.Sub(base.clock.Now()))
	fixture.principal = &Principal{Id: "synthetic-distributed-recovery-operator", Role: "operator", Environment: base.settings.AutomationPolicy.Environment,
		CompetitionId: base.settings.CompetitionId, SeasonId: base.settings.AutomationPolicy.SeasonId}
	fixture.hold, err = base.store.SetAutomationHold(ctx, base.settings, fixture.principal.Id, model.CompetitionAutomationHoldArgs{Kind: "evaluation", Active: true,
		IdempotencyKey: "synthetic-distributed-recovery-hold", ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic all-host recovery qualification.", EvidenceSha256: strings.Repeat("a", 64)})
	if err != nil {
		tb.Fatal(err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		for _, host := range base.settings.DistributedEvaluationPolicy.Hosts {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_execution_slot(slot_id,host_id) VALUES($1,$2) ON CONFLICT(slot_id) DO NOTHING`, host.SlotId, host.HostId))
		}
	})
	fixture.runtime = model.CompetitionJobReevaluationRuntime{Schema: 2, Kind: model.CompetitionDistributedJobReevaluationRuntimeKind,
		Capability: model.CompetitionDistributedJobReevaluationCapability, Accepted: true, IncidentClasses: []string{model.CompetitionJobReevaluationDistributedInfrastructure},
		SourceCommit: strings.Repeat("1", 40), WorkerSha256: strings.Repeat("2", 64), WorkerImageDigest: "sha256:" + strings.Repeat("3", 64), ApiImageDigest: testApiImageDigest(),
		EvidenceSha256: strings.Repeat("4", 64), ApiCompatible: true, WorkerCompatible: true}
	runtimeDigest := competitionRecoveryTestDigest(tb, fixture.runtime)
	if err := BindCompetitionJobReevaluationRuntime(base.settings, fixture.runtime, runtimeDigest); err != nil {
		tb.Fatal(err)
	}
	snapshot := readCompetitionReevaluationPgSnapshot(ctx, job)
	terminalDigest, err := CompetitionJobReevaluationSnapshotDigest(snapshot.job)
	if err != nil {
		tb.Fatal(err)
	}
	policyDigest, err := CompetitionJobReevaluationSnapshotDigest(base.round.PolicyJson)
	if err != nil {
		tb.Fatal(err)
	}
	var eventId int64
	var eventDigest string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT event_id,payload_sha256 FROM competition_job_event WHERE job_id=$1 AND event_type='failed' ORDER BY event_id DESC LIMIT 1`, job.JobId).Scan(&eventId, &eventDigest))
	})
	auth := model.CompetitionJobReevaluationAuthorization{Schema: 2, Kind: model.CompetitionDistributedJobReevaluationAuthorizationKind,
		IncidentClass: model.CompetitionJobReevaluationDistributedInfrastructure, Environment: fixture.principal.Environment, CompetitionId: fixture.principal.CompetitionId, SeasonId: fixture.principal.SeasonId,
		JobId: job.JobId, RoundId: job.RoundId, ExpectedCycle: 0, ExpectedAttemptCount: 1, ExpectedTerminalSnapshot: snapshot.job, ExpectedTerminalSha256: terminalDigest,
		EvidenceSha256: strings.Repeat("5", 64), IncidentEventId: eventId, IncidentEventSha256: eventDigest, IncidentAttempt: 1, RoundPolicySha256: policyDigest,
		BaselineSha256: base.prepared.BaselineSha256, RuntimeQualificationSha256: runtimeDigest, FixedSourceCommit: fixture.runtime.SourceCommit, FixedWorkerSha256: fixture.runtime.WorkerSha256,
		FixedWorkerImageDigest: fixture.runtime.WorkerImageDigest, ApiImageDigest: fixture.runtime.ApiImageDigest, AuthorizedAt: databaseNow.Add(-time.Second), ExpiresAt: databaseNow.Add(time.Hour), SingleUse: true,
		Distributed: &model.CompetitionDistributedJobReevaluationAuthorization{Schema: 1, Reason: model.CompetitionDistributedReevaluationRetentionBudget,
			OriginalHostId: job.ExecutorHostId, OriginalSlotId: job.SlotId, OriginalFencingToken: job.SlotFencingToken, NativeFencingToken: job.SlotFencingToken,
			OriginalRunId: competitionagent.RunId(job.JobId.String(), 0, 1), OriginalStartedAt: *job.StartedAt, OriginalDeadlineAt: *job.ExecutionDeadline,
			OriginalManifestSha256: strings.Repeat("c", 64), OriginalRequestSha256: strings.Repeat("d", 64),
			SourceLockSha256: base.settings.AutomationPolicy.SourceLockSha256, EvaluatorImageDigest: base.settings.EvaluatorImageDigest, PolicyApprovalSha256: base.settings.DistributedEvaluationPolicy.ApprovalSha256,
			ClosureEvidenceSha256: strings.Repeat("6", 64), SupervisorOwnerId: base.settings.automationLease.OwnerId, SupervisorFencingToken: base.settings.automationLease.FencingToken}}
	fixture.request = model.CompetitionJobReevaluationRequest{IdempotencyKey: "synthetic-distributed-recovery", Authorization: auth}
	fixture.refreshIdleCut(tb)
	return fixture
}

// A second grant must explicitly authorize its newly observed complete fence
// cut. This does not mutate a previously sealed request or committed receipt.
func (self *competitionDistributedRecoveryPgFixture) refreshIdleCut(t testing.TB) {
	t.Helper()
	self.request.Authorization.Distributed.IdleSlots = nil
	server.Db(t.Context(), func(conn server.PgConn) {
		rows, err := conn.Query(t.Context(), `SELECT host_id,slot_id,fencing_token FROM competition_execution_slot ORDER BY slot_id`)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var slot model.CompetitionDistributedReevaluationIdleSlot
				server.Raise(rows.Scan(&slot.HostId, &slot.SlotId, &slot.FencingToken))
				self.request.Authorization.Distributed.IdleSlots = append(self.request.Authorization.Distributed.IdleSlots, slot)
			}
		})
	})
	self.request.AuthorizationSha256 = competitionRecoveryTestDigest(t, self.request.Authorization)
}

// Production uses the same store implementation and signed validators, only
// replacing fixed-command SSH with synthetic keys and deterministic barriers.
func (self *competitionDistributedRecoveryPgFixture) authorize(t testing.TB, ctx context.Context) (*model.CompetitionJobReevaluationReceipt, error) {
	t.Helper()
	return self.store.authorizeDistributedJobReevaluation(ctx, self.settings, self.principal, self.request, func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
		return competitionDistributedRecoveryTestTransport{t: t, host: host, auth: self.request.Authorization, round: self.round, calls: &self.calls, before: self.beforeNative, mutate: self.mutateNative}, nil
	})
}

// Releasing only the fixture's exact hold leaves every claim/source/host gate
// active. No helper manufactures a recovery start or resets its original clock.
func (self *competitionDistributedRecoveryPgFixture) releaseHold(t testing.TB) {
	t.Helper()
	args := self.hold.Request
	args.Active, args.ExpectedRevision, args.IdempotencyKey = false, self.hold.Revision, "synthetic-distributed-recovery-release-"+fmt.Sprint(self.hold.Revision)
	args.ReasonCode = "RECOVERY_APPROVED"
	var err error
	self.hold, err = self.store.SetAutomationHold(t.Context(), self.settings, self.principal.Id, args)
	if err != nil {
		t.Fatal(err)
	}
}

// Both the immutable terminal and all empty fenced slots are compared, so a
// rejected grant cannot hide partial queue/fence/event/audit mutation.
type competitionDistributedRecoveryPgSnapshot struct {
	job   competitionReevaluationPgSnapshot
	slots []byte
}

// Query lifetime ends before any candidate operation begins.
func (self *competitionDistributedRecoveryPgFixture) snapshot(ctx context.Context) competitionDistributedRecoveryPgSnapshot {
	result := competitionDistributedRecoveryPgSnapshot{job: readCompetitionReevaluationPgSnapshot(ctx, self.original)}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(s) ORDER BY slot_id),'[]'::jsonb) FROM competition_execution_slot s`).Scan(&result.slots))
	})
	return result
}

// The real original failure and all paid admission/source evidence survive;
// replay neither observes native hosts again nor advances a fence or clock.
func TestCompetitionDistributedRecoveryPgSingleUsePreservesOriginalHistory(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		before := fixture.snapshot(tb.Context())
		receipt, err := fixture.authorize(tb, tb.Context())
		if err != nil || receipt == nil || receipt.Schema != 2 || receipt.Cycle != 1 || receipt.StartedAt != nil || fixture.calls != 3 {
			tb.Fatal("explicit distributed grant failed", err, fixture.calls)
		}
		after := fixture.snapshot(tb.Context())
		requireCompetitionReevaluationOriginalEvidence(tb, before.job, after.job)
		var retainedTerminal []byte
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT original_terminal_json FROM competition_job_reevaluation WHERE job_id=$1`, fixture.original.JobId).Scan(&retainedTerminal))
		})
		if !bytes.Equal(retainedTerminal, before.job.job) {
			tb.Fatal("original terminal snapshot was not retained exactly")
		}
		queued, err := fixture.store.GetJob(tb.Context(), fixture.settings, fixture.original.JobId, fixture.principal)
		if err != nil || queued.ExecutionDeadline != nil || queued.ExecutorHostId != "" || queued.CycleStartedAt != nil || queued.StartedAt == nil || !queued.StartedAt.Equal(*fixture.original.StartedAt) {
			tb.Fatal("authorization allocated a clock or changed original start", err)
		}
		replayed, err := fixture.authorize(tb, tb.Context())
		if err != nil || !reflect.DeepEqual(replayed, receipt) || fixture.calls != 3 || !reflect.DeepEqual(fixture.snapshot(tb.Context()), after) {
			tb.Fatal("exact replay changed custody/history or issued new native observation", err)
		}
		fixture.request.IdempotencyKey = "synthetic-different-grant"
		if again, err := fixture.authorize(tb, tb.Context()); err == nil || again != nil {
			tb.Fatal("second identity granted another cycle")
		}
	})
}

// An old absolute start is history, not the new cycle's time allocator. The
// first claim uses a fresh database cut and retains the normal global attempt.
func TestCompetitionDistributedRecoveryPgClaimUsesFreshDatabaseClock(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		if _, err := fixture.authorize(tb, tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.releaseHold(tb)
		var before, after time.Time
		server.Db(tb.Context(), func(conn server.PgConn) {
			var err error
			before, err = competitionIntakeFirstDatabaseTime(tb.Context(), conn)
			server.Raise(err)
		})
		// An ahead-of-database application clock must not allocate this cycle.
		fixture.clock.Advance(time.Minute)
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-recovered-worker", fixture.runtime.WorkerImageDigest)
		server.Db(tb.Context(), func(conn server.PgConn) {
			var err error
			after, err = competitionIntakeFirstDatabaseTime(tb.Context(), conn)
			server.Raise(err)
		})
		if err != nil || job == nil || job.JobId != fixture.original.JobId || job.EvaluationCycle != 1 || job.AttemptCount != 2 || job.CycleAttemptOffset != 1 ||
			job.CycleStartedAt == nil || job.CycleStartedAt.Before(before) || after.Before(*job.CycleStartedAt) || job.ExecutionDeadline == nil ||
			!job.ExecutionDeadline.Equal(job.CycleStartedAt.Add(3*time.Hour)) || !job.StartedAt.Equal(*fixture.original.StartedAt) || job.SlotFencingToken <= fixture.original.SlotFencingToken ||
			competitionagent.RunId(job.JobId.String(), job.EvaluationCycle, job.AttemptCount) == fixture.request.Authorization.Distributed.OriginalRunId {
			tb.Fatal("recovered claim reused or invented an execution identity/clock", err)
		}
	})
}

// Missing current holds and native closure must fail before mutation, not
// become a generic singleton-absence exception for distributed work.
func TestCompetitionDistributedRecoveryPgRequiresHeldIdleNativeClosure(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		fixture.releaseHold(tb)
		before := fixture.snapshot(tb.Context())
		if receipt, err := fixture.authorize(tb, tb.Context()); receipt != nil || !errors.Is(err, ErrAutomationHeld) || fixture.calls != 0 || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("unheld grant observed native hosts or mutated state", err)
		}
	})
}

// A current native busy/unknown state never allows recovery merely because
// the original database row is terminal and its old lease has expired.
func TestCompetitionDistributedRecoveryPgBusyHostPreservesWholeGrant(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		fixture.mutateNative = func(response *competitionagent.Response) {
			if response.Identity == nil {
				response.State = "occupied"
			}
		}
		before := fixture.snapshot(tb.Context())
		if receipt, err := fixture.authorize(tb, tb.Context()); receipt != nil || err == nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("occupied native host partially granted a recovery", err)
		}
	})
}

// Cancellation at an actual native boundary retains old failure and every
// slot fence. It cannot commit after the caller withdrew the approval owner.
func TestCompetitionDistributedRecoveryPgCanceledObservationIsAtomic(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		ctx, cancel := context.WithCancel(tb.Context())
		defer cancel()
		fixture.beforeNative = func(competitionagent.Request) { cancel() }
		before := fixture.snapshot(tb.Context())
		if receipt, err := fixture.authorize(tb, ctx); receipt != nil || err == nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("canceled native observation changed original/fences", err)
		}
	})
}

// The second database cut must reject a changed otherwise-empty host fence,
// rather than silently refreshing the operator's already sealed authority.
func TestCompetitionDistributedRecoveryPgChangedIdleFenceRejectsGrant(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		changed := false
		fixture.beforeNative = func(request competitionagent.Request) {
			if changed {
				return
			}
			changed = true
			server.Tx(tb.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(tb.Context(), `UPDATE competition_execution_slot SET fencing_token=fencing_token+1 WHERE slot_id=2`))
			})
		}
		before := readCompetitionReevaluationPgSnapshot(tb.Context(), fixture.original)
		if receipt, err := fixture.authorize(tb, tb.Context()); receipt != nil || err == nil || !reflect.DeepEqual(before, readCompetitionReevaluationPgSnapshot(tb.Context(), fixture.original)) {
			tb.Fatal("changed native-observation fence cut still granted a cycle", err)
		}
	})
}

// Existing stale cycle/host authority remains refused before and after the
// fresh recovered claim, including detached persistence callbacks.
func TestCompetitionDistributedRecoveryPgRejectsOriginalResultAndHeartbeat(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		if _, err := fixture.authorize(tb, tb.Context()); err != nil {
			tb.Fatal(err)
		}
		assertStale := func() {
			before := fixture.snapshot(tb.Context())
			stale := withCompetitionJobAuthority(tb.Context(), fixture.original)
			selected := fixture.selected(0)
			if err := fixture.store.Heartbeat(stale, selected, "synthetic-original-successor", fixture.original.JobId); !errors.Is(err, ErrLeaseLost) {
				tb.Fatal("original heartbeat was not fenced", err)
			}
			if _, err := fixture.store.Complete(stale, selected, "synthetic-original-successor", fixture.original.JobId, EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic late original result")}); !errors.Is(err, ErrLeaseLost) {
				tb.Fatal("original result was not fenced", err)
			}
			if !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
				tb.Fatal("old callback changed recovered history")
			}
		}
		assertStale()
		fixture.releaseHold(tb)
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-new-cycle-worker", fixture.runtime.WorkerImageDigest)
		if err != nil || job == nil {
			tb.Fatal(err)
		}
		assertStale()
	})
}

// The shared native/SQL closure rule also covers the separately diagnosed
// legacy no-execution failure, without forging an evaluator success or output.
func TestCompetitionDistributedRecoveryPgClosedPrelaunchGetsOnlyOneFreshCycle(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		fixture.request.Authorization.Distributed.Reason = model.CompetitionDistributedReevaluationLegacyPrelaunch
		fixture.request.AuthorizationSha256 = competitionRecoveryTestDigest(tb, fixture.request.Authorization)
		receipt, err := fixture.authorize(tb, tb.Context())
		if err != nil || receipt == nil || receipt.Distributed.Reason != model.CompetitionDistributedReevaluationLegacyPrelaunch || receipt.PreviousFailure.Code != "evaluation_time_budget_exhausted" {
			tb.Fatal("closed prelaunch recovery relabeled old failure or lacked authority", err)
		}
		var proof competitionDistributedRecoveryProof
		if err := json.Unmarshal(receipt.DistributedProof, &proof); err != nil || proof.OriginalStatus.State != "failed" || proof.OriginalStatus.ErrorCode != "legacy_prelaunch_infrastructure_failure" {
			tb.Fatal("prelaunch proof was replaced with a manufactured success", err)
		}
	})
}

// No conflicting runtime can claim the recovery, and preserved admission
// timestamps/ids still place it ahead of a later already queued paid job.
func TestCompetitionDistributedRecoveryPgRetainsFifoAndQualifiedWorker(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		if _, err := fixture.authorize(tb, tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.releaseHold(tb)
		before := fixture.snapshot(tb.Context())
		if job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-obsolete-worker", testWorkerImageDigest()); job != nil || err == nil || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("obsolete worker consumed recovery authority", err)
		}
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-fixed-worker", fixture.runtime.WorkerImageDigest)
		if err != nil || job == nil || job.JobId != fixture.original.JobId || !job.SubmittedAt.Equal(fixture.original.SubmittedAt) {
			tb.Fatal("recovery changed preserved admission FIFO", err)
		}
	})
}

// An unrelated live job prevents even observation of native hosts. An idle
// legacy singleton row alone is not a distributed closure certificate.
func TestCompetitionDistributedRecoveryPgActiveJobBlocksNativeObservation(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		fixture.releaseHold(tb)
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(1), "synthetic-other-active-worker", testWorkerImageDigest())
		if err != nil || job == nil || job.JobId == fixture.original.JobId {
			tb.Fatal("failed to create independent guarded active owner", err)
		}
		args := fixture.hold.Request
		args.Active, args.ExpectedRevision, args.IdempotencyKey = true, fixture.hold.Revision, "synthetic-rehold"
		args.ReasonCode = "OPERATOR_HOLD"
		fixture.hold, err = fixture.store.SetAutomationHold(tb.Context(), fixture.settings, fixture.principal.Id, args)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.refreshIdleCut(tb)
		before := fixture.snapshot(tb.Context())
		if receipt, err := fixture.authorize(tb, tb.Context()); receipt != nil || err == nil || fixture.calls != 0 || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("active owner was bypassed or probed by terminal recovery", err)
		}
	})
}

// A slot cut pinned before one grant cannot authorize the next one after all
// host fences have advanced, even if every physical host remains idle.
func TestCompetitionDistributedRecoveryPgSealedCensusCannotBeSilentlyRefreshed(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		before := fixture.snapshot(tb.Context())
		oldRequest := competitionRecoveryTestCopy(tb, fixture.request)
		// Reproduce another explicit maintenance action advancing the empty
		// census; the already sealed request must fail before any transport.
		server.Tx(tb.Context(), func(tx server.PgTx) {
			for _, slot := range oldRequest.Authorization.Distributed.IdleSlots {
				server.RaisePgResult(tx.Exec(tb.Context(), `UPDATE competition_execution_slot SET fencing_token=fencing_token+1 WHERE slot_id=$1`, slot.SlotId))
			}
		})
		if receipt, err := fixture.authorize(tb, tb.Context()); receipt != nil || err == nil || fixture.calls != 0 {
			tb.Fatal("sealed idle census was implicitly refreshed", err)
		}
		if after := readCompetitionReevaluationPgSnapshot(tb.Context(), fixture.original); !reflect.DeepEqual(before.job, after) {
			tb.Fatal("refused stale census changed terminal/audit")
		}
		fixture.refreshIdleCut(tb)
		if receipt, err := fixture.authorize(tb, tb.Context()); receipt == nil || err != nil {
			tb.Fatal("separately resealed fresh idle census failed", err)
		}
	})
}

// The new DDL is applied while an actual cycle-zero distributed owner is
// running. Its old SQL lifecycle stays authorized before and after deployment.
func TestCompetitionDistributedRecoveryPgMigrationKeepsRunningCycleZeroWorker(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		version := server.CompetitionDistributedReevaluationMigrationVersion()
		server.ApplyDbMigrationsUpTo(tb.Context(), version-1)
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		job, err := fixture.store.Claim(tb.Context(), settings, "synthetic-running-legacy-control", testWorkerImageDigest())
		if err != nil || job == nil || job.EvaluationCycle != 0 {
			tb.Fatal("old-prefix distributed owner did not claim", err)
		}
		server.ApplyDbMigrationsUpTo(tb.Context(), version)
		fixture.clock.Advance(time.Second)
		ctx := withCompetitionJobAuthority(tb.Context(), job)
		if err := fixture.store.Heartbeat(ctx, settings, "synthetic-running-legacy-control", job.JobId); err != nil {
			tb.Fatal("additive recovery DDL fenced running cycle-zero heartbeat", err)
		}
		outcome := testSharedBaselineOutcome(tb, settings, job, EvaluationOutcome{Score: competitionRankingScore(90, 110), distributedArchiveVerified: true})
		if retry, err := fixture.store.Complete(ctx, settings, "synthetic-running-legacy-control", job.JobId, outcome); err != nil || retry {
			tb.Fatal("additive recovery DDL changed running cycle-zero completion", retry, err)
		}
	})
}

// Owner polling and the general job-event journal see only digest provenance;
// complete signed native inventories remain solely in the private audit row.
func TestCompetitionDistributedRecoveryPgOwnerAndEventOmitPrivateNativeProof(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		receipt, err := fixture.authorize(tb, tb.Context())
		if err != nil || receipt == nil || len(receipt.DistributedProof) == 0 {
			tb.Fatal(err)
		}
		service := newServiceWithImageDigest(fixture.settings, fixture.store, testApiImageDigest(), nil)
		result, status, failure := service.getScoreWithDecisionContract(tb.Context(), fixture.original.JobId, &Principal{Id: "synthetic-submitter", Role: "submitter"}, true)
		if status != 200 || failure != nil || result == nil || result.Reevaluation == nil || result.Reevaluation.Distributed.NativeProofSha256 != receipt.Distributed.NativeProofSha256 {
			tb.Fatal("owner polling lost the authorized cycle projection", status, failure)
		}
		ownerJson, err := json.Marshal(result)
		if err != nil {
			tb.Fatal(err)
		}
		var eventJson, auditJson []byte
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT payload_json FROM competition_job_event WHERE job_id=$1 AND event_type='reevaluation_authorized'`, fixture.original.JobId).Scan(&eventJson))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT receipt_json FROM competition_job_reevaluation WHERE job_id=$1`, fixture.original.JobId).Scan(&auditJson))
		})
		for _, content := range [][]byte{ownerJson, eventJson} {
			for _, hidden := range []string{"distributed_proof", "synthetic-original/worker-result.json", "signature"} {
				if bytes.Contains(content, []byte(hidden)) {
					tb.Fatal("private native proof escaped into polling/general events")
				}
			}
		}
		if !bytes.Contains(auditJson, []byte("distributed_proof")) || !bytes.Contains(auditJson, []byte("synthetic-original/worker-result.json")) {
			tb.Fatal("redaction destroyed the immutable private native audit")
		}
	})
}

// Exactly the two individually authorized jobs receive distinct single-use
// cycles. The second cannot reuse the first grant's already advanced census.
func TestCompetitionDistributedRecoveryPgTwoGrantsRequireFreshCensusAndKeepOrder(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		ctx := tb.Context()
		firstOriginal := fixture.original
		fixture.releaseHold(tb)
		fixture.clock.Advance(firstOriginal.StartedAt.Sub(fixture.clock.Now()))
		other, err := fixture.store.Claim(ctx, fixture.selected(1), "synthetic-second-original-worker", testWorkerImageDigest())
		if err != nil || other == nil || other.JobId == firstOriginal.JobId {
			tb.Fatal("second genuine original did not claim", err)
		}
		fixture.clock.Advance(3*time.Hour + time.Second)
		other, err = fixture.store.Claim(ctx, fixture.selected(1), "synthetic-second-original-successor", testWorkerImageDigest())
		if err != nil || other == nil || other.AttemptCount != 1 {
			tb.Fatal("second original did not retain its historical attempt", err)
		}
		if _, err := fixture.store.Complete(withCompetitionJobAuthority(ctx, other), fixture.selected(1), "synthetic-second-original-successor", other.JobId, EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic second original budget exhausted")}); err != nil {
			tb.Fatal(err)
		}
		var now time.Time
		server.Db(ctx, func(conn server.PgConn) {
			var err error
			now, err = competitionIntakeFirstDatabaseTime(ctx, conn)
			server.Raise(err)
		})
		fixture.clock.Advance(now.Sub(fixture.clock.Now()))
		args := fixture.hold.Request
		args.Active, args.ExpectedRevision, args.IdempotencyKey = true, fixture.hold.Revision, "synthetic-two-originals-rehold"
		args.ReasonCode = "OPERATOR_HOLD"
		fixture.hold, err = fixture.store.SetAutomationHold(ctx, fixture.settings, fixture.principal.Id, args)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.refreshIdleCut(tb)
		firstRequest := competitionRecoveryTestCopy(tb, fixture.request)
		secondRequest := competitionRecoveryTestCopy(tb, fixture.request)
		secondRequest.IdempotencyKey = "synthetic-second-explicit-grant"
		a := &secondRequest.Authorization
		a.JobId = other.JobId
		snapshot := readCompetitionReevaluationPgSnapshot(ctx, other)
		a.ExpectedTerminalSnapshot = snapshot.job
		a.ExpectedTerminalSha256, err = CompetitionJobReevaluationSnapshotDigest(snapshot.job)
		if err != nil {
			tb.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT event_id,payload_sha256 FROM competition_job_event WHERE job_id=$1 AND event_type='failed' ORDER BY event_id DESC LIMIT 1`, other.JobId).Scan(&a.IncidentEventId, &a.IncidentEventSha256))
		})
		d := a.Distributed
		d.Reason = model.CompetitionDistributedReevaluationLegacyPrelaunch
		d.OriginalHostId, d.OriginalSlotId, d.OriginalFencingToken, d.NativeFencingToken = other.ExecutorHostId, other.SlotId, other.SlotFencingToken, other.SlotFencingToken
		d.OriginalRunId, d.OriginalStartedAt, d.OriginalDeadlineAt = competitionagent.RunId(other.JobId.String(), 0, 1), *other.StartedAt, *other.ExecutionDeadline
		secondRequest.AuthorizationSha256 = competitionRecoveryTestDigest(tb, a)
		firstReceipt, err := fixture.authorize(tb, ctx)
		if err != nil || firstReceipt == nil {
			tb.Fatal(err)
		}
		callsBefore := fixture.calls
		fixture.original, fixture.request = other, secondRequest
		if receipt, err := fixture.authorize(tb, ctx); receipt != nil || err == nil || fixture.calls != callsBefore {
			tb.Fatal("second grant silently refreshed a stale idle fence cut", err)
		}
		fixture.refreshIdleCut(tb)
		secondReceipt, err := fixture.authorize(tb, ctx)
		if err != nil || secondReceipt == nil || secondReceipt.JobId == firstReceipt.JobId || secondReceipt.StartedAt != nil {
			tb.Fatal("separately sealed second cycle was not uniquely authorized", err)
		}
		fixture.request, fixture.original = firstRequest, firstOriginal
		if replay, err := fixture.authorize(tb, ctx); err != nil || replay.ReceiptSha256 != firstReceipt.ReceiptSha256 {
			tb.Fatal("later host fences invalidated exact first receipt replay", err)
		}
		fixture.releaseHold(tb)
		first, err := fixture.store.Claim(ctx, fixture.selected(0), "synthetic-first-recovered", fixture.runtime.WorkerImageDigest)
		if err != nil || first == nil || first.JobId != firstOriginal.JobId {
			tb.Fatal("recovered FIFO lost original first admission", err)
		}
		second, err := fixture.store.Claim(ctx, fixture.selected(1), "synthetic-second-recovered", fixture.runtime.WorkerImageDigest)
		if err != nil || second == nil || second.JobId != other.JobId || first.EvaluationCycle != 1 || second.EvaluationCycle != 1 || first.AttemptCount != 2 || second.AttemptCount != 2 {
			tb.Fatal("second recovered claim changed original ordering or attempt namespaces", err)
		}
	})
}

// Reconciliation after custody handback stays on the same new execution, not
// a second recovery clock or an additional native attempt.
func TestCompetitionDistributedRecoveryPgReclaimKeepsCycleClockAndRunId(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		if _, err := fixture.authorize(tb, tb.Context()); err != nil {
			tb.Fatal(err)
		}
		fixture.releaseHold(tb)
		settings := fixture.selected(0)
		first, err := fixture.store.Claim(tb.Context(), settings, "synthetic-recovery-first-owner", fixture.runtime.WorkerImageDigest)
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		if err := fixture.store.HandBack(withCompetitionJobAuthority(tb.Context(), first), "synthetic-recovery-first-owner", first.JobId, "synthetic-owned-shutdown"); err != nil {
			tb.Fatal(err)
		}
		next, err := fixture.store.Claim(tb.Context(), settings, "synthetic-recovery-successor", testWorkerImageDigest())
		if err != nil || next == nil || next.JobId != first.JobId || next.EvaluationCycle != 1 || next.AttemptCount != first.AttemptCount || next.CycleAttemptOffset != first.CycleAttemptOffset ||
			!next.CycleStartedAt.Equal(*first.CycleStartedAt) || !next.ExecutionDeadline.Equal(*first.ExecutionDeadline) || next.SlotFencingToken <= first.SlotFencingToken || next.WorkerImageDigest != first.WorkerImageDigest ||
			competitionagent.RunId(next.JobId.String(), next.EvaluationCycle, next.AttemptCount) != competitionagent.RunId(first.JobId.String(), first.EvaluationCycle, first.AttemptCount) {
			tb.Fatal("recovery handback/reclaim changed native attempt or original new-cycle budget", err)
		}
	})
}

// Cancellation after a successful grant prevents claim while its immutable
// receipt remains replayable; no new cycle clock appears during cancellation.
func TestCompetitionDistributedRecoveryPgCanceledRoundCannotStartCycle(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		receipt, err := fixture.authorize(tb, tb.Context())
		if err != nil {
			tb.Fatal(err)
		}
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), `UPDATE competition_round SET canceled=true WHERE round_id=$1`, fixture.round.RoundId))
		})
		before := fixture.snapshot(tb.Context())
		if replay, err := fixture.authorize(tb, tb.Context()); err != nil || replay.ReceiptSha256 != receipt.ReceiptSha256 || !reflect.DeepEqual(before, fixture.snapshot(tb.Context())) {
			tb.Fatal("canceled round erased or mutated committed receipt replay", err)
		}
		fixture.releaseHold(tb)
		if job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-canceled-cycle-owner", fixture.runtime.WorkerImageDigest); job != nil || err != nil {
			tb.Fatal("canceled round started its recovered cycle", err)
		}
		var started *time.Time
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT cycle_started_at FROM competition_job WHERE job_id=$1`, fixture.original.JobId).Scan(&started))
		})
		if started != nil {
			tb.Fatal("round cancellation invented a recovered start")
		}
	})
}

// The local operator begins from a clockless season role, not a selected
// running worker's already-populated round/host fields. Derive those from the
// authenticated source/round without changing the caller's settings.
func TestCompetitionDistributedRecoveryPgDerivesUnselectedOperatorRound(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		copy := *fixture.settings
		copy.automationRoundId = server.Id{}
		copy.executorHostId, copy.executorSlotId, copy.distributedRoundEpoch = "", 0, 0
		copy.DistributedEvaluationPolicy = nil
		fixture.settings = &copy
		if receipt, err := fixture.authorize(tb, tb.Context()); err != nil || receipt == nil {
			tb.Fatal("unselected operator role could not derive exact frozen source/round", err)
		}
		if copy.automationRoundId != (server.Id{}) || copy.DistributedEvaluationPolicy != nil || copy.executorHostId != "" {
			tb.Fatal("grant mutated the caller's unselected deployment role")
		}
	})
}
