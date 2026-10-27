// Preparation uses the existing singleton and append-only event log. A queued
// source job keeps its original FIFO identity and has no execution start until
// the baseline and real claim commit in one transaction.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// This is the normal claim order/window, including grading after admission
// closes. A running/retried job is observed but never granted a fresh clock.
const roundBaselinePreparationJobSql = `
 WHERE r.competition_id=$1 AND ($2::uuid IS NULL OR r.round_id=$2)
   AND ((j.state='queued' AND j.available_at<=$3) OR (j.state='running' AND j.lease_expires_at<=$3))
   AND NOT r.canceled AND r.finalized_at IS NULL AND r.automation_sealed_at IS NULL
   AND r.opens_at<=$3 AND r.opens_at<=j.submitted_at AND j.submitted_at<r.closes_at
 ORDER BY j.submitted_at,j.job_id LIMIT 1 FOR UPDATE OF j SKIP LOCKED`

// Every preparation boundary retains the existing supervisor/source fence.
// No database connection survives this short transaction into host work.
func (self PostgresStore) roundBaselinePreparationFenceInTx(ctx context.Context, tx server.PgTx, settings *Settings) {
	if settings.AutomationPolicy.Enabled {
		automationScopeLockInTx(ctx, tx, settings)
		if settings.automationLease == nil || settings.automationRoundId == (server.Id{}) {
			panic(ErrLeaseLost)
		}
		server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
		server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "evaluation"))
	}
	server.RaisePgResult(tx.Exec(ctx, `SET LOCAL urnetwork.competition_v2_worker = 'on'`))
}

// Reconstructing the typed payload restores its original canonical Go encoding
// after PostgreSQL jsonb key ordering, so the append-only event hash is checked.
func readRoundBaselinePreparationEvent(ctx context.Context, tx server.PgTx, roundId server.Id, maximum int64) (*roundBaselinePreparation, error) {
	var content []byte
	var digest, actor string
	var jobId server.Id
	err := tx.QueryRow(ctx, `SELECT e.payload_json,e.payload_sha256,e.actor_id,e.job_id FROM competition_job_event e
 JOIN competition_job j ON j.job_id=e.job_id
 WHERE j.round_id=$1 AND e.event_type='round_baseline_preparation_reserved' ORDER BY e.event_id LIMIT 1`, roundId).Scan(&content, &digest, &actor, &jobId)
	if err != nil {
		return nil, err
	}
	var preparation roundBaselinePreparation
	if err := decodeCompetitionPreparationJson(bytes.NewReader(content), maximum, &preparation); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(preparation)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(canonical)
	if digest != hex.EncodeToString(hash[:]) || actor != preparation.Owner || preparation.JobId != jobId || preparation.RoundId != roundId ||
		preparation.Schema != 1 || preparation.Kind != "sim-latency-round-baseline-preparation-reservation" ||
		!workerIdPattern.MatchString(preparation.Owner) || !workerIdPattern.MatchString(preparation.WorkerId) || !sha256Pattern.MatchString(preparation.ContextSha256) ||
		preparation.StartedAt.IsZero() || !preparation.StartedAt.Before(preparation.Deadline) {
		return nil, errors.New("baseline preparation reservation event is corrupt")
	}
	return &preparation, nil
}

