// Disposable PostgreSQL keeps published holds, job/slot/fence and completion
// guards enabled. Only the old signed status is synthetic; no native host runs.
package controller

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// The scope starts after ordinary fixture setup. Any client SELECT of private
// job bodies fails before query execution; normal completion writes are not
// confused with fetching a previous payload.
type competitionExpiredBridgeMetadataTrace struct {
	locked    atomic.Int64
	forbidden atomic.Int64
}

// No SQL text or arguments escape the test; only typed counters are retained.
func (self *competitionExpiredBridgeMetadataTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.ToLower(strings.Join(strings.Fields(data.SQL), " "))
	if !strings.HasPrefix(query, "select ") {
		return ctx
	}
	for _, column := range []string{"patch_bytes", "score_json", "seed_nonce", "seed_ciphertext", "to_jsonb(j)"} {
		if strings.Contains(query, column) {
			self.forbidden.Add(1)
			panic(errors.New("expired bridge attempted a forbidden body SELECT"))
		}
	}
	if strings.HasSuffix(query, " for update of j") {
		self.locked.Add(1)
	}
	return ctx
}

// All ownership is in the caller's original synchronous driver operation.
func (*competitionExpiredBridgeMetadataTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// The exact original claim is expired without rewriting any persisted clock;
// its genuine source parent's renewed live lease and claim-only hold precede
// the metadata observation. Historical job time never expires that SQL lease.
func competitionExpiredBridgePgFixture(t *testing.T, tb testing.TB) (*competitionDistributedPgFixture, *Settings, *queuedJob, *queuedJob, competitionExpiredControlBridgeRecord, *competitionAgentEvaluator) {
	tb.Helper()
	var databaseNow time.Time
	server.Db(tb.Context(), func(conn server.PgConn) {
		var err error
		databaseNow, err = competitionIntakeFirstDatabaseTime(tb.Context(), conn)
		server.Raise(err)
	})
	// Genuine announced admission can predate the SQL clock without rewriting an
	// ASAP grant or updating a job deadline merely to make this test pass.
	fixture := newCompetitionPreparedAnnouncedCompletionFixtureAt(t, tb, databaseNow.Add(-3*time.Hour-time.Second))
	first, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-old-bridge-worker", testWorkerImageDigest())
	if err != nil || first == nil || first.ExecutionDeadline == nil || first.LeaseExpiresAt == nil {
		tb.Fatal("original genuine claim failed", err)
	}
	next := &queuedJob{}
	server.Db(tb.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(tb.Context(), `SELECT job_id FROM competition_job WHERE round_id=$1 AND state='queued' AND job_id<>$2 ORDER BY submitted_at,job_id LIMIT 1`, first.RoundId, first.JobId).Scan(&next.JobId))
		var err error
		databaseNow, err = competitionIntakeFirstDatabaseTime(tb.Context(), conn)
		server.Raise(err)
	})
	fixture.clock.Advance(databaseNow.Sub(fixture.clock.Now()))
	originalParent := fixture.settings.automationLease
	if originalParent == nil || !databaseNow.Before(originalParent.LeaseExpiresAt) ||
		first.ExecutionDeadline.After(databaseNow) || first.LeaseExpiresAt.After(databaseNow) {
		tb.Fatal("fixture did not separate its expired original job from the current live parent")
	}
	if replacement, err := fixture.store.AcquireAutomationLease(tb.Context(), fixture.settings, "synthetic-refused-live-bridge-parent", 5*time.Minute); !errors.Is(err, ErrLeaseLost) || replacement != nil {
		tb.Fatal("historical job time authorized stealing a still-live parent", err)
	}
	server.Db(tb.Context(), func(conn server.PgConn) {
		var unchanged bool
		server.Raise(conn.QueryRow(tb.Context(), `SELECT owner_id=$4 AND fencing_token=$5 AND lease_expires_at=$6
 FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3`,
			originalParent.Environment, originalParent.CompetitionId, originalParent.SeasonId, originalParent.OwnerId, originalParent.FencingToken, originalParent.LeaseExpiresAt).Scan(&unchanged))
		if !unchanged {
			tb.Fatal("refused fixture takeover changed the genuine current parent")
		}
	})
	lease, err := fixture.store.RenewAutomationLease(tb.Context(), fixture.settings, *originalParent, 5*time.Minute)
	if err != nil || lease == nil || lease.OwnerId != originalParent.OwnerId || lease.FencingToken != originalParent.FencingToken ||
		!databaseNow.Before(lease.LeaseExpiresAt) {
		tb.Fatal("genuine current parent could not renew without changing its fence", err)
	}
	fixture.settings.automationLease = lease
	settings := fixture.selected(0)
	hold, err := fixture.store.SetAutomationHold(tb.Context(), settings, "synthetic-bridge-operator", model.CompetitionAutomationHoldArgs{
		Kind: "evaluation", Active: true, IdempotencyKey: "synthetic-expired-bridge-hold", ReasonCode: "OPERATOR_HOLD",
		PublicExplanation: "Synthetic expired-run bridge regression.", EvidenceSha256: strings.Repeat("b", 64)})
	if err != nil || hold == nil {
		tb.Fatal("exact claim-only hold failed", err)
	}
	host := settings.DistributedEvaluationPolicy.Hosts[0]
	connection := competitionExecutorConnection{HostId: host.HostId, AgentCommandSha256: strings.Repeat("6", 64), AgentConfigSha256: strings.Repeat("7", 64),
		ExpiredControlBridge: &competitionExpiredControlBridgeConnection{RecordPath: filepath.Join(tb.TempDir(), "synthetic-expired-pg-bridge.json")}}
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{
		Settings: defaultCompetitionDistributedCoordinatorSettings(), Hosts: []competitionExecutorConnection{connection}}}
	record := competitionExpiredControlBridgeRecord{Schema: 1, Kind: competitionExpiredControlBridgeKind,
		Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		ApprovalSha256: strings.Repeat("8", 64), Identity: competitionagent.Identity{HostId: first.ExecutorHostId,
			RunId: competitionagent.RunId(first.JobId.String(), first.EvaluationCycle, first.AttemptCount), JobId: first.JobId.String(), RoundId: first.RoundId.String(),
			RoundEpoch: first.Round.Epoch, SourceEpoch: evaluationSourceEpoch(&first.Round), Cycle: first.EvaluationCycle, Attempt: first.AttemptCount,
			StartedAt: *first.StartedAt, DeadlineAt: *first.ExecutionDeadline}, SlotId: first.SlotId, SlotFencingToken: first.SlotFencingToken,
		LeaseOwner: first.LeaseOwner, LeaseExpiresAt: *first.LeaseExpiresAt, WorkerImageDigest: first.WorkerImageDigest, EvaluationHoldRevision: hold.Revision,
		AgentSha256: connection.AgentCommandSha256, AgentConfigSha256: connection.AgentConfigSha256, QualificationSha256: competitionExecutorQualificationSha256(settings),
		SourceLockSha256: settings.AutomationPolicy.SourceLockSha256, RoundBaselineSha256: first.RoundBaseline.Sha256,
		EvaluatorImageDigest: competitionExecutionImageForJob(settings, first), ManifestSha256: strings.Repeat("9", 64), RequestSha256: strings.Repeat("a", 64)}
	writeCompetitionExpiredBridgeRecord(tb, settings, record)
	evaluator := &competitionAgentEvaluator{host: host, now: fixture.clock.Now, transport: &competitionExpiredBridgeTransport{record: record, keySeed: 1}}
	return fixture, settings, first, next, record, evaluator
}

