// Exact synthetic old-role status drives the real bridge into one expired
// completion. Every other evaluator, transfer or fresh-work entry is a tripwire.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// Only the ordinary row scanner is synthetic here; the same metadata column
// order and complete public cycle receipt validator run as the SQL boundary.
type competitionExpiredBridgeMetadataRow []any

// Type checks catch projection/scanner drift without coercing identity fields.
func (self competitionExpiredBridgeMetadataRow) Scan(destinations ...any) error {
	if len(self) != len(destinations) {
		return errors.New("synthetic metadata projection changed its column count")
	}
	for index, destination := range destinations {
		target := reflect.ValueOf(destination).Elem()
		if self[index] == nil {
			target.SetZero()
			continue
		}
		value := reflect.ValueOf(self[index])
		if !value.Type().AssignableTo(target.Type()) {
			return errors.New("synthetic metadata projection changed a typed column")
		}
		target.Set(value)
	}
	return nil
}

// Zero/first/second cycles retain their real bounded receipt while no branch
// selects a payload or binds cycle zero to a later optional receipt column.
func TestExpiredControlBridgeMetadataProjectionPreservesClosedCycles(t *testing.T) {
	settings := validSettings()
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, cycleNumber := range []int{0, 1, 2} {
		query, err := competitionExpiredControlJobSelect(cycleNumber)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"patch_bytes", "score_json", "eval_error_json", "seed_nonce", "seed_ciphertext", "to_jsonb(j)"} {
			if strings.Contains(strings.ToLower(query), forbidden) {
				t.Fatal("expired metadata projection selects hidden body data")
			}
		}
		if strings.Contains(query, "second_recovery_receipt_json") != (cycleNumber == 2) ||
			strings.Contains(query, "competition_job_reevaluation") != (cycleNumber == 1) {
			t.Fatal("protected cycle selected another schema's receipt column")
		}
		trace := &competitionExpiredBridgeMetadataTrace{}
		// Normal Complete uses alias "job"; its real metadata lock must not
		// masquerade as the distinct expired selector's alias "j" lock.
		trace.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "SELECT state FROM competition_job AS job FOR UPDATE OF job"})
		if trace.locked.Load() != 0 {
			t.Fatal("metadata trace confused the normal completion alias with the expired selector")
		}
		trace.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: query + " FOR UPDATE OF j"})
		if trace.locked.Load() != 1 || trace.forbidden.Load() != 0 {
			t.Fatal("metadata trace missed its exact expired locked selector")
		}
		var cycle model.CompetitionJobReevaluationCycle
		var receipt []byte
		jobId, roundId, patch := server.NewId(), server.NewId(), strings.Repeat("1", 64)
		started := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
		var cycleStarted *time.Time
		attempt, offset := 1, 0
		if cycleNumber > 0 {
			cycle = competitionDistributedRecoveryCycleForTest(t)
			if cycleNumber == 2 {
				cycle = competitionSecondRecoveryCycleForTest(t)
			}
			jobId, roundId, patch = cycle.JobId, cycle.RoundId, cycle.PatchSha256
			started = cycle.AuthorizedAt.Add(time.Second)
			cycleStarted, attempt, offset = &started, 3, 2
			receipt, err = json.Marshal(model.CompetitionJobReevaluationReceipt{CompetitionJobReevaluationCycle: cycle, AttemptOffset: offset})
			if err != nil {
				t.Fatal(err)
			}
		}
		deadline, lease := started.Add(3*time.Hour), started.Add(time.Minute)
		row := competitionExpiredBridgeMetadataRow{
			jobId, roundId, patch, "running", started, &started, nil, "synthetic-metadata-cache",
			nil, nil, nil, attempt, "synthetic-metadata-owner", &lease, testApiImageDigest(), testWorkerImageDigest(),
			cycleNumber, offset, cycleStarted, receipt, "synthetic-executor", 1, int64(1), &deadline,
			settings.CompetitionId, strings.Repeat("2", 64), nil, nil, strings.Repeat("3", 64), "synthetic-providers.yml",
			[]byte(policy), started.Add(-time.Hour), deadline, deadline, started.Add(-time.Hour), false, 2, false, nil, nil, nil, nil, nil,
		}
		job, err := scanCompetitionExpiredControlJob(row, cycleNumber, deadline.Add(time.Second))
		if err != nil || job == nil || job.JobId != jobId || job.RoundBaseline != nil || len(job.Patch) != 0 || job.Score != nil || len(job.Round.SeedNonce) != 0 || len(job.Round.SeedCiphertext) != 0 {
			t.Fatal("metadata scanner lost the exact body-free job identity", cycleNumber, err)
		}
		if cycleNumber > 0 && (job.Reevaluation == nil || job.Reevaluation.Cycle != cycleNumber || job.Reevaluation.FixedWorkerImageDigest != cycle.FixedWorkerImageDigest ||
			job.Reevaluation.StartedAt == nil || !job.Reevaluation.StartedAt.Equal(started)) {
			t.Fatal("metadata scanner dropped the recovered cycle's original clock or fixed runtime")
		}
		if _, err := scanCompetitionExpiredControlJob(row, (cycleNumber+1)%3, deadline.Add(time.Second)); err == nil {
			t.Fatal("metadata scanner accepted another protected-record cycle")
		}
		for _, index := range []int{8, 9, 10, 26, 27} {
			changed := append(competitionExpiredBridgeMetadataRow(nil), row...)
			changed[index] = []byte("synthetic unexpected hidden bytes")
			if index == 8 || index == 9 {
				changed[index] = []byte("{}")
			}
			if _, err := scanCompetitionExpiredControlJob(changed, cycleNumber, deadline.Add(time.Second)); err == nil {
				t.Fatal("unexpected body was cleared rather than refused")
			}
		}
	}
	for _, cycle := range []int{-1, 3} {
		if query, err := competitionExpiredControlJobSelect(cycle); !errors.Is(err, ErrConflict) || query != "" {
			t.Fatal("unknown cycle selected arbitrary receipt authority")
		}
	}
}