// Reserve returning nil is not a claim capability: another owner can fail or
// a new job can arrive before Claim begins. The ordinary claim's singleton/job
// locks serialize this final baseline/HOLD check with publication and holds.
func (self PostgresStore) allowPreparedRoundBaselineClaimInTx(ctx context.Context, tx server.PgTx, settings *Settings, job *queuedJob, baseline *roundBaselineRecord) (bool, error) {
	reserved, err := readRoundBaselinePreparationEvent(ctx, tx, job.RoundId, settings.roundBaselinePreparation.Settings.MaximumRecordBytes)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if reserved != nil {
		var held bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job_event WHERE job_id=$1 AND event_type='round_baseline_preparation_held' AND payload_json->>'owner'=$2)`, reserved.JobId, reserved.Owner).Scan(&held); err != nil {
			return false, err
		}
		if held || baseline == nil {
			return false, errRoundBaselinePreparationHeld
		}
	}
	if baseline == nil {
		// A newly committed cold admission goes back through Reserve on the next
		// poll; no clock/attempt is started merely because a prior peek was empty.
		return false, nil
	}
	frozen, err := settingsForFrozenRound(settings, &job.Round)
	if err != nil {
		return false, err
	}
	if err := validateRoundBaselineRecord(frozen, &job.Round, baseline); err != nil {
		return false, err
	}
	return true, nil
}

// Reservation changes only the singleton and append-only audit events. In
// particular, job.state, attempt_count and started_at remain untouched.
func (self PostgresStore) ReserveRoundBaseline(ctx context.Context, settings *Settings, workerId, workerImage string) (preparation *roundBaselinePreparation, resultErr error) {
	if settings == nil || settings.roundBaselinePreparation == nil || !workerIdPattern.MatchString(workerId) || !imageDigestPattern.MatchString(workerImage) || settings.EvaluationPolicy.Replicates != 9 {
		return nil, errors.New("baseline preparation requires its worker profile and full R9")
	}
	profile := settings.roundBaselinePreparation.Settings
	if profile.TotalTimeout <= 0 || profile.CleanupTimeout <= 0 || profile.FenceInterval <= 0 || profile.MaximumRecordBytes <= 0 || profile.MaximumDiagnosticBytes <= 0 || profile.MaximumManifestBytes <= 0 || profile.MaximumEvidenceBytes <= 0 || profile.MaximumRawFileBytes <= 0 || profile.MaximumArtifacts <= 0 || profile.ArchiveAttempts <= 0 || profile.ArchiveBackoff <= 0 || profile.Now == nil {
		return nil, errors.New("baseline preparation owner bounds are invalid")
	}
	now := self.nowUtc()
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			preparation = nil
			self.roundBaselinePreparationFenceInTx(ctx, tx, settings)
			var slotOwner *string
			var slotJob *server.Id
			var slotDeadline *time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT worker_id,job_id,lease_expires_at FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&slotOwner, &slotJob, &slotDeadline))
			if slotOwner != nil && slotDeadline != nil && now.Before(*slotDeadline) {
				return
			}
			job, err := scanJob(tx.QueryRow(ctx, jobSelect+roundBaselinePreparationJobSql, settings.CompetitionId, optionalAutomationRoundId(settings), now), true, now)
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			server.Raise(err)
			frozen, err := settingsForFrozenRound(settings, &job.Round)
			server.Raise(err)
			reserved, reservationErr := readRoundBaselinePreparationEvent(ctx, tx, job.RoundId, profile.MaximumRecordBytes)
			if reservationErr != nil && !errors.Is(reservationErr, pgx.ErrNoRows) {
				server.Raise(reservationErr)
			}
			if reserved != nil {
				var held bool
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job_event WHERE job_id=$1 AND event_type='round_baseline_preparation_held' AND payload_json->>'owner'=$2)`, reserved.JobId, reserved.Owner).Scan(&held))
				if held {
					panic(errRoundBaselinePreparationHeld)
				}
			}
			baseline, err := scanRoundBaseline(tx.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,source_artifact_manifest_sha256,
 base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at FROM competition_round_baseline WHERE round_id=$1`, job.RoundId))
			if err == nil {
				server.Raise(validateRoundBaselineRecord(frozen, &job.Round, baseline))
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
			if reserved != nil || job.State != "queued" || job.StartedAt != nil || job.AttemptCount != 0 {
				panic(errRoundBaselinePreparationHeld)
			}
			if settings.AutomationPolicy.Enabled {
				server.Raise(ensureAutomationRoundValidInTx(ctx, tx, job.RoundId))
			}
			job.WorkerImageDigest = workerImage
			contextDigest, err := roundBaselinePreparationContext(frozen, job)
			server.Raise(err)
			owner := "baseline." + server.NewId().String()
			preparation = &roundBaselinePreparation{Schema: 1, Kind: "sim-latency-round-baseline-preparation-reservation", Owner: owner, WorkerId: workerId,
				JobId: job.JobId, RoundId: job.RoundId, PatchSha256: job.PatchSha256, ContextSha256: contextDigest,
				WorkerImage: workerImage, StartedAt: now, Deadline: now.Add(profile.TotalTimeout), SourceJob: job}
			appendEvent(ctx, tx, job.JobId, now, "round_baseline_preparation_reserved", owner, preparation)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=$1,job_id=NULL,lease_expires_at=$2,heartbeat_at=$3 WHERE slot_id=1`, owner, preparation.Deadline.Add(profile.CleanupTimeout), now))
		}, server.OptReadWrite())
	})
	return preparation, resultErr
}