// Both snapshot and ordinary-status paths are real; no stored seed/request or
// candidate bytes are needed to mint the exact old-role completion capability.
func competitionExpiredBridgePgWitness(tb testing.TB, fixture *competitionDistributedPgFixture, settings *Settings, record competitionExpiredControlBridgeRecord, evaluator *competitionAgentEvaluator) *competitionExpiredControlWitness {
	tb.Helper()
	snapshot, err := fixture.store.expiredCompetitionJob(tb.Context(), settings)
	if err != nil || snapshot == nil || len(snapshot.Patch) != 0 || snapshot.Score != nil || len(snapshot.Round.SeedNonce) != 0 || len(snapshot.Round.SeedCiphertext) != 0 {
		tb.Fatal("metadata-only occupied snapshot failed", err)
	}
	connection, _ := competitionExpiredControlConnection(settings)
	witness, err := evaluator.observeExpiredControl(tb.Context(), settings, snapshot, record, connection.ExpiredControlBridge.RecordSha256)
	if err != nil {
		tb.Fatal("exact synthetic old-primary signature failed", err)
	}
	return witness
}

// The historical refusal remains; only the root-approved occupied expired row
// is reclaimed and completed, with all source/clock/image/FIFO metadata intact.
func TestExpiredControlBridgePgCompletesOnlyExpiredSlotUnderHold(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, settings, first, next, record, evaluator := competitionExpiredBridgePgFixture(t, tb)
		if value, err := fixture.store.Claim(tb.Context(), settings, "synthetic-refused-ordinary-bridge", testWorkerImageDigest()); value != nil || !errors.Is(err, ErrAutomationHeld) {
			tb.Fatal("old held path did not refuse", err)
		}
		trace := &competitionExpiredBridgeMetadataTrace{}
		scope, err := server.NewTestPgQueryScope(tb.Context(), trace)
		if err != nil {
			tb.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				tb.Error(err)
			}
		}()
		witness := competitionExpiredBridgePgWitness(tb, fixture, settings, record, evaluator)
		selected := *settings
		selected.executorReconcileOnly, selected.executorReconcileExpiredOnly, selected.executorExpiredControlWitness = true, true, witness
		claimed, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-expired-bridge-successor", testWorkerImageDigest())
		if closeErr := scope.Close(); closeErr != nil {
			tb.Fatal(closeErr)
		}
		if err != nil || claimed == nil || claimed.State != "failed" || claimed.LeaseOwner != "" || claimed.LeaseExpiresAt != nil || claimed.JobId != first.JobId || claimed.AttemptCount != first.AttemptCount ||
			claimed.SlotFencingToken != first.SlotFencingToken+1 || !claimed.StartedAt.Equal(*first.StartedAt) || !claimed.ExecutionDeadline.Equal(*first.ExecutionDeadline) || claimed.WorkerImageDigest != first.WorkerImageDigest ||
			len(claimed.Patch) != 0 || claimed.Score != nil || len(claimed.Round.SeedNonce) != 0 || len(claimed.Round.SeedCiphertext) != 0 || trace.locked.Load() != 1 || trace.forbidden.Load() != 0 {
			tb.Fatal("real atomic expired claim changed execution or retained a lease", err)
		}
		connection, _ := competitionExpiredControlConnection(settings)
		if completed, err := fixture.store.expiredCompetitionJobCompleted(tb.Context(), settings, record, connection.ExpiredControlBridge.RecordSha256); err != nil || !completed {
			tb.Fatal("atomic expired completion lacks its exact durable receipt", completed, err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var state string
			var attempts int
			var started, completed *time.Time
			server.Raise(conn.QueryRow(tb.Context(), `SELECT state,attempt_count,started_at,completed_at FROM competition_job WHERE job_id=$1`, next.JobId).Scan(&state, &attempts, &started, &completed))
			if state != "queued" || attempts != 0 || started != nil || completed != nil {
				tb.Fatal("expired-only bridge consumed queued work")
			}
			var slotJob *server.Id
			server.Raise(conn.QueryRow(tb.Context(), `SELECT job_id FROM competition_execution_slot WHERE slot_id=$1`, first.SlotId).Scan(&slotJob))
			if slotJob != nil {
				tb.Fatal("normal expired completion retained stale slot")
			}
		})
	})
}

