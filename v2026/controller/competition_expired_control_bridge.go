// An exact expired legacy run may release stale database custody under its
// held original role. This path cannot fetch a score, start work or admit a job.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
)

// A closed protocol name never denotes general existing-run admission.
const competitionExpiredControlBridgeKind = "expired-executor-control-bridge/v1"

// Only protected-record cycle selects a closed receipt column. Cycle zero
// does not bind a later additive schema; no branch materializes a whole row.
// Literal null positions retain the ordinary scanner without fetching bodies.
func competitionExpiredControlJobSelect(cycle int) (string, error) {
	receipt := "NULL::jsonb"
	switch cycle {
	case 0:
	case 1:
		receipt = "(SELECT receipt_json FROM competition_job_reevaluation recovery WHERE recovery.job_id=j.job_id)"
	case 2:
		receipt = "j.second_recovery_receipt_json"
	default:
		return "", ErrConflict
	}
	return `SELECT j.job_id,j.round_id,j.patch_sha256,j.state,j.submitted_at,
 j.started_at,j.completed_at,j.cache_key,NULL::jsonb,NULL::jsonb,NULL::bytea,
 j.attempt_count,COALESCE(j.lease_owner,''),j.lease_expires_at,j.api_image_digest,COALESCE(j.worker_image_digest,''),
 j.evaluation_cycle,j.cycle_attempt_offset,j.cycle_started_at,` + receipt + `,
 COALESCE(j.executor_host_id,''),COALESCE(j.executor_slot_id,0),COALESCE(j.executor_fencing_token,0),j.execution_deadline_at,
 r.competition_id,r.workload_commitment,NULL::bytea,NULL::bytea,r.providers_sha256,r.providers_path,
 r.policy_json,r.opens_at,r.closes_at,r.reveal_at,r.created_at,r.canceled,r.epoch_number,r.staging,
 r.finalized_at,r.winner_job_id,r.admission_closed_at,r.automation_sealed_at,r.automation_publication_at
 FROM competition_job j JOIN competition_round r ON r.round_id=j.round_id`, nil
}

// This refuses unexpected bodies rather than clearing them after a fetch.
// The original cycle receipt digest and fixed runtime checks remain enabled.
func scanCompetitionExpiredControlJob(row pgx.Row, cycle int, now time.Time) (*queuedJob, error) {
	job, err := scanJob(row, true, now)
	if err != nil {
		return nil, err
	}
	if job.EvaluationCycle != cycle || len(job.Patch) != 0 || job.Score != nil || job.EvalError != nil || len(job.Round.SeedNonce) != 0 || len(job.Round.SeedCiphertext) != 0 {
		return nil, ErrLeaseLost
	}
	return job, nil
}

// Only the protected coordinator role can select this exact root-owned record.
type competitionExpiredControlBridgeConnection struct {
	RecordPath   string `json:"record_path"`
	RecordSha256 string `json:"record_sha256"`
}

// Public metadata binds one old execution; no request, seed or artifact body is
// needed to authenticate its original signed status and immutable elapsed clock.
type competitionExpiredControlBridgeRecord struct {
	Schema                 int                       `json:"schema"`
	Kind                   string                    `json:"kind"`
	Environment            string                    `json:"environment"`
	CompetitionId          string                    `json:"competition_id"`
	SeasonId               string                    `json:"season_id"`
	ApprovalSha256         string                    `json:"approval_sha256"`
	Identity               competitionagent.Identity `json:"identity"`
	SlotId                 int                       `json:"slot_id"`
	SlotFencingToken       int64                     `json:"slot_fencing_token"`
	LeaseOwner             string                    `json:"lease_owner"`
	LeaseExpiresAt         time.Time                 `json:"lease_expires_at"`
	WorkerImageDigest      string                    `json:"worker_image_digest"`
	EvaluationHoldRevision int                       `json:"evaluation_hold_revision"`
	AgentSha256            string                    `json:"agent_sha256"`
	AgentConfigSha256      string                    `json:"agent_config_sha256"`
	QualificationSha256    string                    `json:"qualification_sha256"`
	SourceLockSha256       string                    `json:"source_lock_sha256"`
	RoundBaselineSha256    string                    `json:"round_baseline_sha256"`
	EvaluatorImageDigest   string                    `json:"evaluator_image_digest"`
	ManifestSha256         string                    `json:"manifest_sha256"`
	RequestSha256          string                    `json:"request_sha256"`
}