// The caller owns one deterministic synthetic clock and no native endpoint.
type competitionExpiredBridgeTransport struct {
	record    competitionExpiredControlBridgeRecord
	calls     int
	forbidden int
	mutate    func(*competitionagent.Response)
	after     func()
	keySeed   byte
}

// Only one status envelope, with no body directories, can reach the old role.
func (self *competitionExpiredBridgeTransport) Exchange(_ context.Context, request competitionagent.Request, input, output string) (competitionagent.Response, error) {
	if request.Operation != "status" || request.Start != nil || input != "" || output != "" || request.RunId != self.record.Identity.RunId || request.FencingToken != self.record.SlotFencingToken {
		self.forbidden++
		return competitionagent.Response{}, errors.New("synthetic forbidden operation")
	}
	self.calls++
	identity := self.record.Identity
	response := competitionagent.Response{Schema: competitionagent.Schema, HostId: identity.HostId, RunId: identity.RunId, Identity: &identity,
		FencingToken: request.FencingToken, State: "succeeded", ManifestSha256: self.record.ManifestSha256, RequestSha256: self.record.RequestSha256}
	if self.mutate != nil {
		self.mutate(&response)
	}
	seed := self.keySeed
	if seed == 0 {
		seed = 2
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
	if err := competitionagent.SignResponse(&response, key); err != nil {
		return response, err
	}
	if self.after != nil {
		self.after()
	}
	return response, nil
}

// A separately approved observer is deliberately not a bridge fallback.
func (self *competitionExpiredBridgeTransport) ExchangeExisting(context.Context, competitionagent.Request, string) (competitionagent.Response, error) {
	self.forbidden++
	return competitionagent.Response{}, errors.New("synthetic mixed observer tripwire")
}

// Locked-row mutations are explicit barriers, never sleeps or queue polling.
type competitionExpiredBridgeStore struct {
	Store
	job                 *queuedJob
	record              competitionExpiredControlBridgeRecord
	now                 time.Time
	held                bool
	revision            int
	queued              int
	claims              int
	completions         int
	beforeClaim         func(*Settings)
	outcome             EvaluationOutcome
	terminal            *queuedJob
	eventContent        []byte
	eventSha256         string
	eventAt             time.Time
	eventActor          string
	terminalContent     []byte
	terminalSha256      string
	claimAckError       error
	claimRollbackError  error
	afterCommit         func()
	liveSupervisorOwner string
	liveSupervisorFence int64
	slotRetained        bool
}

// A root-selected occupied row is the only snapshot; FIFO is not represented.
func (self *competitionExpiredBridgeStore) expiredCompetitionJob(_ context.Context, settings *Settings) (*queuedJob, error) {
	if !competitionExpiredControlHoldMatches(self.record, settings, self.held, self.revision) {
		return nil, ErrLeaseLost
	}
	if self.job == nil {
		return nil, nil
	}
	copy := *self.job
	return &copy, nil
}

// Exercise the same opaque capability, held revision and late row comparisons
// as the real claim callback; PostgreSQL cases separately prove its SQL locks.
func (self *competitionExpiredBridgeStore) Claim(_ context.Context, settings *Settings, workerId, _ string) (*queuedJob, error) {
	self.claims++
	if self.beforeClaim != nil {
		self.beforeClaim(settings)
	}
	witness := settings.executorExpiredControlWitness
	if witness == nil || !settings.executorReconcileOnly || !settings.executorReconcileExpiredOnly || settings.executorFreshOnly ||
		!witness.consume(settings) || !competitionExpiredControlHoldMatches(witness.record, settings, self.held, self.revision) {
		return nil, ErrAutomationHeld
	}
	if self.job == nil || !witness.slotMatches(self.job.ExecutorHostId, &self.job.JobId, &self.job.LeaseOwner, self.job.LeaseExpiresAt, self.job.SlotFencingToken) {
		return nil, ErrLeaseLost
	}
	// The real job SELECT returns metadata without its independently loaded
	// shared baseline. Full authority cannot be checked until that read joins.
	copy := *self.job
	copy.RoundBaseline = nil
	copy.RoundBaseline = self.job.RoundBaseline
	if !witness.matches(settings, &copy, self.now) {
		return nil, ErrLeaseLost
	}
	if self.claimRollbackError != nil {
		return nil, self.claimRollbackError
	}
	copy.SlotFencingToken++
	lease := self.now.Add(time.Minute)
	payload := witness.eventPayload(copy.SlotFencingToken, workerId, lease)
	if payload == nil {
		return nil, ErrLeaseLost
	}
	self.eventContent, _ = json.Marshal(map[string]any{"attempt": copy.AttemptCount, "worker_image_digest": copy.WorkerImageDigest,
		"executor_host_id": copy.ExecutorHostId, "executor_slot_id": copy.SlotId, "executor_fencing_token": copy.SlotFencingToken,
		"execution_deadline_at": *copy.ExecutionDeadline, "expired_control_bridge": payload})
	digest := sha256.Sum256(self.eventContent)
	self.eventSha256, self.eventAt, self.eventActor = hex.EncodeToString(digest[:]), self.now, workerId
	self.outcome = EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "submission exhausted its total evaluation time budget")}
	self.terminalContent, _ = json.Marshal(map[string]any{"attempt": copy.AttemptCount, "api_image_digest": copy.ApiImageDigest, "worker_image_digest": copy.WorkerImageDigest})
	digest = sha256.Sum256(self.terminalContent)
	self.terminalSha256 = hex.EncodeToString(digest[:])
	completed := self.now
	copy.State, copy.LeaseOwner, copy.LeaseExpiresAt, copy.CompletedAt = "failed", "", nil, &completed
	self.completions++
	self.job, self.terminal = nil, &copy
	if self.afterCommit != nil {
		self.afterCommit()
	}
	if self.claimAckError != nil {
		return nil, self.claimAckError
	}
	return &copy, nil
}