// Only the caller's acknowledgement is dropped; PostgreSQL executes the real
// claim and normal completion in their one original transaction.
type competitionExpiredBridgePgAckStore struct {
	PostgresStore
	claims      int
	completions int
}

// The first successful terminal commit is deliberately not acknowledged.
func (self *competitionExpiredBridgePgAckStore) Claim(ctx context.Context, settings *Settings, workerId, image string) (*queuedJob, error) {
	self.claims++
	job, err := self.PostgresStore.Claim(ctx, settings, workerId, image)
	if err == nil && job != nil {
		return nil, errors.New("synthetic successful commit acknowledgement lost")
	}
	return job, err
}

// A separate completion would recreate the stranded intermediate-lease bug.
func (self *competitionExpiredBridgePgAckStore) Complete(context.Context, *Settings, string, server.Id, EvaluationOutcome) (bool, error) {
	self.completions++
	return false, errors.New("synthetic second completion tripwire")
}

// An actual committed transaction survives an uncertain reply, worker restart,
// supervisor takeover and expiry of its transaction-local metadata lease.
func TestExpiredControlBridgePgLostAckAndRestartReadOnlyTerminal(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, settings, first, next, record, evaluator := competitionExpiredBridgePgFixture(t, tb)
		store := &competitionExpiredBridgePgAckStore{PostgresStore: fixture.store}
		worker := &Worker{settings: settings, store: store, workerId: "synthetic-uncertain-bridge-worker", workerImageDigest: testWorkerImageDigest()}
		if occupied, err := worker.reconcileExpiredControl(tb.Context(), evaluator); !errors.Is(err, ErrAutomationHeld) || !occupied || store.claims != 1 || store.completions != 0 {
			tb.Fatal("lost commit acknowledgement was not resolved terminal-only", occupied, err)
		}
		fixture.clock.Advance(6 * time.Minute)
		lease, err := fixture.store.AcquireAutomationLease(tb.Context(), settings, "synthetic-restarted-bridge-parent", 5*time.Minute)
		if err != nil || lease == nil {
			tb.Fatal("fresh live supervisor takeover failed", err)
		}
		settings.automationLease = lease
		worker.workerId = "synthetic-restarted-bridge-worker"
		if occupied, err := worker.reconcileExpiredControl(tb.Context(), evaluator); !errors.Is(err, ErrAutomationHeld) || !occupied || store.claims != 1 || store.completions != 0 {
			tb.Fatal("restart reissued claim/completion or stranded committed terminal", occupied, err)
		}
		transport := evaluator.transport.(*competitionExpiredBridgeTransport)
		if transport.calls != 1 || transport.forbidden != 0 {
			tb.Fatal("terminal readback performed another native operation")
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var state string
			var fence int64
			var attempts, reconciliations, failures int
			var started, deadline time.Time
			server.Raise(conn.QueryRow(tb.Context(), `SELECT state,executor_fencing_token,attempt_count,started_at,execution_deadline_at FROM competition_job WHERE job_id=$1`, first.JobId).
				Scan(&state, &fence, &attempts, &started, &deadline))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FILTER(WHERE event_type='execution_reconciled' AND payload_json->'expired_control_bridge'->>'record_sha256'=$2),
 count(*) FILTER(WHERE event_type='failed') FROM competition_job_event WHERE job_id=$1`, first.JobId, settings.distributedCoordinator.Config.Hosts[0].ExpiredControlBridge.RecordSha256).
				Scan(&reconciliations, &failures))
			if state != "failed" || fence != record.SlotFencingToken+1 || attempts != first.AttemptCount || !started.Equal(record.Identity.StartedAt) ||
				!deadline.Equal(record.Identity.DeadlineAt) || reconciliations != 1 || failures != 1 {
				tb.Fatal("uncertain acknowledgement changed the original attempt or published twice")
			}
			var queued bool
			server.Raise(conn.QueryRow(tb.Context(), `SELECT state='queued' AND attempt_count=0 AND started_at IS NULL AND completed_at IS NULL FROM competition_job WHERE job_id=$1`, next.JobId).Scan(&queued))
			if !queued {
				tb.Fatal("terminal restart consumed the queued job")
			}
		})
	})
}

// Failure at the ordinary completion's actual pre-publication barrier must
// roll back the reconcile event, new fence and metadata lease together.
func TestExpiredControlBridgePgCompletionRefusalRollsBackWholeClaim(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, settings, first, _, record, evaluator := competitionExpiredBridgePgFixture(t, tb)
		witness := competitionExpiredBridgePgWitness(tb, fixture, settings, record, evaluator)
		selected := *settings
		selected.executorReconcileOnly, selected.executorReconcileExpiredOnly, selected.executorExpiredControlWitness = true, true, witness
		barrierCalls := 0
		fixture.store.afterCompletionBaselineForTest = func() {
			barrierCalls++
			panic(ErrLeaseLost)
		}
		if value, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-rolled-back-bridge-worker", testWorkerImageDigest()); value != nil || !errors.Is(err, ErrLeaseLost) || barrierCalls == 0 {
			tb.Fatal("completion failure did not reject its original claim transaction", err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var original bool
			server.Raise(conn.QueryRow(tb.Context(), `SELECT j.state='running' AND j.lease_owner=$2 AND j.lease_expires_at=$3 AND j.executor_fencing_token=$4
 AND s.job_id=j.job_id AND s.worker_id=j.lease_owner AND s.lease_expires_at=j.lease_expires_at AND s.fencing_token=j.executor_fencing_token
 FROM competition_job j JOIN competition_execution_slot s ON s.slot_id=j.executor_slot_id WHERE j.job_id=$1`,
				first.JobId, record.LeaseOwner, record.LeaseExpiresAt, record.SlotFencingToken).Scan(&original))
			var count int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type IN('execution_reconciled','failed')`, first.JobId).Scan(&count))
			if !original || count != 0 {
				tb.Fatal("failed completion left a committed intermediate fence, lease or event")
			}
		})
	})
}