// Construction requires one fresh ordinary-primary RPC. Consumption and every
// locked callback retain the same root record, old lease and supervisor fence.
type competitionExpiredControlWitness struct {
	consumed          atomic.Bool
	evaluator         *competitionAgentEvaluator
	record            competitionExpiredControlBridgeRecord
	recordPath        string
	recordSha256      string
	response          competitionagent.Response
	jobId             server.Id
	roundId           server.Id
	observedAt        time.Time
	expiresAt         time.Time
	rpcStartedAt      time.Time
	rpcReceivedAt     time.Time
	rpcTimeout        time.Duration
	witnessMaximumAge time.Duration
	supervisorOwner   string
	supervisorFence   int64
}

// Ordinary and mixed-generation observer transports remain entirely separate.
func competitionExpiredControlConnection(settings *Settings) (competitionExecutorConnection, bool) {
	if settings == nil || settings.distributedCoordinator == nil || settings.executorHostId == "" {
		return competitionExecutorConnection{}, false
	}
	for _, connection := range settings.distributedCoordinator.Config.Hosts {
		if connection.HostId == settings.executorHostId && connection.ExpiredControlBridge != nil {
			return connection, true
		}
	}
	return competitionExecutorConnection{}, false
}

// Root custody, canonical bytes and the whole-record digest precede decoding
// authority. The optional final newline does not permit duplicate JSON members.
func readCompetitionExpiredControlRecord(connection competitionExecutorConnection, limits competitionagent.Limits) (competitionExpiredControlBridgeRecord, error) {
	var record competitionExpiredControlBridgeRecord
	value := connection.ExpiredControlBridge
	if value == nil || !sha256Pattern.MatchString(value.RecordSha256) || limits.DocumentBytes <= 0 {
		return record, ErrConflict
	}
	if err := competitionQualificationRootPath(value.RecordPath, false); err != nil {
		return record, errors.New("expired bridge record lacks protected root custody")
	}
	info, err := os.Lstat(value.RecordPath)
	if err != nil {
		return record, errors.New("expired bridge record is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || stat.Gid != 0 || stat.Nlink != 1 || info.Mode().Perm() != 0400 {
		return record, errors.New("expired bridge record is not an immutable private leaf")
	}
	content, err := readCompetitionPreparationFile(value.RecordPath, limits.DocumentBytes, true)
	if err != nil {
		return record, errors.New("expired bridge record could not be authenticated")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != value.RecordSha256 || decodeFailureEvidenceJson(content, &record) != nil {
		return record, errors.New("expired bridge record differs from its exact approved bytes")
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(content, canonical) && !bytes.Equal(content, append(canonical, '\n')) {
		return record, errors.New("expired bridge record is not one canonical closed document")
	}
	after, err := os.Lstat(value.RecordPath)
	if err != nil || !os.SameFile(info, after) || info.Mode() != after.Mode() || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return record, errors.New("expired bridge record changed during authentication")
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok || afterStat.Nlink != 1 || afterStat.Uid != 0 || afterStat.Gid != 0 || !statChangeTimeEqual(afterStat, stat) {
		return record, errors.New("expired bridge record custody changed during authentication")
	}
	return record, nil
}

// An ordinary endpoint remains exact. Provisioning this bridge grants neither
// an ExistingControl role nor new transport, source, image or host identities.
func validateCompetitionExpiredControlBridgeConnection(connection competitionExecutorConnection, host CompetitionExecutorHostPolicy, settings competitionDistributedCoordinatorSettings) error {
	if connection.ExpiredControlBridge == nil {
		return nil
	}
	if connection.ExistingControl != nil || settings.ExistingWitnessMaximumAge <= 0 || settings.ExistingWitnessMaximumAge > settings.RequestTimeout {
		return errors.New("expired bridge cannot inherit a mixed observer or unbounded witness")
	}
	record, err := readCompetitionExpiredControlRecord(connection, settings.Limits)
	if err != nil {
		return err
	}
	return validateCompetitionExpiredControlRecord(record, connection, host, settings.Limits)
}

// These are all required scalar identities; temporal permission is checked
// from the occupied database snapshot and again after its final row locks.
func validateCompetitionExpiredControlRecord(record competitionExpiredControlBridgeRecord, connection competitionExecutorConnection, host CompetitionExecutorHostPolicy, limits competitionagent.Limits) error {
	jobId, jobErr := server.ParseId(record.Identity.JobId)
	roundId, roundErr := server.ParseId(record.Identity.RoundId)
	if record.Schema != 1 || record.Kind != competitionExpiredControlBridgeKind || record.Identity.Validate(limits) != nil || jobErr != nil || roundErr != nil ||
		jobId.String() != record.Identity.JobId || roundId.String() != record.Identity.RoundId || record.Identity.HostId != host.HostId || connection.HostId != host.HostId ||
		record.SlotId != host.SlotId || record.SlotFencingToken <= 0 || !workerIdPattern.MatchString(record.LeaseOwner) || record.LeaseExpiresAt.IsZero() || record.EvaluationHoldRevision <= 0 ||
		record.Environment == "" || record.CompetitionId == "" || record.SeasonId == "" || !imageDigestPattern.MatchString(record.WorkerImageDigest) || !imageDigestPattern.MatchString(record.EvaluatorImageDigest) ||
		record.AgentSha256 != connection.AgentCommandSha256 || record.AgentConfigSha256 != connection.AgentConfigSha256 {
		return errors.New("expired bridge record does not bind one exact old execution role")
	}
	for _, digest := range []string{record.ApprovalSha256, record.AgentSha256, record.AgentConfigSha256, record.QualificationSha256, record.SourceLockSha256, record.RoundBaselineSha256, record.ManifestSha256, record.RequestSha256} {
		if !sha256Pattern.MatchString(digest) {
			return errors.New("expired bridge omitted a complete approved identity")
		}
	}
	return nil
}

// A witness never selects FIFO. Its root record and current occupied row must
// already name the same immutable run, old lease and elapsed original deadline.
func competitionExpiredControlRecordMatches(settings *Settings, job *queuedJob, record competitionExpiredControlBridgeRecord, now time.Time) bool {
	if settings == nil || settings.automationLease == nil || settings.automationLease.OwnerId == "" || settings.automationLease.FencingToken <= 0 || job == nil || !sameCompetitionExistingJob(job, record.Identity) || job.RoundBaseline == nil ||
		settings.automationRoundId.String() != record.Identity.RoundId || settings.executorHostId != record.Identity.HostId || settings.executorSlotId != record.SlotId ||
		settings.AutomationPolicy.Environment != record.Environment || settings.CompetitionId != record.CompetitionId || settings.AutomationPolicy.SeasonId != record.SeasonId ||
		job.SlotId != record.SlotId || job.SlotFencingToken != record.SlotFencingToken || job.LeaseOwner != record.LeaseOwner || job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.Equal(record.LeaseExpiresAt) ||
		job.WorkerImageDigest != record.WorkerImageDigest || now.Before(record.Identity.DeadlineAt) || now.Before(record.LeaseExpiresAt) ||
		job.RoundBaseline.Sha256 != record.RoundBaselineSha256 || settings.AutomationPolicy.SourceLockSha256 != record.SourceLockSha256 || competitionExecutionImageForJob(settings, job) != record.EvaluatorImageDigest {
		return false
	}
	return competitionExecutorQualificationSha256(settings) == record.QualificationSha256
}

// The exact source-pinned primary status path reports unknown while launched
// cleanup is pending. A terminal response is NOT a new host admission proof.
func (self *competitionAgentEvaluator) observeExpiredControl(ctx context.Context, settings *Settings, job *queuedJob, record competitionExpiredControlBridgeRecord, recordSha256 string) (*competitionExpiredControlWitness, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	connection, present := competitionExpiredControlConnection(settings)
	started := self.existingNow()
	if !present || connection.ExistingControl != nil || connection.ExpiredControlBridge.RecordSha256 != recordSha256 ||
		validateCompetitionExpiredControlRecord(record, connection, self.host, settings.distributedCoordinator.Config.Settings.Limits) != nil ||
		!competitionExpiredControlRecordMatches(settings, job, record, started) {
		return nil, ErrLeaseLost
	}
	maximumAge := settings.distributedCoordinator.Config.Settings.ExistingWitnessMaximumAge
	rpcTimeout := settings.distributedCoordinator.Config.Settings.RequestTimeout
	if maximumAge <= 0 || rpcTimeout <= 0 {
		return nil, ErrLeaseLost
	}
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	response, err := self.exchangeRun(rpcCtx, competitionagent.Request{Operation: "status", RunId: record.Identity.RunId, FencingToken: record.SlotFencingToken}, "", "", false)
	if err != nil {
		return nil, err
	}
	if err := rpcCtx.Err(); err != nil {
		return nil, err
	}
	received := self.existingNow()
	if received.Before(started) || received.Sub(started) > rpcTimeout {
		return nil, ErrLeaseLost
	}
	key, keyErr := base64.StdEncoding.DecodeString(self.host.AgentPublicKeyBase64)
	if keyErr != nil || len(key) != ed25519.PublicKeySize || competitionagent.VerifyResponse(response, key) != nil || response.Schema != competitionagent.Schema ||
		response.HostId != record.Identity.HostId || response.RunId != record.Identity.RunId || response.FencingToken != record.SlotFencingToken ||
		!sameCompetitionAgentIdentity(response.Identity, &record.Identity) || response.ManifestSha256 != record.ManifestSha256 || response.RequestSha256 != record.RequestSha256 ||
		response.ExistingRun != nil || len(response.SelfCheck) != 0 || !terminalCompetitionExpiredControlState(response.State) {
		return nil, errors.Join(competitionagent.ErrIntegrity, errors.New("expired bridge lacks its exact old signed terminal status"))
	}
	// This is an already-terminal, elapsed run's metadata-completion window,
	// not a fresh native admission or score-observation lease. Preserve the full
	// independently bounded old RPC, then start its short receipt-only CAS age.
	expires := received.Add(maximumAge)
	if !self.existingNow().Before(expires) {
		return nil, ErrLeaseLost
	}
	jobId, _ := server.ParseId(record.Identity.JobId)
	roundId, _ := server.ParseId(record.Identity.RoundId)
	return &competitionExpiredControlWitness{evaluator: self, record: record, recordPath: connection.ExpiredControlBridge.RecordPath, recordSha256: recordSha256, response: response, jobId: jobId, roundId: roundId,
		observedAt: received, expiresAt: expires, rpcStartedAt: started, rpcReceivedAt: received,
		rpcTimeout: rpcTimeout, witnessMaximumAge: maximumAge,
		supervisorOwner: settings.automationLease.OwnerId, supervisorFence: settings.automationLease.FencingToken}, nil
}

// Success here discards even a genuinely successful late native score.
func terminalCompetitionExpiredControlState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "canceled" || state == "expired"
}

// Reuse is rejected before SQL; transaction retries can only recheck an already
// consumed witness under its unchanged database and supervisor authorities.
func (self *competitionExpiredControlWitness) consume(settings *Settings) bool {
	return self.roleMatches(settings) && self.consumed.CompareAndSwap(false, true)
}

// The same exact configured role remains required after every row/fence wait.
func (self *competitionExpiredControlWitness) roleMatches(settings *Settings) bool {
	connection, present := competitionExpiredControlConnection(settings)
	return self != nil && self.evaluator != nil && present && connection.ExistingControl == nil && settings.automationLease != nil &&
		settings.automationRoundId == self.roundId && settings.executorHostId == self.record.Identity.HostId && settings.executorSlotId == self.record.SlotId &&
		connection.ExpiredControlBridge.RecordPath == self.recordPath && connection.ExpiredControlBridge.RecordSha256 == self.recordSha256 && connection.AgentCommandSha256 == self.record.AgentSha256 && connection.AgentConfigSha256 == self.record.AgentConfigSha256 &&
		settings.automationLease.OwnerId == self.supervisorOwner && settings.automationLease.FencingToken == self.supervisorFence
}

// Every pointer is the original occupied row, never an empty or newly queued slot.
func (self *competitionExpiredControlWitness) slotMatches(hostId string, job *server.Id, worker *string, lease *time.Time, fence int64) bool {
	return self != nil && hostId == self.record.Identity.HostId && job != nil && *job == self.jobId && worker != nil && *worker == self.record.LeaseOwner &&
		lease != nil && lease.Equal(self.record.LeaseExpiresAt) && fence == self.record.SlotFencingToken
}

// Freshness and immutable row identity are rechecked after all lock waits.
func (self *competitionExpiredControlWitness) matches(settings *Settings, job *queuedJob, now time.Time) bool {
	return self.roleMatches(settings) && self.consumed.Load() &&
		!now.Before(self.observedAt) && now.Before(self.expiresAt) && competitionExpiredControlRecordMatches(settings, job, self.record, now)
}

// Scope locking is caller-owned; this read never acquires another connection or
// mistakes the finalization/reward head for the exact current evaluation hold.
func (self *competitionExpiredControlWitness) requireHeldInTx(ctx context.Context, tx server.PgTx, settings *Settings) error {
	if self == nil || settings == nil || settings.AutomationPolicy.Environment != self.record.Environment || settings.CompetitionId != self.record.CompetitionId || settings.AutomationPolicy.SeasonId != self.record.SeasonId {
		return ErrLeaseLost
	}
	var active bool
	var revision int
	err := tx.QueryRow(ctx, `SELECT active,revision FROM competition_automation_hold
 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND kind='evaluation' ORDER BY revision DESC LIMIT 1`,
		self.record.Environment, self.record.CompetitionId, self.record.SeasonId).Scan(&active, &revision)
	if err != nil {
		return err
	}
	if !competitionExpiredControlHoldMatches(self.record, settings, active, revision) {
		return ErrLeaseLost
	}
	return nil
}

// A different kind/head, released hold or later active revision is not the
// root-pinned claim-only boundary under which the observation was approved.
func competitionExpiredControlHoldMatches(record competitionExpiredControlBridgeRecord, settings *Settings, active bool, revision int) bool {
	return settings != nil && record.EvaluationHoldRevision > 0 && active && revision == record.EvaluationHoldRevision &&
		settings.AutomationPolicy.Environment == record.Environment && settings.CompetitionId == record.CompetitionId && settings.AutomationPolicy.SeasonId == record.SeasonId
}

// The snapshot deliberately selects metadata only: no patch, seed, result,
// score, raw evaluator request or artifact body is fetched for this operation.
func (self PostgresStore) expiredCompetitionJob(ctx context.Context, settings *Settings) (job *queuedJob, resultErr error) {
	connection, present := competitionExpiredControlConnection(settings)
	if !present || settings.automationLease == nil || !competitionDistributedEnabled(settings) {
		return nil, ErrLeaseLost
	}
	record, err := readCompetitionExpiredControlRecord(connection, settings.distributedCoordinator.Config.Settings.Limits)
	if err != nil {
		return nil, err
	}
	found := false
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		if host.HostId == settings.executorHostId && host.SlotId == settings.executorSlotId {
			if err := validateCompetitionExpiredControlRecord(record, connection, host, settings.distributedCoordinator.Config.Settings.Limits); err != nil {
				return nil, err
			}
			found = true
			break
		}
	}
	if !found {
		return nil, ErrLeaseLost
	}
	jobId, err := server.ParseId(record.Identity.JobId)
	if err != nil {
		return nil, ErrConflict
	}
	held := &competitionExpiredControlWitness{record: record}
	query, err := competitionExpiredControlJobSelect(record.Identity.Cycle)
	if err != nil {
		return nil, err
	}
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			job = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			server.Raise(held.requireHeldInTx(ctx, tx, settings))
			value, err := scanCompetitionExpiredControlJob(tx.QueryRow(ctx, query+`
 WHERE j.job_id=$1 AND j.round_id=$2 AND j.state='running' AND j.executor_host_id=$3 AND j.executor_slot_id=$4
 AND j.evaluation_cycle=$5
 AND EXISTS(SELECT 1 FROM competition_execution_slot s WHERE s.slot_id=j.executor_slot_id AND s.host_id=j.executor_host_id
 AND s.job_id=j.job_id AND s.round_id=j.round_id AND s.worker_id=j.lease_owner AND s.fencing_token=j.executor_fencing_token
 AND s.lease_expires_at=j.lease_expires_at)`, jobId, settings.automationRoundId, settings.executorHostId, settings.executorSlotId, record.Identity.Cycle), record.Identity.Cycle, self.nowUtc())
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			server.Raise(err)
			_, err = settingsForFrozenRound(settings, &value.Round)
			server.Raise(err)
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, value.RoundId))
			baseline, err := scanRoundBaseline(tx.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,
 source_artifact_manifest_sha256,base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at
 FROM competition_round_baseline WHERE round_id=$1`, value.RoundId))
			server.Raise(err)
			server.Raise(validateRoundBaselineRecord(settings, &value.Round, baseline))
			value.RoundBaseline = baseline
			server.Raise(validateCompetitionSecurityToolchainScope(settings, &value.Round, baseline))
			server.Raise(requireCompetitionSecurityToolchainDecisionInTx(ctx, tx, settings, baseline))
			if !competitionExpiredControlRecordMatches(settings, value, record, self.nowUtc()) {
				panic(ErrLeaseLost)
			}
			job = value
		})
	})
	if resultErr != nil {
		return nil, resultErr
	}
	return job, nil
}

// Configured bridges cannot fall through to ordinary admission, even after
// their one occupied row was completed. Removal needs a new protected role.
func (self *Worker) reconcileExpiredControl(ctx context.Context, evaluator *competitionAgentEvaluator) (bool, error) {
	connection, present := competitionExpiredControlConnection(self.settings)
	if !present {
		return false, nil
	}
	store, supported := self.store.(interface {
		expiredCompetitionJob(context.Context, *Settings) (*queuedJob, error)
		expiredCompetitionJobCompleted(context.Context, *Settings, competitionExpiredControlBridgeRecord, string) (bool, error)
	})
	if !supported || evaluator == nil || self.settings.executorExistingWitness != nil || self.settings.executorExpiredControlWitness != nil {
		return true, ErrLeaseLost
	}
	record, err := readCompetitionExpiredControlRecord(connection, self.settings.distributedCoordinator.Config.Settings.Limits)
	if err != nil {
		return true, err
	}
	// This independently checks the current hold and live parent. A previous
	// atomic completion remains acknowledgeable after a restart or lease expiry.
	completed, err := store.expiredCompetitionJobCompleted(ctx, self.settings, record, connection.ExpiredControlBridge.RecordSha256)
	if err != nil {
		return true, err
	}
	if completed {
		return true, ErrAutomationHeld
	}
	job, err := store.expiredCompetitionJob(ctx, self.settings)
	if err != nil {
		return true, err
	}
	if job == nil {
		return true, ErrAutomationHeld
	}
	witness, err := evaluator.observeExpiredControl(ctx, self.settings, job, record, connection.ExpiredControlBridge.RecordSha256)
	if err != nil {
		return true, err
	}
	// A changed or replaced approval cannot survive the external observation.
	if _, err := readCompetitionExpiredControlRecord(connection, self.settings.distributedCoordinator.Config.Settings.Limits); err != nil {
		return true, err
	}
	selected := *self.settings
	selected.executorReconcileOnly, selected.executorReconcileExpiredOnly, selected.executorFreshOnly = true, true, false
	selected.executorExpiredControlWitness = witness
	claimed, err := self.store.Claim(ctx, &selected, self.workerId, self.workerImageDigest)
	if err != nil {
		// Claim never commits a replacement lease without its fixed failure.
		// An uncertain acknowledgement is read back, never retried as a claim.
		completed, readErr := store.expiredCompetitionJobCompleted(ctx, self.settings, record, connection.ExpiredControlBridge.RecordSha256)
		if readErr == nil && completed {
			return true, ErrAutomationHeld
		}
		return true, errors.Join(err, readErr)
	}
	if claimed == nil || !witness.consumed.Load() || claimed.SlotFencingToken != record.SlotFencingToken+1 ||
		!competitionExpiredControlImmutableMatches(self.settings, claimed, record) || claimed.State != "failed" || claimed.LeaseOwner != "" || claimed.LeaseExpiresAt != nil {
		return true, ErrLeaseLost
	}
	completed, err = store.expiredCompetitionJobCompleted(ctx, self.settings, record, connection.ExpiredControlBridge.RecordSha256)
	if err != nil || !completed {
		return true, errors.Join(ErrLeaseLost, err)
	}
	return true, nil
}