// Checks the exact source job and original lease without renewing its deadline.
// Lease/fence expiry or any prior started attempt can never grant a fresh prep.
func (self PostgresStore) checkRoundBaselinePreparationInTx(ctx context.Context, tx server.PgTx, settings *Settings, preparation *roundBaselinePreparation) (*queuedJob, *Settings, error) {
	if preparation == nil || settings == nil || settings.roundBaselinePreparation == nil {
		return nil, nil, ErrConflict
	}
	self.roundBaselinePreparationFenceInTx(ctx, tx, settings)
	now := self.nowUtc()
	var owner *string
	var slotJob *server.Id
	var deadline *time.Time
	if err := tx.QueryRow(ctx, `SELECT worker_id,job_id,lease_expires_at FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&owner, &slotJob, &deadline); err != nil {
		return nil, nil, err
	}
	if owner == nil || *owner != preparation.Owner || slotJob != nil || deadline == nil || !now.Before(*deadline) || !now.Before(preparation.Deadline) {
		return nil, nil, ErrLeaseLost
	}
	job, err := scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.job_id=$1 FOR UPDATE OF j`, preparation.JobId), true, now)
	if err != nil {
		return nil, nil, err
	}
	if job.RoundId != preparation.RoundId || job.State != "queued" || job.StartedAt != nil || job.AttemptCount != 0 || job.Round.Canceled || job.Round.FinalizedAt != nil || job.Round.AutomationSealedAt != nil || now.Before(job.Round.OpensAt) || job.SubmittedAt.Before(job.Round.OpensAt) || !job.SubmittedAt.Before(job.Round.ClosesAt) {
		return nil, nil, ErrConflict
	}
	frozen, err := settingsForFrozenRound(settings, &job.Round)
	if err != nil {
		return nil, nil, err
	}
	if settings.AutomationPolicy.Enabled {
		if settings.automationRoundId != job.RoundId {
			return nil, nil, ErrLeaseLost
		}
		if err := ensureAutomationRoundValidInTx(ctx, tx, job.RoundId); err != nil {
			return nil, nil, err
		}
	}
	job.WorkerImageDigest = preparation.WorkerImage
	digest, err := roundBaselinePreparationContext(frozen, job)
	if err != nil || digest != preparation.ContextSha256 || job.PatchSha256 != preparation.PatchSha256 {
		return nil, nil, ErrConflict
	}
	reserved, err := readRoundBaselinePreparationEvent(ctx, tx, job.RoundId, settings.roundBaselinePreparation.Settings.MaximumRecordBytes)
	if err != nil || reserved.Owner != preparation.Owner || reserved.JobId != preparation.JobId || reserved.WorkerId != preparation.WorkerId || reserved.WorkerImage != preparation.WorkerImage || reserved.RoundId != preparation.RoundId || reserved.PatchSha256 != preparation.PatchSha256 || reserved.ContextSha256 != preparation.ContextSha256 || !reserved.StartedAt.Equal(preparation.StartedAt) || !reserved.Deadline.Equal(preparation.Deadline) || preparation.Deadline.Sub(preparation.StartedAt) != settings.roundBaselinePreparation.Settings.TotalTimeout {
		return nil, nil, ErrConflict
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job_event WHERE job_id=$1 AND event_type='round_baseline_preparation_held' AND payload_json->>'owner'=$2)`, reserved.JobId, reserved.Owner).Scan(&held); err != nil {
		return nil, nil, err
	}
	if held {
		return nil, nil, errRoundBaselinePreparationHeld
	}
	return job, frozen, nil
}

// A parent-owned periodic check also covers the producer's archive/backoff work.
func (self PostgresStore) CheckRoundBaselinePreparation(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation) error {
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			_, _, err := self.checkRoundBaselinePreparationInTx(ctx, tx, settings, preparation)
			server.Raise(err)
			result, err := tx.Exec(ctx, `UPDATE competition_worker_slot SET heartbeat_at=$2 WHERE slot_id=1 AND worker_id=$1 AND job_id IS NULL`, preparation.Owner, self.nowUtc())
			server.Raise(err)
			if result.RowsAffected() != 1 {
				panic(ErrLeaseLost)
			}
		}, server.OptReadWrite())
	})
}

// The reservation itself is the permanent no-resampling fence; this additional
// event gives operators a bounded explicit infrastructure-hold classification.
func (self PostgresStore) HoldRoundBaselinePreparation(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation, hold roundBaselinePreparationHold) error {
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			// Match Claim/Check/publication lock order, including when its source
			// job is not the queued job selected by a concurrent warm claim.
			var slotId int
			server.Raise(tx.QueryRow(ctx, `SELECT slot_id FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&slotId))
			var sourceJobId server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT job_id FROM competition_job WHERE job_id=$1 AND round_id=$2 FOR UPDATE`, preparation.JobId, preparation.RoundId).Scan(&sourceJobId))
			reserved, err := readRoundBaselinePreparationEvent(ctx, tx, preparation.RoundId, settings.roundBaselinePreparation.Settings.MaximumRecordBytes)
			server.Raise(err)
			if reserved.Owner != preparation.Owner || reserved.ContextSha256 != preparation.ContextSha256 {
				panic(ErrConflict)
			}
			if hold.Stage == "" || len(hold.Stage) > 64 || hold.ArtifactArchiveRetained != sha256Pattern.MatchString(hold.ArtifactManifestSha256) {
				panic(ErrConflict)
			}
			var alreadyHeld bool
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job_event WHERE job_id=$1 AND event_type='round_baseline_preparation_held' AND payload_json->>'owner'=$2)`, preparation.JobId, preparation.Owner).Scan(&alreadyHeld))
			if alreadyHeld {
				return
			}
			appendEvent(ctx, tx, preparation.JobId, self.nowUtc(), "round_baseline_preparation_held", preparation.Owner,
				map[string]any{"schema": 1, "owner": preparation.Owner, "context_sha256": preparation.ContextSha256, "hold": hold})
		}, server.OptReadWrite())
	})
}