// One serial disposable transaction owns this fault. Closing its actual socket
// before commit sends no commit bytes; no second database connection is owned.
type competitionExpiredBridgeCommitDisconnect struct {
	armed        atomic.Bool
	disconnects  atomic.Int64
	commitErrors atomic.Int64
	closeError   error
}

// The real completion barrier arms precisely this transaction's first commit.
func (self *competitionExpiredBridgeCommitDisconnect) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") && self.armed.CompareAndSwap(true, false) {
		self.disconnects.Add(1)
		self.closeError = conn.PgConn().Conn().Close()
	}
	return ctx
}

// A driver commit error is required in addition to closing the physical socket.
func (self *competitionExpiredBridgeCommitDisconnect) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if self.disconnects.Load() == 1 && data.Err != nil {
		self.commitErrors.Add(1)
	}
}

// This is a physical pre-commit disconnect, not the post-commit reply-dropping
// wrapper above. Both real states are safe without guessing whether it committed.
func TestExpiredControlBridgePgPhysicalDisconnectBeforeCommitRollsBack(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, settings, first, _, record, evaluator := competitionExpiredBridgePgFixture(t, tb)
		witness := competitionExpiredBridgePgWitness(tb, fixture, settings, record, evaluator)
		selected := *settings
		selected.executorReconcileOnly, selected.executorReconcileExpiredOnly, selected.executorExpiredControlWitness = true, true, witness
		fault := &competitionExpiredBridgeCommitDisconnect{}
		scope, err := server.NewTestPgQueryScope(tb.Context(), fault)
		if err != nil {
			tb.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				tb.Error(err)
			}
		}()
		barrierCalls := 0
		fixture.store.afterCompletionBaselineForTest = func() {
			barrierCalls++
			fault.armed.Store(true)
		}
		value, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-disconnected-bridge-worker", testWorkerImageDigest())
		if err == nil || value != nil || barrierCalls != 1 || fault.disconnects.Load() != 1 || fault.closeError != nil || fault.commitErrors.Load() == 0 {
			tb.Fatal("actual pre-commit disconnect did not fail its sole terminal transaction", err)
		}
		if err := scope.Close(); err != nil {
			tb.Fatal(err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var original bool
			server.Raise(conn.QueryRow(tb.Context(), `SELECT j.state='running' AND j.lease_owner=$2 AND j.lease_expires_at=$3 AND j.executor_fencing_token=$4
 AND s.job_id=j.job_id AND s.worker_id=j.lease_owner AND s.lease_expires_at=j.lease_expires_at AND s.fencing_token=j.executor_fencing_token
 FROM competition_job j JOIN competition_execution_slot s ON s.slot_id=j.executor_slot_id WHERE j.job_id=$1`,
				first.JobId, record.LeaseOwner, record.LeaseExpiresAt, record.SlotFencingToken).Scan(&original))
			var count int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type IN('execution_reconciled','failed')`, first.JobId).Scan(&count))
			if !original || count != 0 {
				tb.Fatal("physical disconnect left an intermediate committed lease, fence or event")
			}
		})
	})
}

// Complete historically retained its captured terminal timeout across real
// transaction reruns. A backward owner clock cannot resurrect an earlier
// retriable outcome merely because the callback body moved into a helper.
func TestExpiredControlBridgePgCompleteRetryRetainsCapturedTimeout(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		fixture, job := newCompetitionLeasePgFixture(tb, ctx, "captured-completion-timeout")
		if job.StartedAt == nil || job.AttemptCount >= fixture.settings.MaxInfrastructureAttempts {
			tb.Fatal("normal singleton fixture omitted its original retry budget")
		}
		deadline := job.StartedAt.Add(time.Duration(fixture.settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
		target, step := deadline.Add(-time.Second), time.Duration(fixture.settings.WorkerLeaseSeconds)*time.Second-time.Second
		if step <= 0 {
			tb.Fatal("normal singleton heartbeat bound is invalid")
		}
		// Preserve the admitted durations and original start. Only ordinary
		// acknowledged heartbeats keep both leases live near the fixed deadline.
		for fixture.clock.Before(target) {
			next := fixture.clock.Add(step)
			if target.Before(next) {
				next = target
			}
			fixture.clock = next
			if err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-lease-worker", job.JobId); err != nil {
				tb.Fatal("ordinary heartbeat lost original singleton authority", err)
			}
		}
		rewound := target.Add(-time.Minute)
		_, exhausted := infrastructureRetrySchedule(fixture.settings, *job.StartedAt, target, job.AttemptCount)
		_, recovered := infrastructureRetrySchedule(fixture.settings, *job.StartedAt, rewound, job.AttemptCount)
		if exhausted || !recovered || rewound.Before(*job.StartedAt) {
			tb.Fatal("controlled clock values did not distinguish exhausted and available retry budgets")
		}
		prepareCompetitionSupervisorReadCommitFault(ctx, "40001")
		fault := &competitionSupervisorReadCommitFault{}
		scope, err := server.NewTestPgQueryScope(ctx, fault)
		if err != nil {
			tb.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				tb.Error(err)
			}
		}()
		reruns, callbacks := 0, 0
		fixture.store.afterCompletionBaselineForTest = func() { callbacks++ }
		ctx = server.Testing_WithTxRerunHook(ctx, func() {
			reruns++
			fixture.clock = rewound
		})
		outcome := EvaluationOutcome{Error: infrastructureError("synthetic_retriable_completion", "Synthetic retained infrastructure failure.")}
		if retry, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-lease-worker", job.JobId, outcome); err != nil || retry ||
			reruns != 1 || callbacks != 2 || fault.commitFailures.Load() != 1 {
			tb.Fatal("commit retry resurrected its pre-timeout infrastructure outcome", retry, err, reruns, callbacks)
		}
		if err := scope.Close(); err != nil {
			tb.Fatal(err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var state, code string
			var attempts, failures, retries int
			var started time.Time
			var completed *time.Time
			var scoreAbsent, retriable bool
			server.Raise(conn.QueryRow(tb.Context(), `SELECT state,COALESCE(eval_error_json->>'code',''),
 COALESCE((eval_error_json->>'retriable')::boolean,false),score_json IS NULL,attempt_count,started_at,completed_at
 FROM competition_job WHERE job_id=$1`, job.JobId).Scan(&state, &code, &retriable, &scoreAbsent, &attempts, &started, &completed))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FILTER(WHERE event_type='failed'),count(*) FILTER(WHERE event_type='infrastructure_retry')
 FROM competition_job_event WHERE job_id=$1`, job.JobId).Scan(&failures, &retries))
			if state != "failed" || code != "evaluation_time_budget_exhausted" || retriable || !scoreAbsent || attempts != job.AttemptCount ||
				!started.Equal(*job.StartedAt) || completed == nil || !completed.Equal(rewound) || failures != 1 || retries != 0 {
				tb.Fatal("normal completion lost its captured timeout, original attempt or single terminal publication")
			}
		})
	})
}