// The real bridge finishes in its claim transaction, never a second call.
func (self *competitionExpiredBridgeStore) Complete(context.Context, *Settings, string, server.Id, EvaluationOutcome) (bool, error) {
	return false, errors.New("synthetic second completion tripwire")
}

// The actual proof parser authenticates durable metadata; a new current live
// parent may acknowledge it, but an intermediate running row never passes.
func (self *competitionExpiredBridgeStore) expiredCompetitionJobCompleted(ctx context.Context, settings *Settings, record competitionExpiredControlBridgeRecord, recordSha256 string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	connection, present := competitionExpiredControlConnection(settings)
	if !present || connection.ExistingControl != nil || connection.ExpiredControlBridge.RecordSha256 != recordSha256 || settings.automationLease == nil ||
		settings.automationLease.OwnerId != self.liveSupervisorOwner || settings.automationLease.FencingToken != self.liveSupervisorFence ||
		!competitionExpiredControlHoldMatches(record, settings, self.held, self.revision) {
		return false, ErrLeaseLost
	}
	if self.terminal == nil && len(self.eventContent) == 0 && (self.job == nil || competitionExpiredControlRecordMatches(settings, self.job, record, self.now)) {
		return false, nil
	}
	proof, err := validateCompetitionExpiredControlClaimEvent(settings, record, recordSha256, self.eventAt, self.eventActor, self.eventContent, self.eventSha256)
	if err != nil {
		return false, err
	}
	job := self.terminal
	if job == nil || self.job != nil || self.slotRetained || !competitionExpiredControlImmutableMatches(settings, job, record) || job.State != "failed" ||
		job.SlotFencingToken != proof.NewFence || job.CompletedAt == nil || job.CompletedAt.Before(self.eventAt) || job.CompletedAt.Before(record.Identity.DeadlineAt) ||
		job.LeaseOwner != "" || job.LeaseExpiresAt != nil || self.now.Before(record.Identity.DeadlineAt) ||
		self.outcome.Score != nil || self.outcome.Error == nil || self.outcome.Error.Code != "evaluation_time_budget_exhausted" || self.outcome.Error.Retriable || len(self.outcome.ArtifactManifest) != 0 {
		return false, ErrLeaseLost
	}
	if err := validateCompetitionExpiredControlTerminalEvent(job, self.terminalContent, self.terminalSha256); err != nil {
		return false, err
	}
	return true, nil
}

