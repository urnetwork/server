// Distributed recovery queues only an already admitted failed job after two
// held, globally idle database censuses bracket authenticated native closure.
// No transaction retains a database connection across transport or blob I/O.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The locked original is retained unchanged in the immutable audit before any
// cycle metadata moves. Derivation authenticates source/runtime outside locks.
type competitionDistributedRecoveryGate struct {
	job      *queuedJob
	terminal json.RawMessage
	baseline *roundBaselineRecord
	settings *Settings
	now      time.Time
}

// Clock allocation is a database observation made after authority/row waits.
// An operator approval timestamp and the old submission start cannot supply it.
func competitionDistributedRecoveryDatabaseTime(ctx context.Context, tx server.PgTx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT timezone('UTC',clock_timestamp())`).Scan(&now)
	return now.UTC().Truncate(time.Microsecond), err
}

// Every legacy and frozen distributed slot is locked and positively empty;
// expired ownership is not absence. No lease is renewed or reclaimed here.
func (self PostgresStore) lockCompetitionDistributedRecoveryIdle(ctx context.Context, tx server.PgTx, settings *Settings, auth model.CompetitionJobReevaluationAuthorization) error {
	d := auth.Distributed
	var legacyWorker *string
	var legacyJob *server.Id
	var legacyExpiry *time.Time
	if err := tx.QueryRow(ctx, `SELECT worker_id,job_id,lease_expires_at FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&legacyWorker, &legacyJob, &legacyExpiry); err != nil {
		return err
	}
	if legacyWorker != nil || legacyJob != nil || legacyExpiry != nil {
		return ErrLeaseLost
	}
	var running bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job WHERE state='running')`).Scan(&running); err != nil {
		return err
	}
	if running {
		return ErrLeaseLost
	}
	policy := settings.DistributedEvaluationPolicy
	if policy == nil || len(d.IdleSlots) != len(policy.Hosts) {
		return ErrConflict
	}
	for _, expected := range d.IdleSlots {
		approved := false
		for _, host := range policy.Hosts {
			approved = approved || host.HostId == expected.HostId && host.SlotId == expected.SlotId
		}
		if !approved {
			return ErrConflict
		}
		var hostId string
		var worker *string
		var job *server.Id
		var expiry *time.Time
		var fence int64
		if err := tx.QueryRow(ctx, `SELECT host_id,worker_id,job_id,lease_expires_at,fencing_token FROM competition_execution_slot WHERE slot_id=$1 FOR UPDATE`, expected.SlotId).
			Scan(&hostId, &worker, &job, &expiry, &fence); err != nil {
			return err
		}
		if hostId != expected.HostId || worker != nil || job != nil || expiry != nil || fence != expected.FencingToken {
			return ErrLeaseLost
		}
	}
	// Unknown occupied hosts cannot hide outside this round's frozen census.
	var occupied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_execution_slot WHERE worker_id IS NOT NULL OR job_id IS NOT NULL OR lease_expires_at IS NOT NULL)`).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return ErrLeaseLost
	}
	lease := model.CompetitionSupervisorLease{Environment: auth.Environment, CompetitionId: auth.CompetitionId, SeasonId: auth.SeasonId,
		OwnerId: d.SupervisorOwnerId, FencingToken: d.SupervisorFencingToken}
	var actualOwner string
	var actualFence int64
	var expiry time.Time
	if err := tx.QueryRow(ctx, `SELECT owner_id,fencing_token,lease_expires_at FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3 FOR UPDATE`, auth.Environment, auth.CompetitionId, auth.SeasonId).
		Scan(&actualOwner, &actualFence, &expiry); err != nil {
		return err
	}
	now, err := competitionDistributedRecoveryDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	if actualOwner != lease.OwnerId || actualFence != lease.FencingToken || !now.Before(expiry) {
		return ErrLeaseLost
	}
	return setAutomationLeaseGuardsInTx(ctx, tx, settings, lease)
}