// A new hold revision can commit while the old RPC is outside the transaction;
// the genuine late held check must refuse even though the hold remains active.
func TestExpiredControlBridgePgRechecksHoldAfterObservation(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, settings, first, _, record, evaluator := competitionExpiredBridgePgFixture(t, tb)
		witness := competitionExpiredBridgePgWitness(tb, fixture, settings, record, evaluator)
		if _, err := fixture.store.SetAutomationHold(tb.Context(), settings, "synthetic-bridge-operator", model.CompetitionAutomationHoldArgs{
			Kind: "evaluation", Active: true, ExpectedRevision: record.EvaluationHoldRevision, IdempotencyKey: "synthetic-expired-bridge-new-hold",
			ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic later hold head.", EvidenceSha256: strings.Repeat("c", 64)}); err != nil {
			tb.Fatal(err)
		}
		selected := *settings
		selected.executorReconcileOnly, selected.executorReconcileExpiredOnly, selected.executorExpiredControlWitness = true, true, witness
		if claimed, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-refused-later-hold", testWorkerImageDigest()); claimed != nil || !errors.Is(err, ErrLeaseLost) {
			tb.Fatal("a later active hold head reused the expired bridge", err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var fence int64
			server.Raise(conn.QueryRow(tb.Context(), `SELECT executor_fencing_token FROM competition_job WHERE job_id=$1`, first.JobId).Scan(&fence))
			if fence != first.SlotFencingToken {
				tb.Fatal("rejected held claim changed custody")
			}
		})
	})
}