// Full-package QA supplies protected root-owned TMPDIR; no production path or
// identity is a fixture. The existing factory supplies frozen synthetic policy.
func newCompetitionExpiredBridgeFixture(t *testing.T) (*Worker, *competitionAgentEvaluator, *competitionExpiredBridgeStore, *competitionExpiredBridgeTransport, competitionExpiredControlBridgeRecord, string) {
	t.Helper()
	worker, evaluator, old, _, _ := newCompetitionExistingWorkerFixture(t, true)
	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	started, deadline, expiredLease := now.Add(-3*time.Hour-time.Minute), now.Add(-time.Minute), now.Add(-2*time.Minute)
	job := *old.job
	job.StartedAt, job.ExecutionDeadline, job.LeaseExpiresAt = &started, &deadline, &expiredLease
	connection := worker.settings.distributedCoordinator.Config.Hosts[0]
	connection.ExistingControl = nil
	connection.AgentCommandSha256, connection.AgentConfigSha256 = strings.Repeat("6", 64), strings.Repeat("7", 64)
	record := competitionExpiredControlBridgeRecord{Schema: 1, Kind: competitionExpiredControlBridgeKind,
		Environment: worker.settings.AutomationPolicy.Environment, CompetitionId: worker.settings.CompetitionId, SeasonId: worker.settings.AutomationPolicy.SeasonId,
		ApprovalSha256: strings.Repeat("8", 64), Identity: competitionagent.Identity{HostId: job.ExecutorHostId,
			RunId: competitionagent.RunId(job.JobId.String(), job.EvaluationCycle, job.AttemptCount), JobId: job.JobId.String(), RoundId: job.RoundId.String(),
			RoundEpoch: job.Round.Epoch, SourceEpoch: evaluationSourceEpoch(&job.Round), Cycle: job.EvaluationCycle, Attempt: job.AttemptCount, StartedAt: started, DeadlineAt: deadline},
		SlotId: job.SlotId, SlotFencingToken: job.SlotFencingToken, LeaseOwner: job.LeaseOwner, LeaseExpiresAt: expiredLease,
		WorkerImageDigest: job.WorkerImageDigest, EvaluationHoldRevision: 7, AgentSha256: connection.AgentCommandSha256, AgentConfigSha256: connection.AgentConfigSha256,
		QualificationSha256: competitionExecutorQualificationSha256(worker.settings), SourceLockSha256: worker.settings.AutomationPolicy.SourceLockSha256,
		RoundBaselineSha256: job.RoundBaseline.Sha256, EvaluatorImageDigest: competitionExecutionImageForJob(worker.settings, &job),
		ManifestSha256: strings.Repeat("9", 64), RequestSha256: strings.Repeat("a", 64)}
	path := filepath.Join(t.TempDir(), "synthetic-expired-bridge.json")
	connection.ExpiredControlBridge = &competitionExpiredControlBridgeConnection{RecordPath: path}
	worker.settings.distributedCoordinator.Config.Hosts[0] = connection
	writeCompetitionExpiredBridgeRecord(t, worker.settings, record)
	store := &competitionExpiredBridgeStore{job: &job, record: record, now: now, held: true, revision: 7, queued: 5}
	store.liveSupervisorOwner, store.liveSupervisorFence = worker.settings.automationLease.OwnerId, worker.settings.automationLease.FencingToken
	transport := &competitionExpiredBridgeTransport{record: record}
	worker.store, evaluator.transport = store, transport
	evaluator.now = func() time.Time { return now }
	return worker, evaluator, store, transport, record, path
}