// Publication starts the real job exactly once. The existing foreign-key and
// running-attempt trigger see this actual claim inside the same transaction.
func (self PostgresStore) ClaimPreparedRoundBaseline(ctx context.Context, settings *Settings, preparation *roundBaselinePreparation, prepared *preparedRoundBaseline) (job *queuedJob, resultErr error) {
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			job = nil
			source, frozen, err := self.checkRoundBaselinePreparationInTx(ctx, tx, settings, preparation)
			server.Raise(err)
			if err := validatePreparedRoundBaseline(prepared, preparation, frozen); err != nil {
				panic(err)
			}
			now := self.nowUtc()
			// Reservation is the selection's linearization point, just as Claim
			// is for a warm round. A slow admission may commit an older timestamp
			// afterward; it stays queued rather than invalidating owned controls.
			leaseUntil := now.Add(time.Duration(settings.WorkerLeaseSeconds) * time.Second)
			jobUpdate, updateErr := tx.Exec(ctx, `UPDATE competition_job SET state='running',started_at=$2,
				lease_owner=$3,lease_expires_at=$4,attempt_count=1,worker_image_digest=$5
 WHERE job_id=$1 AND state='queued' AND started_at IS NULL AND attempt_count=0`, source.JobId, now, preparation.WorkerId, leaseUntil, preparation.WorkerImage)
			server.Raise(updateErr)
			if jobUpdate.RowsAffected() != 1 {
				panic(ErrLeaseLost)
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round_baseline
 (round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,source_artifact_manifest_sha256,
 base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at)
 VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,$9,$10,$11,$12)`, source.RoundId, prepared.BaselineJson, prepared.BaselineSha256,
				source.JobId, prepared.ArtifactManifestSha256, frozen.BaseSha, source.Round.ProvidersSha256, frozen.EvaluatorImageDigest, ScorerVersion,
				frozen.EvaluationPolicy.HardwareId, frozen.EvaluationPolicy.HostQualificationSha256, now))
			baseline := &roundBaselineRecord{RoundId: source.RoundId, Json: prepared.BaselineJson, Sha256: prepared.BaselineSha256,
				SourceJobId: source.JobId, SourceAttempt: 1, SourceArtifactManifestSha256: prepared.ArtifactManifestSha256,
				BaseSha: frozen.BaseSha, ProvidersSha256: source.Round.ProvidersSha256, EvaluatorImageDigest: frozen.EvaluatorImageDigest,
				ScorerVersion: ScorerVersion, HardwareId: frozen.EvaluationPolicy.HardwareId, HostQualificationSha256: frozen.EvaluationPolicy.HostQualificationSha256, CreatedAt: now}
			server.Raise(validateRoundBaselineRecord(frozen, &source.Round, baseline))
			appendEvent(ctx, tx, source.JobId, now, "round_baseline_preparation_published", preparation.Owner,
				map[string]any{"schema": 1, "owner": preparation.Owner, "baseline_sha256": prepared.BaselineSha256, "artifact_manifest_sha256": prepared.ArtifactManifestSha256})
			appendEvent(ctx, tx, source.JobId, now, "claimed", preparation.WorkerId, map[string]any{"attempt": 1, "worker_image_digest": preparation.WorkerImage})
			slotUpdate, updateErr := tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=$5,job_id=$2,lease_expires_at=$3,heartbeat_at=$4 WHERE slot_id=1 AND worker_id=$1 AND job_id IS NULL`, preparation.Owner, source.JobId, leaseUntil, now, preparation.WorkerId)
			server.Raise(updateErr)
			if slotUpdate.RowsAffected() != 1 {
				panic(ErrLeaseLost)
			}
			source.State, source.StartedAt, source.AttemptCount, source.LeaseOwner, source.LeaseExpiresAt = "running", &now, 1, preparation.WorkerId, &leaseUntil
			source.RoundBaseline = baseline
			job = source
		}, server.OptReadWrite())
	})
	return job, resultErr
}

// A successful transfer has non-null job_id, so this cannot release the real
// candidate's slot. An obsolete owner also cannot erase a later reservation.
func (self PostgresStore) ReleaseRoundBaselinePreparation(ctx context.Context, preparation *roundBaselinePreparation) error {
	return releaseCompetitionQualificationSlot(ctx, preparation.Owner)
}
