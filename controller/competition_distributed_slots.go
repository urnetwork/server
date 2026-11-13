// Distributed slots retain one execution owner per qualified host while the
// epoch-one singleton remains a distinct, backwards-compatible resource.
package controller

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// A selected host is authority only after exact frozen-round derivation.
func competitionExecutorSlotId(settings *Settings) int {
	if competitionDistributedEnabled(settings) {
		return settings.executorSlotId
	}
	return 1
}

// Only trusted, fixed SQL identifiers are returned; no configuration is SQL.
func competitionExecutionSlotTable(settings *Settings) string {
	if competitionDistributedEnabled(settings) {
		return "competition_execution_slot"
	}
	return "competition_worker_slot"
}

// Persistence cleanup lacks Settings, so its captured typed authority chooses
// the table. A caller cannot revive an obsolete owner by reusing its worker id.
func competitionExecutionSlotForContext(ctx context.Context) (string, int) {
	if authority, present := ctx.Value(competitionJobAuthorityKey{}).(competitionJobAuthority); present && authority.SlotFencingToken > 0 {
		return "competition_execution_slot", authority.SlotId
	}
	return "competition_worker_slot", 1
}

// Every non-claim operation presents the exact fence returned by its claim.
// SQL independently checks these values against the immutable job and slot.
func requireCompetitionExecutorAuthorityInTx(ctx context.Context, tx server.PgTx, settings *Settings, jobId server.Id) error {
	if !competitionDistributedEnabled(settings) {
		return nil
	}
	authority, present := ctx.Value(competitionJobAuthorityKey{}).(competitionJobAuthority)
	if !present || authority.JobId != jobId || authority.ExecutorHostId != settings.executorHostId ||
		authority.SlotId != settings.executorSlotId || authority.SlotFencingToken <= 0 {
		return ErrLeaseLost
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job j JOIN competition_execution_slot s ON s.slot_id=j.executor_slot_id
	 WHERE j.job_id=$1 AND j.executor_host_id=$2 AND j.executor_slot_id=$3 AND j.executor_fencing_token=$4
	 AND s.host_id=j.executor_host_id AND s.job_id=j.job_id AND s.fencing_token=j.executor_fencing_token)`,
		jobId, authority.ExecutorHostId, authority.SlotId, authority.SlotFencingToken).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return ErrLeaseLost
	}
	return setCompetitionExecutorAuthorityInTx(ctx, tx, authority.ExecutorHostId, authority.SlotId, authority.SlotFencingToken)
}

// Claims obtain these fields from locked database rows, never request input.
func setCompetitionExecutorAuthorityInTx(ctx context.Context, tx server.PgTx, hostId string, slotId int, fence int64) error {
	_, err := tx.Exec(ctx, `SELECT set_config('urnetwork.competition_executor_host',$1,true),
	 set_config('urnetwork.competition_executor_slot',$2,true),set_config('urnetwork.competition_executor_fence',$3,true)`,
		hostId, strconv.Itoa(slotId), strconv.FormatInt(fence, 10))
	return err
}

// A frozen shared baseline and an idle legacy containment owner are required
// before dispatch. The coordinator never creates a baseline independently on
// each host; each selected host consumes the same retained control bytes.
func (self PostgresStore) claimDistributedCompetitionJob(ctx context.Context, settings *Settings, workerId, workerImageDigest string) (job *queuedJob, resultErr error) {
	if settings.automationLease == nil || settings.automationRoundId == (server.Id{}) {
		return nil, ErrLeaseLost
	}
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			job = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "evaluation"))
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.competition_v2_worker = 'on'`))
			var legacyWorker *string
			var legacyJob *server.Id
			var legacyExpiry *time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT worker_id,job_id,lease_expires_at FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&legacyWorker, &legacyJob, &legacyExpiry))
			if legacyJob != nil {
				return
			}
			if legacyWorker != nil {
				now := self.nowUtc().Truncate(time.Microsecond)
				if legacyExpiry == nil || now.Before(*legacyExpiry) {
					return
				}
				var unpublishedPreparation bool
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job_event e JOIN competition_job j ON j.job_id=e.job_id
				 LEFT JOIN competition_round_baseline b ON b.round_id=j.round_id WHERE e.actor_id=$1
				 AND e.event_type='round_baseline_preparation_reserved' AND b.round_id IS NULL)`, *legacyWorker).Scan(&unpublishedPreparation))
				if unpublishedPreparation {
					return
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=NULL,job_id=NULL,lease_expires_at=NULL,heartbeat_at=$1
				 WHERE slot_id=1`, now))
			}
			var legacyRunning bool
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job WHERE state='running' AND executor_host_id IS NULL)`).Scan(&legacyRunning))
			if legacyRunning {
				return
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_execution_slot(slot_id,host_id) VALUES($1,$2) ON CONFLICT(slot_id) DO NOTHING`, settings.executorSlotId, settings.executorHostId))
			var hostId string
			var slotWorker *string
			var slotJob *server.Id
			var slotLease *time.Time
			var slotFence int64
			server.Raise(tx.QueryRow(ctx, `SELECT host_id,worker_id,job_id,lease_expires_at,fencing_token FROM competition_execution_slot WHERE slot_id=$1 FOR UPDATE`, settings.executorSlotId).
				Scan(&hostId, &slotWorker, &slotJob, &slotLease, &slotFence))
			if hostId != settings.executorHostId {
				panic(ErrConflict)
			}
			if (settings.executorReconcileExpiredOnly || settings.executorReconcileOnly) && slotJob == nil {
				return
			}
			now := self.nowUtc().Truncate(time.Microsecond)
			if slotJob != nil && slotWorker != nil && slotLease != nil && now.Before(*slotLease) {
				return
			}
			var scanErr error
			reconcile := slotJob != nil
			if self.beforeLeaseRowLockForTest != nil {
				self.beforeLeaseRowLockForTest("distributed_job")
			}
			if reconcile {
				job, scanErr = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.job_id=$1 FOR UPDATE OF j`, *slotJob), true, now)
			} else {
				// The scope lock serializes these short claims across hosts. Do not
				// skip a locked oldest row and invert the admission's true FIFO.
				job, scanErr = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.state='queued' AND j.available_at<=$1
				 AND j.round_id=$2 AND r.competition_id=$3 AND NOT r.canceled AND r.opens_at<=$1
				 AND r.opens_at<=j.submitted_at AND j.submitted_at<r.closes_at
				 AND r.finalized_at IS NULL AND r.automation_sealed_at IS NULL
				 ORDER BY j.submitted_at,j.job_id LIMIT 1 FOR UPDATE OF j`,
					now, settings.automationRoundId, settings.CompetitionId), true, now)
			}
			if errors.Is(scanErr, pgx.ErrNoRows) {
				job = nil
				return
			}
			server.Raise(scanErr)
			if job.RoundId != settings.automationRoundId || !roundUsesAutomation(&job.Round) {
				panic(ErrLeaseLost)
			}
			_, policyErr := settingsForFrozenRound(settings, &job.Round)
			server.Raise(policyErr)
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, job.RoundId))
			baseline, baselineErr := scanRoundBaseline(tx.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,
			 source_artifact_manifest_sha256,base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at
			 FROM competition_round_baseline WHERE round_id=$1`, job.RoundId))
			server.Raise(baselineErr)
			server.Raise(validateRoundBaselineRecord(settings, &job.Round, baseline))
			job.RoundBaseline = baseline
			server.Raise(validateCompetitionSecurityToolchainScope(settings, &job.Round, baseline))
			server.Raise(requireCompetitionSecurityToolchainDecisionInTx(ctx, tx, settings, baseline))
			if settings.roundBaselinePreparation != nil {
				allowed, gateErr := self.allowPreparedRoundBaselineClaimInTx(ctx, tx, settings, job, baseline)
				server.Raise(gateErr)
				if !allowed {
					job = nil
					return
				}
			}
			var qualified bool
			server.Raise(tx.QueryRow(ctx, `SELECT competition_executor_control_hosts_ready($1)`, job.RoundId).Scan(&qualified))
			if !qualified {
				job = nil
				return
			}
			allowed, gateErr := competitionIntakeFirstCandidateClaimInTx(ctx, tx, settings, job.RoundId)
			server.Raise(gateErr)
			if !allowed {
				job = nil
				return
			}
			executionWorkerImageDigest := workerImageDigest
			if reconcile {
				executionWorkerImageDigest = job.WorkerImageDigest
			}
			if !imageDigestPattern.MatchString(executionWorkerImageDigest) || job.EvaluationCycle > 0 && (job.Reevaluation == nil || executionWorkerImageDigest != job.Reevaluation.FixedWorkerImageDigest) {
				panic(ErrLeaseLost)
			}
			// Job and baseline reads may wait after the slot acquisition. A real
			// claim's clock starts only after those waits and a fresh parent fence.
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			now = self.nowUtc().Truncate(time.Microsecond)
			if job.EvaluationCycle > 0 && job.Reevaluation != nil && job.Reevaluation.Schema == 2 {
				var clockErr error
				now, clockErr = competitionDistributedRecoveryDatabaseTime(ctx, tx)
				server.Raise(clockErr)
				if now.Before(job.Reevaluation.AuthorizedAt) {
					panic(ErrLeaseLost)
				}
			}
			if reconcile && job.ExecutionDeadline != nil && now.Before(*job.ExecutionDeadline) {
				var rejected bool
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job_event WHERE job_id=$1 AND event_type='execution_custody_released'
 AND payload_json->>'reason'='executor_integrity_rejected' AND (payload_json->>'evaluation_cycle')::integer=$2
 AND (payload_json->>'attempt')::integer=$3)`, job.JobId, job.EvaluationCycle, job.AttemptCount).Scan(&rejected))
				if rejected {
					job = nil
					return
				}
			}
			if settings.executorReconcileExpiredOnly && (job.ExecutionDeadline == nil || now.Before(*job.ExecutionDeadline)) {
				job = nil
				return
			}
			if reconcile && (job.State != "running" || job.ExecutorHostId != hostId || job.SlotId != settings.executorSlotId ||
				job.SlotFencingToken != slotFence || job.ExecutionDeadline == nil || job.LeaseExpiresAt == nil || now.Before(*job.LeaseExpiresAt)) {
				panic(ErrLeaseLost)
			}
			fence := slotFence + 1
			server.Raise(setCompetitionExecutorAuthorityInTx(ctx, tx, hostId, settings.executorSlotId, fence))
			server.Raise(setCompetitionJobAuthorityInTx(ctx, tx, job.JobId, job.EvaluationCycle, job.AttemptCount))
			leaseUntil := now.Add(time.Duration(settings.WorkerLeaseSeconds) * time.Second)
			deadline := job.ExecutionDeadline
			if deadline == nil {
				started := now
				if job.EvaluationCycle > 0 {
					if job.CycleStartedAt != nil {
						started = *job.CycleStartedAt
					}
				} else if job.StartedAt != nil {
					started = *job.StartedAt
				}
				value := started.Add(time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
				deadline = &value
			}
			increment := 1
			if reconcile {
				increment = 0
			}
			updated, updateErr := tx.Exec(ctx, `UPDATE competition_job SET state='running',started_at=COALESCE(started_at,$2),
			 cycle_started_at=CASE WHEN evaluation_cycle>0 THEN COALESCE(cycle_started_at,$2) ELSE cycle_started_at END,
			 lease_owner=$3,lease_expires_at=$4,attempt_count=attempt_count+$5,worker_image_digest=$6,
			 executor_host_id=$7,executor_slot_id=$8,executor_fencing_token=$9,execution_deadline_at=$10 WHERE job_id=$1`,
				job.JobId, now, workerId, leaseUntil, increment, executionWorkerImageDigest, hostId, settings.executorSlotId, fence, *deadline)
			server.Raise(updateErr)
			if updated.RowsAffected() != 1 {
				panic(ErrLeaseLost)
			}
			server.Raise(setCompetitionJobAuthorityInTx(ctx, tx, job.JobId, job.EvaluationCycle, job.AttemptCount+increment))
			updated, updateErr = tx.Exec(ctx, `UPDATE competition_execution_slot SET round_id=$2,worker_id=$3,job_id=$4,
			 lease_expires_at=$5,heartbeat_at=$6,fencing_token=$7 WHERE slot_id=$1 AND host_id=$8`,
				settings.executorSlotId, job.RoundId, workerId, job.JobId, leaseUntil, now, fence, hostId)
			server.Raise(updateErr)
			if updated.RowsAffected() != 1 {
				panic(ErrLeaseLost)
			}
			eventType := "claimed"
			if reconcile {
				eventType = "execution_reconciled"
			}
			appendEvent(ctx, tx, job.JobId, now, eventType, workerId, map[string]any{
				"attempt": job.AttemptCount + increment, "worker_image_digest": executionWorkerImageDigest,
				"executor_host_id": hostId, "executor_slot_id": settings.executorSlotId,
				"executor_fencing_token": fence, "execution_deadline_at": *deadline,
			})
			job.State, job.LeaseOwner, job.LeaseExpiresAt = "running", workerId, &leaseUntil
			job.AttemptCount += increment
			job.WorkerImageDigest, job.ExecutorHostId, job.SlotId, job.SlotFencingToken, job.ExecutionDeadline = executionWorkerImageDigest, hostId, settings.executorSlotId, fence, deadline
			job.executorSettings = settings
			if job.StartedAt == nil {
				job.StartedAt = &now
			}
			if job.EvaluationCycle > 0 && job.CycleStartedAt == nil {
				job.CycleStartedAt, job.Reevaluation.StartedAt = &now, &now
			}
		})
	})
	if resultErr != nil {
		return nil, resultErr
	}
	return job, nil
}