// The test record uses the production reader's exact canonical/hash surface.
func writeCompetitionExpiredBridgeRecord(t testing.TB, settings *Settings, record competitionExpiredControlBridgeRecord) {
	t.Helper()
	connection, _ := competitionExpiredControlConnection(settings)
	content, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(connection.ExpiredControlBridge.RecordPath, content, 0400); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	connection.ExpiredControlBridge.RecordSha256 = hex.EncodeToString(digest[:])
}

// The old held claim refuses, while the exact bridge changes only stale custody
// to the normal elapsed-budget failure and never consumes the queued jobs.
func TestExpiredControlBridgeCompletesOnlyOriginalExpiredFailure(t *testing.T) {
	worker, evaluator, store, transport, record, _ := newCompetitionExpiredBridgeFixture(t)
	if _, err := store.Claim(t.Context(), worker.settings, worker.workerId, worker.workerImageDigest); !errors.Is(err, ErrAutomationHeld) {
		t.Fatal("old held path did not refuse", err)
	}
	occupied, err := worker.reconcileExpiredControl(t.Context(), evaluator)
	if err != nil || !occupied || transport.calls != 1 || transport.forbidden != 0 || store.completions != 1 || store.queued != 5 || store.job != nil {
		t.Fatal("exact expired-only composed bridge did not isolate queued work", occupied, err)
	}
	if record.Identity.StartedAt.Add(3*time.Hour) != record.Identity.DeadlineAt || store.outcome.Score != nil || store.outcome.Error.Code != "evaluation_time_budget_exhausted" {
		t.Fatal("bridge changed the immutable clock or accepted late score")
	}
}

// Released, absent and later hold heads cannot reuse an older approval.
func TestExpiredControlBridgeRequiresExactCurrentHold(t *testing.T) {
	for _, item := range []struct {
		active   bool
		revision int
	}{{active: false, revision: 7}, {active: true, revision: 6}, {active: true, revision: 8}, {active: true, revision: 0}} {
		worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
		store.held, store.revision = item.active, item.revision
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err == nil || store.completions != 0 || store.queued != 5 || transport.forbidden != 0 {
			t.Fatal("another hold head authorized expired reconciliation", item, err)
		}
	}
}

// The explicit post-RPC barrier mutates actual locked snapshot fields.
func TestExpiredControlBridgeRejectsChangedLockedAuthority(t *testing.T) {
	for _, mutate := range []func(*queuedJob){
		func(job *queuedJob) { job.SlotFencingToken++ },
		func(job *queuedJob) { job.LeaseOwner = "another-owner.example" },
		func(job *queuedJob) { value := job.LeaseExpiresAt.Add(time.Second); job.LeaseExpiresAt = &value },
		func(job *queuedJob) { job.AttemptCount++ },
		func(job *queuedJob) { job.EvaluationCycle++ },
		func(job *queuedJob) { job.RoundId = server.NewId() },
		func(job *queuedJob) { job.ExecutorHostId = "another-host.example" },
		func(job *queuedJob) { value := job.ExecutionDeadline.Add(time.Second); job.ExecutionDeadline = &value },
		func(job *queuedJob) { job.WorkerImageDigest = "sha256:" + strings.Repeat("c", 64) },
	} {
		worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
		store.beforeClaim = func(*Settings) { mutate(store.job) }
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err == nil || transport.calls != 1 || store.completions != 0 || store.queued != 5 {
			t.Fatal("changed locked authority consumed the bridge", err)
		}
	}
}

