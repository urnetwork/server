// Append-only admission clocks and retained no-op receipts share the current
// coordinator's SQL fence. No connection is retained across agent or MinIO I/O.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Runtime images are pinned beside the clock so a main rollout cannot silently
// change an already-dispatched qualification bundle on reconnection.
type competitionExecutorQualificationRun struct {
	Recovery          *competitionExecutorQualificationRecoveryRecord
	JobId             server.Id
	StartedAt         time.Time
	DeadlineAt        time.Time
	ApiImageDigest    string
	WorkerImageDigest string
}

// Serializes only initial clock publication. Repeated calls return its original
// database microsecond instants, image identities and fixed three-hour deadline.
func (self PostgresStore) reserveDistributedExecutorQualification(ctx context.Context, settings *Settings, roundId server.Id, hostId string, jobId server.Id) (run *competitionExecutorQualificationRun, resultErr error) {
	if settings.automationLease == nil || settings.automationRoundId != roundId {
		return nil, ErrLeaseLost
	}
	workerImage, err := runtimeImageDigest()
	if err != nil {
		return nil, err
	}
	intent, err := self.SourceActivation(ctx, settings, settings.AutomationPolicy.SourceActivationGeneration)
	if err != nil {
		return nil, err
	}
	receipts, err := self.sourcePreparationReceipts(ctx, settings, intent.Generation)
	if err != nil {
		return nil, err
	}
	apiImage, err := competitionSourceApiRuntimeIdentity(intent, "", receipts)
	if err != nil {
		return nil, err
	}
	round, err := self.GetRound(ctx, settings, roundId)
	if err != nil {
		return nil, err
	}
	recovery, err := self.loadExecutorQualificationRecovery(ctx, settings, round, hostId)
	if err != nil {
		return nil, err
	}
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			run = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "evaluation"))
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_executor_control_amendment',$1,true)`, competitionExecutorControlAmendmentSha256(settings)))
			now := self.nowUtc().Truncate(time.Microsecond)
			deadline := now.Add(time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
			if recovery == nil {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_executor_qualification_run
 (round_id,host_id,job_id,started_at,deadline_at,api_image_digest,worker_image_digest) VALUES($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT(round_id,host_id) DO NOTHING`, roundId, hostId, jobId, now, deadline, apiImage, workerImage))
			}
			stored := &competitionExecutorQualificationRun{Recovery: recovery}
			server.Raise(tx.QueryRow(ctx, `SELECT job_id,started_at,deadline_at,api_image_digest,worker_image_digest
 FROM competition_executor_qualification_clock($1,$2)`, roundId, hostId).
				Scan(&stored.JobId, &stored.StartedAt, &stored.DeadlineAt, &stored.ApiImageDigest, &stored.WorkerImageDigest))
			expectedJobId := jobId
			if recovery != nil {
				expectedJobId = recovery.Authorization.ReplacementJobId
			}
			if stored.JobId != expectedJobId || !stored.DeadlineAt.Equal(stored.StartedAt.Add(time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second)) ||
				!imageDigestPattern.MatchString(stored.ApiImageDigest) || !imageDigestPattern.MatchString(stored.WorkerImageDigest) {
				panic(ErrConflict)
			}
			run = stored
		}, server.OptReadWrite())
	})
	if resultErr != nil {
		return nil, resultErr
	}
	return run, nil
}

// Reading an existing passing receipt also authenticates its retained exact
// object version. A corrupt/unavailable archive cannot be replaced by resampling.
func (self PostgresStore) distributedExecutorQualified(ctx context.Context, settings *Settings, round *roundRecord, baseline *roundBaselineRecord, host CompetitionExecutorHostPolicy) (bool, error) {
	var content []byte
	var expected string
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			loadErr := conn.QueryRow(ctx, `SELECT receipt_json,receipt_sha256 FROM competition_executor_equivalence WHERE round_id=$1 AND host_id=$2`, round.RoundId, host.HostId).Scan(&content, &expected)
			if errors.Is(loadErr, pgx.ErrNoRows) {
				return
			}
			server.Raise(loadErr)
		})
	})
	if err != nil || len(content) == 0 {
		return false, err
	}
	receipt := &competitionExecutorEquivalenceReceipt{}
	if err := decodeFailureEvidenceJson(content, receipt); err != nil {
		return false, err
	}
	selected := *settings
	selected.executorQualificationRecovery = nil
	if receipt.RecoverySha256 != "" {
		selected.executorQualificationRecovery, err = self.loadExecutorQualificationRecovery(ctx, settings, round, host.HostId)
		if err != nil || selected.executorQualificationRecovery == nil {
			return false, errors.New("stored host receipt has no authenticated recovery authority")
		}
	}
	canonical, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != expected || validateCompetitionExecutorReceipt(&selected, round, baseline, host, receipt) != nil {
		return false, errors.New("stored executor admission evidence failed authentication")
	}
	reader, ok := settings.artifactArchive.(interface {
		VerifyExecutorQualification(context.Context, *retainedArtifact) error
	})
	if !ok {
		return false, errors.New("executor admission archive cannot authenticate retained qualification")
	}
	if err := reader.VerifyExecutorQualification(ctx, receipt.Retention); err != nil {
		return false, err
	}
	return true, nil
}

// A receipt is published only after its exact signed evaluation artifacts and
// uncertainty document have both been retained and authenticated by main.
func (self PostgresStore) recordDistributedExecutorQualification(ctx context.Context, settings *Settings, round *roundRecord, baseline *roundBaselineRecord, host CompetitionExecutorHostPolicy, receipt *competitionExecutorEquivalenceReceipt) error {
	if settings.automationLease == nil || validateCompetitionExecutorReceipt(settings, round, baseline, host, receipt) != nil {
		return ErrConflict
	}
	content, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "evaluation"))
			var startedAt, deadlineAt time.Time
			var jobId server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT job_id,started_at,deadline_at FROM competition_executor_qualification_clock($1,$2)`, round.RoundId, host.HostId).
				Scan(&jobId, &startedAt, &deadlineAt))
			if jobId.String() != receipt.QualificationJobId || !startedAt.Equal(receipt.QualificationStartedAt) || !deadlineAt.Equal(receipt.QualificationDeadlineAt) || !self.nowUtc().Before(deadlineAt) {
				panic(ErrConflict)
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_executor_equivalence(round_id,host_id,receipt_json,receipt_sha256,baseline_sha256,created_at)
 VALUES($1,$2,$3::jsonb,$4,$5,$6) ON CONFLICT(round_id,host_id) DO NOTHING`, round.RoundId, host.HostId, content, hex.EncodeToString(digest[:]), baseline.Sha256, receipt.CreatedAt))
			var stored string
			server.Raise(tx.QueryRow(ctx, `SELECT receipt_sha256 FROM competition_executor_equivalence WHERE round_id=$1 AND host_id=$2`, round.RoundId, host.HostId).Scan(&stored))
			if stored != hex.EncodeToString(digest[:]) {
				panic(ErrConflict)
			}
		}, server.OptReadWrite())
	})
}

// Qualification documents use the same immutable retention implementation and
// post-upload version authentication as public job artifacts, but their ids are
// deliberately absent from public admission/job tables.
func (self *blobArtifactArchive) ArchiveExecutorQualification(ctx context.Context, settings *Settings, job *queuedJob, content []byte) (*retainedArtifact, error) {
	if len(content) == 0 || len(content) > maxEvaluatorResultBytes {
		return nil, errors.New("executor admission document exceeds its bound")
	}
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file, err := os.CreateTemp(settings.ArtifactRoot, ".executor-qualification-")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return nil, err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return nil, err
	}
	key := filepath.ToSlash(filepath.Join(self.attemptPrefix(settings, job), "objects", sha, "executor-qualification.json"))
	return self.retainFile(ctx, settings, "executor-qualification.json", key, path, sha, int64(len(content)), "application/json")
}

// Re-read the exact retained version without exposing any blob credential to an
// agent. This is a bounded independent check, not another upload or calibration.
func (self *blobArtifactArchive) VerifyExecutorQualification(ctx context.Context, retained *retainedArtifact) error {
	if retained == nil || retained.Mode != "COMPLIANCE" || retained.Bytes <= 0 || retained.Bytes > maxEvaluatorResultBytes || retained.Key == "" || !sha256Pattern.MatchString(retained.Sha256) {
		return errors.New("invalid retained executor qualification identity")
	}
	reader, err := self.store.GetVersion(ctx, retained.Key, retained.VersionId)
	if err != nil {
		return err
	}
	digest := sha256.New()
	count, readErr := io.Copy(digest, io.LimitReader(reader, retained.Bytes+1))
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil || count != retained.Bytes || hex.EncodeToString(digest.Sum(nil)) != retained.Sha256 {
		return errors.New("retained executor qualification version failed authentication")
	}
	return nil
}