// Two independently fresh observations of one old row cannot both spend its
// fence; neither a second witness nor replay of the first may steal the result.
func TestExpiredControlBridgePgStaleFenceAndReplayRefuse(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, settings, first, _, record, evaluator := competitionExpiredBridgePgFixture(t, tb)
		older := competitionExpiredBridgePgWitness(tb, fixture, settings, record, evaluator)
		newer := competitionExpiredBridgePgWitness(tb, fixture, settings, record, evaluator)
		selected := *settings
		selected.executorReconcileOnly, selected.executorReconcileExpiredOnly, selected.executorExpiredControlWitness = true, true, newer
		claimed, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-winning-bridge-fence", testWorkerImageDigest())
		if err != nil || claimed == nil {
			tb.Fatal(err)
		}
		for _, witness := range []*competitionExpiredControlWitness{older, newer} {
			selected.executorExpiredControlWitness = witness
			if value, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-refused-bridge-replay", testWorkerImageDigest()); value != nil || !errors.Is(err, ErrLeaseLost) {
				tb.Fatal("stale or consumed bridge stole current custody", err)
			}
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var fence int64
			var attempts int
			var deadline time.Time
			server.Raise(conn.QueryRow(tb.Context(), `SELECT executor_fencing_token,attempt_count,execution_deadline_at FROM competition_job WHERE job_id=$1`, first.JobId).Scan(&fence, &attempts, &deadline))
			if fence != claimed.SlotFencingToken || attempts != first.AttemptCount || !deadline.Equal(*first.ExecutionDeadline) {
				tb.Fatal("rejected bridge changed the original attempt/clock")
			}
		})
	})
}