// Even validly signed unknown/active/mismatched envelopes cannot grant custody.
func TestExpiredControlBridgeRejectsNonterminalOrUnboundStatus(t *testing.T) {
	for _, mutate := range []func(*competitionagent.Response){
		func(response *competitionagent.Response) { response.State = "unknown" },
		func(response *competitionagent.Response) { response.State = "accepted" },
		func(response *competitionagent.Response) { response.State = "running" },
		func(response *competitionagent.Response) { response.RunId = strings.Repeat("0", 64) },
		func(response *competitionagent.Response) { response.FencingToken++ },
		func(response *competitionagent.Response) { response.ManifestSha256 = strings.Repeat("b", 64) },
		func(response *competitionagent.Response) { response.RequestSha256 = strings.Repeat("c", 64) },
		func(response *competitionagent.Response) { response.Identity = nil },
		func(response *competitionagent.Response) {
			response.ExistingRun = &competitionagent.ExistingRunEvidence{}
		},
	} {
		worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
		transport.mutate = mutate
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err == nil || store.claims != 0 || store.completions != 0 || store.queued != 5 {
			t.Fatal("unbound signed status authorized a claim", err)
		}
	}
	worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
	transport.keySeed = 3
	if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err == nil || store.claims != 0 {
		t.Fatal("another signing key authorized the bridge", err)
	}
}