// Shutdown withdraws only coordinator custody, not the possibly running remote
// execution. Its next owner reconciles the same host/job/attempt and clock.
func (self PostgresStore) handBackDistributedCompetitionJob(ctx context.Context, workerId string, jobId server.Id, reason string, failure competitionHeartbeatFailure, authority competitionJobAuthority) error {
	settings := authority.ExecutorSettings
	if !competitionDistributedEnabled(settings) || settings.automationLease == nil || authority.JobId != jobId {
		return ErrLeaseLost
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			var slotFence int64
			loadErr := tx.QueryRow(ctx, `SELECT fencing_token FROM competition_execution_slot
			 WHERE slot_id=$1 AND host_id=$2 AND job_id=$3 AND worker_id=$4 FOR UPDATE`, authority.SlotId, authority.ExecutorHostId, jobId, workerId).Scan(&slotFence)
			if errors.Is(loadErr, pgx.ErrNoRows) {
				panic(ErrLeaseLost)
			}
			server.Raise(loadErr)
			var cycle, attempt int
			loadErr = tx.QueryRow(ctx, `SELECT evaluation_cycle,attempt_count FROM competition_job
			 WHERE job_id=$1 AND state='running' AND lease_owner=$2 FOR UPDATE`, jobId, workerId).Scan(&cycle, &attempt)
			if errors.Is(loadErr, pgx.ErrNoRows) || slotFence != authority.SlotFencingToken {
				panic(ErrLeaseLost)
			}
			server.Raise(loadErr)
			server.Raise(requireCompetitionJobAuthorityInTx(ctx, tx, jobId, cycle, attempt))
			server.Raise(requireCompetitionExecutorAuthorityInTx(ctx, tx, settings, jobId))
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.competition_v2_worker = 'on'`))
			now := self.nowUtc()
			updated, updateErr := tx.Exec(ctx, `UPDATE competition_job SET lease_expires_at=$2 WHERE job_id=$1`, jobId, now)
			server.Raise(updateErr)
			if updated.RowsAffected() != 1 {
				panic(ErrLeaseLost)
			}
			updated, updateErr = tx.Exec(ctx, `UPDATE competition_execution_slot SET lease_expires_at=$2,heartbeat_at=$2 WHERE slot_id=$1`, authority.SlotId, now)
			server.Raise(updateErr)
			if updated.RowsAffected() != 1 {
				panic(ErrLeaseLost)
			}
			appendEvent(ctx, tx, jobId, now, "execution_custody_released", workerId, struct {
				competitionHandBackEvent
				EvaluationCycle int `json:"evaluation_cycle"`
				Attempt         int `json:"attempt"`
			}{competitionHandBackEvent: competitionHandBackEvent{Reason: reason, competitionHeartbeatFailure: failure}, EvaluationCycle: cycle, Attempt: attempt})
		})
	})
}