// This gate deliberately never asks grading_ready: the mandatory evaluation
// hold makes that projection false. Source, baseline and host authority retain
// their own independent checks, including the historical security exception.
func (self PostgresStore) competitionDistributedRecoveryGateInTxWithLock(ctx context.Context, tx server.PgTx, settings *Settings, auth model.CompetitionJobReevaluationAuthorization) (*competitionDistributedRecoveryGate, error) {
	if self.beforeLeaseRowLockForTest != nil {
		self.beforeLeaseRowLockForTest("distributed_recovery")
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.job_id=$1 AND j.round_id=$2 FOR UPDATE OF j,r`, auth.JobId, auth.RoundId), false, self.nowUtc())
	if err != nil {
		return nil, err
	}
	if job.Round.Canceled || job.Round.FinalizedAt != nil || job.Round.AutomationSealedAt != nil || job.Round.AdmissionClosedAt != nil || job.Round.WinnerJobId != nil || job.Round.Staging ||
		job.Round.CompetitionId != settings.CompetitionId || job.State != "failed" || job.EvaluationCycle != 0 || job.AttemptCount != auth.ExpectedAttemptCount ||
		job.StartedAt == nil || job.CompletedAt == nil || job.ExecutionDeadline == nil || job.Score != nil || job.LeaseOwner != "" || job.LeaseExpiresAt != nil ||
		job.EvalError == nil || !model.CompetitionJobReevaluationFailureAllowed(auth.IncidentClass, model.CompetitionEvaluationFailure{Kind: job.EvalError.Kind, Code: job.EvalError.Code, Retriable: job.EvalError.Retriable}) {
		return nil, ErrConflict
	}
	d := auth.Distributed
	patchRetry := auth.IncidentClass == model.CompetitionJobReevaluationOperatorRequestedPatchRetry
	if job.ExecutorHostId != d.OriginalHostId || job.SlotId != d.OriginalSlotId || job.SlotFencingToken != d.OriginalFencingToken ||
		!job.StartedAt.Equal(d.OriginalStartedAt) || !job.ExecutionDeadline.Equal(d.OriginalDeadlineAt) ||
		(!patchRetry && job.CompletedAt.Before(*job.ExecutionDeadline)) ||
		(patchRetry && (d.PatchRetry == nil || d.PatchRetry.PatchSha256 != job.PatchSha256 || job.CompletedAt.Before(*job.StartedAt) || !job.CompletedAt.Before(*job.ExecutionDeadline))) ||
		settings.DistributedEvaluationPolicy == nil || settings.DistributedEvaluationPolicy.ApprovalSha256 != d.PolicyApprovalSha256 ||
		settings.AutomationPolicy.SourceLockSha256 != d.SourceLockSha256 || job.EvaluatorImageDigest != d.EvaluatorImageDigest ||
		settings.EvaluationPolicy.ScoreTimeoutSeconds != 3*60*60 {
		return nil, ErrConflict
	}
	holds, err := readCompetitionIntakeHolds(ctx, tx, settings)
	if err != nil {
		return nil, err
	}
	if !holds.Evaluation || auth.PostCloseApproval != nil && !holds.Finalization {
		return nil, ErrAutomationHeld
	}
	if err := self.lockCompetitionDistributedRecoveryIdle(ctx, tx, settings, auth); err != nil {
		return nil, err
	}
	now, err := competitionDistributedRecoveryDatabaseTime(ctx, tx)
	if err != nil || now.Before(*job.CompletedAt) || !competitionReevaluationWithinApprovedWindow(auth, &job.Round, now) {
		return nil, ErrConflict
	}
	var terminal []byte
	if err := tx.QueryRow(ctx, `SELECT to_jsonb(j) FROM competition_job j WHERE job_id=$1`, job.JobId).Scan(&terminal); err != nil {
		return nil, err
	}
	digest, err := CompetitionJobReevaluationSnapshotDigest(terminal)
	if err != nil || digest != auth.ExpectedTerminalSha256 {
		return nil, ErrConflict
	}
	policyDigest, err := CompetitionJobReevaluationSnapshotDigest(job.Round.PolicyJson)
	if err != nil || policyDigest != auth.RoundPolicySha256 {
		return nil, ErrConflict
	}
	if err := ensureAutomationRoundValidInTx(ctx, tx, job.RoundId); err != nil {
		return nil, err
	}
	current, err := currentSourceActivationInTx(ctx, tx, settings)
	if err != nil || current.ActivatedAt == nil || current.AbandonedAt != nil || current.TargetRoundId != job.RoundId || current.TargetEpoch != job.Round.Epoch ||
		current.Commits["server"] != settings.BaseSha || current.SourceLockSha256 != d.SourceLockSha256 {
		return nil, ErrConflict
	}
	baseline, err := scanRoundBaseline(tx.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,source_artifact_manifest_sha256,base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at FROM competition_round_baseline WHERE round_id=$1 FOR SHARE`, job.RoundId))
	if err != nil || validateRoundBaselineRecord(settings, &job.Round, baseline) != nil || baseline.Sha256 != auth.BaselineSha256 ||
		validateCompetitionSecurityToolchainScope(settings, &job.Round, baseline) != nil || requireCompetitionSecurityToolchainDecisionInTx(ctx, tx, settings, baseline) != nil {
		return nil, ErrConflict
	}
	var hostsReady bool
	if err := tx.QueryRow(ctx, `SELECT competition_executor_control_hosts_ready($1)`, job.RoundId).Scan(&hostsReady); err != nil {
		return nil, err
	}
	if !hostsReady {
		return nil, ErrReviewNotReady
	}
	// Historical terminal events intentionally omit error_code. The locked full
	// terminal snapshot binds it; event identity/attempt/time/digest stay exact.
	var eventAt time.Time
	var eventType, eventDigest string
	var eventJson []byte
	if err := tx.QueryRow(ctx, `SELECT event_at,event_type,payload_sha256,payload_json FROM competition_job_event WHERE event_id=$1 AND job_id=$2`, auth.IncidentEventId, job.JobId).
		Scan(&eventAt, &eventType, &eventDigest, &eventJson); err != nil {
		return nil, err
	}
	var event struct {
		Attempt int `json:"attempt"`
	}
	if json.Unmarshal(eventJson, &event) != nil || event.Attempt != job.AttemptCount || eventType != "failed" || eventDigest != auth.IncidentEventSha256 || !eventAt.Equal(*job.CompletedAt) {
		return nil, ErrConflict
	}
	now, err = competitionDistributedRecoveryDatabaseTime(ctx, tx)
	if err != nil || now.Before(*job.CompletedAt) || !competitionReevaluationWithinApprovedWindow(auth, &job.Round, now) {
		return nil, ErrConflict
	}
	return &competitionDistributedRecoveryGate{job: job, terminal: terminal, baseline: baseline, settings: settings, now: now}, nil
}