// Both the original immutable deadline and abandoned database lease must expire.
func TestExpiredControlBridgeRequiresOriginalElapsedDeadline(t *testing.T) {
	for _, changeDeadline := range []bool{true, false} {
		worker, evaluator, store, transport, record, path := newCompetitionExpiredBridgeFixture(t)
		future := store.now.Add(time.Minute)
		if changeDeadline {
			record.Identity.DeadlineAt, store.job.ExecutionDeadline = future, &future
			started := future.Add(-3 * time.Hour)
			record.Identity.StartedAt, store.job.StartedAt = started, &started
		} else {
			record.LeaseExpiresAt, store.job.LeaseExpiresAt = future, &future
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		writeCompetitionExpiredBridgeRecord(t, worker.settings, record)
		if err := os.Chmod(path, 0400); err != nil {
			t.Fatal(err)
		}
		transport.record = record
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err == nil || transport.calls != 0 || store.claims != 0 || store.completions != 0 {
			t.Fatal("unelapsed authority reached native status", err)
		}
	}
}

// Updated pinned roles cannot inherit a witness minted by the old endpoint.
func TestExpiredControlBridgeRejectsChangedRoleAfterObservation(t *testing.T) {
	for _, mutate := range []func(*Settings){
		func(settings *Settings) {
			settings.distributedCoordinator.Config.Hosts[0].AgentCommandSha256 = strings.Repeat("b", 64)
		},
		func(settings *Settings) {
			settings.distributedCoordinator.Config.Hosts[0].AgentConfigSha256 = strings.Repeat("c", 64)
		},
		func(settings *Settings) {
			settings.distributedCoordinator.Config.Hosts[0].ExpiredControlBridge.RecordSha256 = strings.Repeat("d", 64)
		},
		func(settings *Settings) {
			settings.distributedCoordinator.Config.Hosts[0].ExpiredControlBridge.RecordPath += ".alias"
		},
		func(settings *Settings) { settings.executorSlotId++ },
	} {
		worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
		// Reconciliation passes a selected copy. Mutate the actual Claim
		// authority, not an outer scalar that this callback no longer owns.
		store.beforeClaim = func(selected *Settings) { mutate(selected) }
		if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err == nil || transport.calls != 1 || store.completions != 0 || store.queued != 5 {
			t.Fatal("changed operational role inherited old bridge authority", err)
		}
	}
}

// Atomic consumption and exact RPC-age/supervisor/pin checks reject reuse.
func TestExpiredControlBridgeWitnessIsSingleUseAndFresh(t *testing.T) {
	worker, evaluator, store, _, record, _ := newCompetitionExpiredBridgeFixture(t)
	connection, _ := competitionExpiredControlConnection(worker.settings)
	witness, err := evaluator.observeExpiredControl(t.Context(), worker.settings, store.job, record, connection.ExpiredControlBridge.RecordSha256)
	if err != nil || !witness.consume(worker.settings) || witness.consume(worker.settings) || !witness.matches(worker.settings, store.job, store.now) {
		t.Fatal("fresh witness did not have exactly one consumption", err)
	}
	metadata := *store.job
	metadata.RoundBaseline = nil
	if witness.matches(worker.settings, &metadata, store.now) {
		t.Fatal("unhydrated job metadata passed full baseline authority")
	}
	metadata.RoundBaseline = store.job.RoundBaseline
	if !witness.matches(worker.settings, &metadata, store.now) {
		t.Fatal("hydrated matching baseline lost full authority")
	}
	wrongBaseline := *metadata.RoundBaseline
	wrongBaseline.Sha256 = strings.Repeat("d", 64)
	metadata.RoundBaseline = &wrongBaseline
	if witness.matches(worker.settings, &metadata, store.now) {
		t.Fatal("hydrated wrong baseline passed full authority")
	}
	if witness.matches(worker.settings, store.job, witness.expiresAt) || witness.matches(worker.settings, store.job, witness.observedAt.Add(-time.Nanosecond)) {
		t.Fatal("stale or future witness passed")
	}
	if witness.eventPayload(record.SlotFencingToken+1, worker.workerId, store.now.Add(time.Minute)) == nil || witness.eventPayload(record.SlotFencingToken, worker.workerId, store.now.Add(time.Minute)) != nil {
		t.Fatal("event proof did not bind the exact next metadata fence")
	}
	rpcTimeout := witness.rpcTimeout
	witness.rpcTimeout = 0
	if witness.eventPayload(record.SlotFencingToken+1, worker.workerId, store.now.Add(time.Minute)) != nil {
		t.Fatal("invalid recorded observation bounds reached the durable event")
	}
	witness.rpcTimeout = rpcTimeout
	worker.settings.automationLease.FencingToken++
	if witness.matches(worker.settings, store.job, store.now) {
		t.Fatal("changed supervisor fence reused the witness")
	}
}

// The old pre-RPC age check deterministically rejects a healthy 61-second
// primary startup. Only terminal-reconciliation receipt freshness is renewed.
func TestExpiredControlBridgeSlowTerminalRpcUsesReceiptFreshness(t *testing.T) {
	worker, evaluator, store, transport, record, _ := newCompetitionExpiredBridgeFixture(t)
	started := store.now
	received := started.Add(61 * time.Second)
	now := started
	evaluator.now = func() time.Time { return now }
	transport.after = func() { now = received }
	connection, _ := competitionExpiredControlConnection(worker.settings)
	settings := worker.settings.distributedCoordinator.Config.Settings
	if received.Before(started.Add(settings.ExistingWitnessMaximumAge)) || received.Sub(started) >= settings.RequestTimeout {
		t.Fatal("counterfactual did not exceed old age within the real RPC bound")
	}
	witness, err := evaluator.observeExpiredControl(t.Context(), worker.settings, store.job, record, connection.ExpiredControlBridge.RecordSha256)
	if err != nil || !witness.rpcStartedAt.Equal(started) || !witness.rpcReceivedAt.Equal(received) || !witness.observedAt.Equal(received) ||
		!witness.expiresAt.Equal(received.Add(settings.ExistingWitnessMaximumAge)) || !witness.consume(worker.settings) || !witness.matches(worker.settings, store.job, received) ||
		witness.matches(worker.settings, store.job, witness.expiresAt) || !record.Identity.DeadlineAt.Equal(*store.job.ExecutionDeadline) || store.queued != 5 {
		t.Fatal("healthy terminal RPC lost its short receipt-only reconciliation window", err)
	}
}

// A backward clock, response beyond the existing RPC cap, or canceled parent
// cannot produce an observational age window from an unbounded old operation.
func TestExpiredControlBridgeRpcDurationBoundsRemainClosed(t *testing.T) {
	for _, delay := range []time.Duration{-time.Second, 2*time.Minute + time.Nanosecond} {
		worker, evaluator, store, transport, record, _ := newCompetitionExpiredBridgeFixture(t)
		now := store.now
		evaluator.now = func() time.Time { return now }
		transport.after = func() { now = now.Add(delay) }
		connection, _ := competitionExpiredControlConnection(worker.settings)
		if _, err := evaluator.observeExpiredControl(t.Context(), worker.settings, store.job, record, connection.ExpiredControlBridge.RecordSha256); err == nil || store.claims != 0 || store.queued != 5 {
			t.Fatal("out-of-bound old RPC produced a bridge witness", delay, err)
		}
	}
}

// A changed protected record after the external RPC cannot reach claim.
func TestExpiredControlBridgeRecordMutationAfterObservationRefuses(t *testing.T) {
	worker, evaluator, store, transport, _, path := newCompetitionExpiredBridgeFixture(t)
	transport.after = func() {
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0400); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err == nil || transport.calls != 1 || store.claims != 0 || store.completions != 0 {
		t.Fatal("changed root record survived external observation", err)
	}
}

// A configured completed bridge remains closed; no implicit FIFO fallback exists.
func TestExpiredControlBridgeEmptySlotNeverCallsFreshPath(t *testing.T) {
	worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
	store.job = nil
	occupied, err := worker.reconcileExpiredControl(t.Context(), evaluator)
	if !occupied || !errors.Is(err, ErrAutomationHeld) || transport.calls != 0 || store.claims != 0 || store.completions != 0 || store.queued != 5 {
		t.Fatal("empty occupied-slot bridge fell through to fresh work", occupied, err)
	}
}

// No corrected mixed observer or canceled parent can gain this legacy authority.
func TestExpiredControlBridgeRejectsMixedRoleAndParentCancellation(t *testing.T) {
	worker, evaluator, store, transport, _, _ := newCompetitionExpiredBridgeFixture(t)
	worker.settings.distributedCoordinator.Config.Hosts[0].ExistingControl = &competitionExistingControlConnection{}
	if _, err := worker.reconcileExpiredControl(t.Context(), evaluator); err == nil || transport.calls != 0 || store.claims != 0 {
		t.Fatal("mixed observer role authorized a legacy bridge", err)
	}
	worker, evaluator, store, transport, _, _ = newCompetitionExpiredBridgeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	transport.after = cancel
	if _, err := worker.reconcileExpiredControl(ctx, evaluator); !errors.Is(err, context.Canceled) || store.claims != 0 || store.completions != 0 || transport.forbidden != 0 {
		t.Fatal("canceled parent produced claim authority", err)
	}
}

// Root ownership/0400/single-link and canonical exact SHA remain real I/O checks.
func TestExpiredControlBridgeRecordCustodyAndCanonicalBytes(t *testing.T) {
	worker, _, _, _, _, path := newCompetitionExpiredBridgeFixture(t)
	connection, _ := competitionExpiredControlConnection(worker.settings)
	limits := worker.settings.distributedCoordinator.Config.Settings.Limits
	if _, err := readCompetitionExpiredControlRecord(connection, limits); err != nil {
		t.Fatal("protected synthetic canonical record refused", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompetitionExpiredControlRecord(connection, limits); err == nil {
		t.Fatal("mutable record accepted")
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompetitionExpiredControlRecord(connection, limits); err == nil {
		t.Fatal("hard-linked record accepted")
	}
	if err := os.Remove(path + ".alias"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	content = append([]byte(" "), content...)
	if err := os.WriteFile(path, content, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	connection.ExpiredControlBridge.RecordSha256 = hex.EncodeToString(digest[:])
	if _, err := readCompetitionExpiredControlRecord(connection, limits); err == nil {
		t.Fatal("noncanonical root record accepted")
	}
}