// Immutable exact replay takes precedence over deployment/native observations.
// No replay sends transport, allocates a clock or changes any database row.
func (self PostgresStore) authorizeDistributedJobReevaluation(ctx context.Context, settings *Settings, principal *Principal, request model.CompetitionJobReevaluationRequest, transportForHost competitionDistributedRecoveryTransportFactory) (*model.CompetitionJobReevaluationReceipt, error) {
	if err := validateCompetitionDistributedRecoveryRequest(settings, principal, request); err != nil {
		return nil, err
	}
	requestDigest, err := model.CompetitionAutomationDigest(request)
	if err != nil {
		return nil, err
	}
	if previous, err := readCompetitionReevaluationReceipt(ctx, settings, request, requestDigest); err != nil || previous != nil {
		if err == nil && (previous.Schema != 2 || validateCompetitionDistributedRecoveryReceipt(previous) != nil) {
			return nil, ErrConflict
		}
		return previous, err
	}
	a := request.Authorization
	limits := defaultCompetitionDistributedReevaluationSettings()
	if settings.distributedReevaluationSettings != nil {
		limits = *settings.distributedReevaluationSettings
	}
	if limits.NativeProofLifetime <= 0 {
		return nil, ErrConflict
	}
	proof := settings.reevaluationRuntime
	if proof == nil || settings.reevaluationRuntimeSha256 != a.RuntimeQualificationSha256 || proof.SourceCommit != a.FixedSourceCommit ||
		proof.WorkerSha256 != a.FixedWorkerSha256 || proof.WorkerImageDigest != a.FixedWorkerImageDigest || proof.ApiImageDigest != a.ApiImageDigest {
		return nil, ErrConflict
	}
	copy := *settings
	bindRuntime := bindCompetitionDistributedReevaluationRuntime
	if a.IncidentClass == model.CompetitionJobReevaluationOperatorRequestedPatchRetry {
		bindRuntime = bindCompetitionOperatorRequestedPatchRetryRuntime
	}
	if bindRuntime(&copy, *proof, a.RuntimeQualificationSha256) != nil {
		return nil, ErrConflict
	}
	// Read the actual source envelope and retained execution exceptions before
	// entering any locked transaction. Never derive current execution from a tag.
	round, err := self.GetRound(ctx, settings, a.RoundId)
	if err != nil {
		return nil, err
	}
	frozen, err := settingsForAutomationRound(ctx, settings, self, round)
	if err != nil {
		return nil, err
	}
	frozen.automationRoundId = a.RoundId
	baseline, err := self.distributedRoundBaseline(ctx, frozen, a.RoundId)
	if err != nil || baseline == nil {
		return nil, ErrConflict
	}
	if err := self.loadExecutorControlAmendment(ctx, frozen, round); err != nil {
		return nil, err
	}
	if err := self.loadCompetitionReadinessSecurityToolchain(ctx, frozen, round, baseline); err != nil {
		return nil, err
	}
	// Execution still uses the separately configured approved exception. Reading
	// readiness authority above does not silently grant a new evaluator identity.
	var gate *competitionDistributedRecoveryGate
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, frozen)
			var gateErr error
			gate, gateErr = self.competitionDistributedRecoveryGateInTxWithLock(ctx, tx, frozen, a)
			server.Raise(gateErr)
		})
	})
	if err != nil {
		return nil, err
	}
	nativeProof, err := observeCompetitionDistributedRecovery(ctx, frozen, &gate.job.Round, request, transportForHost)
	if err != nil {
		return nil, err
	}
	nativeProof.DatabaseBracketStartedAt = gate.now
	nativeProof.DatabaseBracketExpiresAt = gate.now.Add(limits.NativeProofLifetime)
	nativeJson, nativeDigest, err := competitionDistributedRecoveryProofJson(nativeProof)
	if err != nil {
		return nil, err
	}
	var receipt *model.CompetitionJobReevaluationReceipt
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			receipt = nil
			automationScopeLockInTx(ctx, tx, frozen)
			var oldKey, oldDigest string
			var oldReceipt []byte
			loadErr := tx.QueryRow(ctx, `SELECT idempotency_key,request_sha256,receipt_json FROM competition_job_reevaluation WHERE job_id=$1`, a.JobId).Scan(&oldKey, &oldDigest, &oldReceipt)
			if loadErr == nil {
				if oldKey != request.IdempotencyKey || oldDigest != requestDigest {
					panic(ErrConflict)
				}
				receipt = &model.CompetitionJobReevaluationReceipt{}
				server.Raise(decodeFailureEvidenceJson(oldReceipt, receipt))
				server.Raise(model.ValidateCompetitionJobReevaluationCycle(&receipt.CompetitionJobReevaluationCycle))
				server.Raise(validateCompetitionDistributedRecoveryReceipt(receipt))
				return
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			current, gateErr := self.competitionDistributedRecoveryGateInTxWithLock(ctx, tx, frozen, a)
			server.Raise(gateErr)
			if !competitionDistributedRecoveryProofTimely(gate.now, current.now, limits.NativeProofLifetime) {
				panic(ErrLeaseLost)
			}
			server.Raise(ctx.Err())
			d := a.Distributed
			cycle := model.CompetitionJobReevaluationCycle{Schema: 2, Environment: a.Environment, CompetitionId: a.CompetitionId, SeasonId: a.SeasonId,
				JobId: a.JobId, RoundId: a.RoundId, PatchSha256: current.job.PatchSha256, RoundPolicySha256: a.RoundPolicySha256, BaselineSha256: a.BaselineSha256,
				Cycle: 1, AuthorizationSha256: request.AuthorizationSha256, EvidenceSha256: a.EvidenceSha256, PreviousTerminalSha256: a.ExpectedTerminalSha256,
				PreviousFailure: model.CompetitionEvaluationFailure{Kind: current.job.EvalError.Kind, Code: current.job.EvalError.Code, Retriable: current.job.EvalError.Retriable},
				IncidentClass:   a.IncidentClass, RuntimeQualificationSha256: a.RuntimeQualificationSha256, FixedSourceCommit: a.FixedSourceCommit,
				FixedWorkerSha256: a.FixedWorkerSha256, FixedWorkerImageDigest: a.FixedWorkerImageDigest, ApiImageDigest: a.ApiImageDigest, AuthorizedAt: current.now,
				ExecutionTimeoutSeconds: 3 * 60 * 60, Distributed: &model.CompetitionDistributedJobReevaluationCycle{Schema: 1, Reason: d.Reason, OriginalHostId: d.OriginalHostId,
					OriginalSlotId: d.OriginalSlotId, OriginalFencingToken: d.OriginalFencingToken, OriginalRunId: d.OriginalRunId, OriginalStartedAt: d.OriginalStartedAt,
					OriginalDeadlineAt: d.OriginalDeadlineAt, SourceLockSha256: d.SourceLockSha256, EvaluatorImageDigest: d.EvaluatorImageDigest,
					PolicyApprovalSha256: d.PolicyApprovalSha256, ClosureEvidenceSha256: d.ClosureEvidenceSha256, NativeProofSha256: nativeDigest, PatchRetry: d.PatchRetry}}
			var digestErr error
			cycle.ReceiptSha256, digestErr = model.CompetitionJobReevaluationCycleDigest(cycle)
			server.Raise(digestErr)
			server.Raise(model.ValidateCompetitionJobReevaluationCycle(&cycle))
			receipt = &model.CompetitionJobReevaluationReceipt{CompetitionJobReevaluationCycle: cycle, RequestSha256: requestDigest, AttemptOffset: current.job.AttemptCount, DistributedProof: nativeJson}
			server.Raise(validateCompetitionDistributedRecoveryReceipt(receipt))
			authorizationJson, jsonErr := json.Marshal(a)
			server.Raise(jsonErr)
			receiptJson, jsonErr := json.Marshal(receipt)
			server.Raise(jsonErr)
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_reevaluation_operator',$1,true),set_config('urnetwork.competition_reevaluation_request',$2,true),set_config('urnetwork.competition_v2_worker','on',true),set_config('urnetwork.competition_distributed_recovery_proof',$3,true)`, principal.Id, requestDigest, nativeDigest))
			if a.IncidentClass == model.CompetitionJobReevaluationOperatorRequestedPatchRetry {
				server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_patch_retry_proof',$1,true)`, nativeDigest))
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_job_reevaluation(job_id,round_id,environment,competition_id,season_id,operator_id,idempotency_key,request_sha256,authorization_sha256,authorized_at,original_terminal_json,authorization_json,receipt_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13::jsonb)`, a.JobId, a.RoundId, a.Environment, a.CompetitionId, a.SeasonId, principal.Id, request.IdempotencyKey, requestDigest, request.AuthorizationSha256, current.now, string(current.terminal), string(authorizationJson), string(receiptJson)))
			for _, slot := range d.IdleSlots {
				updated, updateErr := tx.Exec(ctx, `UPDATE competition_execution_slot SET fencing_token=fencing_token+1 WHERE slot_id=$1 AND host_id=$2 AND fencing_token=$3 AND worker_id IS NULL AND job_id IS NULL AND lease_expires_at IS NULL`, slot.SlotId, slot.HostId, slot.FencingToken)
				server.Raise(updateErr)
				if updated.RowsAffected() != 1 {
					panic(ErrLeaseLost)
				}
			}
			server.Raise(ctx.Err())
			updated, updateErr := tx.Exec(ctx, `UPDATE competition_job SET evaluation_cycle=1,cycle_attempt_offset=attempt_count,cycle_started_at=NULL,state='queued',available_at=$2,completed_at=NULL,score_json=NULL,eval_error_json=NULL,artifact_manifest_json=NULL,artifact_manifest_sha256=NULL,executor_host_id=NULL,executor_slot_id=NULL,executor_fencing_token=NULL,execution_deadline_at=NULL WHERE job_id=$1 AND state='failed' AND evaluation_cycle=0`, a.JobId, current.now)
			server.Raise(updateErr)
			if updated.RowsAffected() != 1 {
				panic(ErrConflict)
			}
			eventReceipt := *receipt
			eventReceipt.DistributedProof = nil
			appendEvent(ctx, tx, a.JobId, current.now, "reevaluation_authorized", principal.Id, eventReceipt)
		})
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}
