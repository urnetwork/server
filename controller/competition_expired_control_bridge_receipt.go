// Append-only reconciliation evidence resolves unknown acknowledgements without
// another claim, a new key/fence, or a late score/artifact adoption path. The
// original claim and fixed expired completion commit in one transaction.
package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// The existing event has one closed extra object; it is not a new admission.
const competitionExpiredControlClaimKind = "expired-control-claimed/v1"

// Every field is immutable public execution metadata or an already-signed old
// response. Neither a seed-bearing request nor an artifact body is retained.
type competitionExpiredControlClaimProof struct {
	Schema                 int                       `json:"schema"`
	Kind                   string                    `json:"kind"`
	RecordSha256           string                    `json:"record_sha256"`
	ApprovalSha256         string                    `json:"approval_sha256"`
	Identity               competitionagent.Identity `json:"identity"`
	OldFence               int64                     `json:"old_fence"`
	NewFence               int64                     `json:"new_fence"`
	OldLeaseOwner          string                    `json:"old_lease_owner"`
	OldLeaseExpiresAt      time.Time                 `json:"old_lease_expires_at"`
	NewLeaseOwner          string                    `json:"new_lease_owner"`
	NewLeaseExpiresAt      time.Time                 `json:"new_lease_expires_at"`
	EvaluationHoldRevision int                       `json:"evaluation_hold_revision"`
	SupervisorOwner        string                    `json:"supervisor_owner"`
	SupervisorFence        int64                     `json:"supervisor_fence"`
	RpcStartedAt           time.Time                 `json:"rpc_started_at"`
	RpcReceivedAt          time.Time                 `json:"rpc_received_at"`
	RpcTimeout             time.Duration             `json:"rpc_timeout"`
	WitnessMaximumAge      time.Duration             `json:"witness_maximum_age"`
	WitnessExpiresAt       time.Time                 `json:"witness_expires_at"`
	ManifestSha256         string                    `json:"manifest_sha256"`
	RequestSha256          string                    `json:"request_sha256"`
	Status                 competitionagent.Response `json:"status"`
}

// JSONB may reorder objects. Reconstruct the exact typed Go map representation
// used by appendEvent rather than hashing PostgreSQL's differently formatted text.
func (self competitionExpiredControlClaimProof) payload() map[string]any {
	return map[string]any{"schema": self.Schema, "kind": self.Kind, "record_sha256": self.RecordSha256, "approval_sha256": self.ApprovalSha256,
		"identity": self.Identity, "old_fence": self.OldFence, "new_fence": self.NewFence,
		"old_lease_owner": self.OldLeaseOwner, "old_lease_expires_at": self.OldLeaseExpiresAt,
		"new_lease_owner": self.NewLeaseOwner, "new_lease_expires_at": self.NewLeaseExpiresAt,
		"evaluation_hold_revision": self.EvaluationHoldRevision, "supervisor_owner": self.SupervisorOwner, "supervisor_fence": self.SupervisorFence,
		"rpc_started_at": self.RpcStartedAt, "rpc_received_at": self.RpcReceivedAt, "witness_expires_at": self.WitnessExpiresAt,
		"rpc_timeout": self.RpcTimeout, "witness_maximum_age": self.WitnessMaximumAge,
		"manifest_sha256": self.ManifestSha256, "request_sha256": self.RequestSha256, "status": self.Status}
}

// The claim callback invokes this only after consuming and rechecking the
// witness. Invalid arguments cannot become a nullable/partial durable proof.
func (self *competitionExpiredControlWitness) eventPayload(newFence int64, newLeaseOwner string, newLeaseExpiresAt time.Time) map[string]any {
	if self == nil || !self.consumed.Load() || newFence <= self.record.SlotFencingToken || newFence-self.record.SlotFencingToken != 1 ||
		!workerIdPattern.MatchString(newLeaseOwner) || !self.rpcReceivedAt.Before(newLeaseExpiresAt) || self.rpcTimeout <= 0 ||
		self.witnessMaximumAge <= 0 || self.witnessMaximumAge > self.rpcTimeout || self.rpcReceivedAt.Before(self.rpcStartedAt) ||
		self.rpcReceivedAt.Sub(self.rpcStartedAt) > self.rpcTimeout || !self.expiresAt.Equal(self.rpcReceivedAt.Add(self.witnessMaximumAge)) {
		return nil
	}
	return (competitionExpiredControlClaimProof{Schema: 1, Kind: competitionExpiredControlClaimKind, RecordSha256: self.recordSha256,
		ApprovalSha256: self.record.ApprovalSha256, Identity: self.record.Identity, OldFence: self.record.SlotFencingToken, NewFence: newFence,
		OldLeaseOwner: self.record.LeaseOwner, OldLeaseExpiresAt: self.record.LeaseExpiresAt, NewLeaseOwner: newLeaseOwner, NewLeaseExpiresAt: newLeaseExpiresAt,
		EvaluationHoldRevision: self.record.EvaluationHoldRevision, SupervisorOwner: self.supervisorOwner, SupervisorFence: self.supervisorFence,
		RpcStartedAt: self.rpcStartedAt, RpcReceivedAt: self.rpcReceivedAt, RpcTimeout: self.rpcTimeout, WitnessMaximumAge: self.witnessMaximumAge, WitnessExpiresAt: self.expiresAt,
		ManifestSha256: self.record.ManifestSha256, RequestSha256: self.record.RequestSha256, Status: self.response}).payload()
}

// These are precisely the old six outer event fields plus the bridge object.
type competitionExpiredControlClaimEvent struct {
	Attempt              int                                 `json:"attempt"`
	WorkerImageDigest    string                              `json:"worker_image_digest"`
	ExecutorHostId       string                              `json:"executor_host_id"`
	ExecutorSlotId       int                                 `json:"executor_slot_id"`
	ExecutorFencingToken int64                               `json:"executor_fencing_token"`
	ExecutionDeadlineAt  time.Time                           `json:"execution_deadline_at"`
	ExpiredControlBridge competitionExpiredControlClaimProof `json:"expired_control_bridge"`
}

// Authentication binds both the event's retained hash and the independent old
// host signature before any current row is interpreted as a committed claim.
func validateCompetitionExpiredControlClaimEvent(settings *Settings, record competitionExpiredControlBridgeRecord, recordSha256 string, eventAt time.Time, actorId string, content []byte, expected string) (competitionExpiredControlClaimProof, error) {
	var event competitionExpiredControlClaimEvent
	if decodeFailureEvidenceJson(content, &event) != nil || settings == nil || settings.distributedCoordinator == nil || settings.DistributedEvaluationPolicy == nil ||
		!sha256Pattern.MatchString(recordSha256) || !sha256Pattern.MatchString(expected) {
		return event.ExpiredControlBridge, ErrLeaseLost
	}
	proof := event.ExpiredControlBridge
	canonical, err := json.Marshal(map[string]any{"attempt": event.Attempt, "worker_image_digest": event.WorkerImageDigest, "executor_host_id": event.ExecutorHostId,
		"executor_slot_id": event.ExecutorSlotId, "executor_fencing_token": event.ExecutorFencingToken, "execution_deadline_at": event.ExecutionDeadlineAt,
		"expired_control_bridge": proof.payload()})
	digest := sha256.Sum256(canonical)
	if err != nil || hex.EncodeToString(digest[:]) != expected || proof.Schema != 1 || proof.Kind != competitionExpiredControlClaimKind ||
		proof.RecordSha256 != recordSha256 || proof.ApprovalSha256 != record.ApprovalSha256 || !sameCompetitionAgentIdentity(&proof.Identity, &record.Identity) ||
		proof.OldFence != record.SlotFencingToken || proof.NewFence <= proof.OldFence || proof.NewFence-proof.OldFence != 1 ||
		proof.OldLeaseOwner != record.LeaseOwner || !proof.OldLeaseExpiresAt.Equal(record.LeaseExpiresAt) ||
		!workerIdPattern.MatchString(proof.NewLeaseOwner) || actorId != proof.NewLeaseOwner || !eventAt.Before(proof.NewLeaseExpiresAt) ||
		proof.EvaluationHoldRevision != record.EvaluationHoldRevision || !workerIdPattern.MatchString(proof.SupervisorOwner) || proof.SupervisorFence <= 0 ||
		proof.RpcStartedAt.IsZero() || proof.RpcReceivedAt.Before(proof.RpcStartedAt) || proof.RpcTimeout <= 0 || proof.RpcReceivedAt.Sub(proof.RpcStartedAt) > proof.RpcTimeout ||
		proof.RpcStartedAt.Before(record.Identity.DeadlineAt) || proof.RpcStartedAt.Before(record.LeaseExpiresAt) ||
		proof.WitnessMaximumAge <= 0 || proof.WitnessMaximumAge > proof.RpcTimeout || !proof.WitnessExpiresAt.Equal(proof.RpcReceivedAt.Add(proof.WitnessMaximumAge)) ||
		eventAt.Before(proof.RpcReceivedAt) || !eventAt.Before(proof.WitnessExpiresAt) || eventAt.Before(record.Identity.DeadlineAt) ||
		proof.ManifestSha256 != record.ManifestSha256 || proof.RequestSha256 != record.RequestSha256 ||
		event.Attempt != record.Identity.Attempt || event.WorkerImageDigest != record.WorkerImageDigest || event.ExecutorHostId != record.Identity.HostId ||
		event.ExecutorSlotId != record.SlotId || event.ExecutorFencingToken != proof.NewFence || !event.ExecutionDeadlineAt.Equal(record.Identity.DeadlineAt) {
		return proof, ErrLeaseLost
	}
	var host *CompetitionExecutorHostPolicy
	for index := range settings.DistributedEvaluationPolicy.Hosts {
		if settings.DistributedEvaluationPolicy.Hosts[index].HostId == record.Identity.HostId {
			host = &settings.DistributedEvaluationPolicy.Hosts[index]
		}
	}
	if host == nil {
		return proof, ErrLeaseLost
	}
	key, err := base64.StdEncoding.DecodeString(host.AgentPublicKeyBase64)
	response := proof.Status
	if err != nil || len(key) != ed25519.PublicKeySize || competitionagent.VerifyResponse(response, key) != nil || response.Schema != competitionagent.Schema ||
		response.HostId != record.Identity.HostId || response.RunId != record.Identity.RunId || response.FencingToken != record.SlotFencingToken ||
		!sameCompetitionAgentIdentity(response.Identity, &record.Identity) || response.ManifestSha256 != record.ManifestSha256 || response.RequestSha256 != record.RequestSha256 ||
		response.ExistingRun != nil || len(response.SelfCheck) != 0 || !terminalCompetitionExpiredControlState(response.State) {
		return proof, ErrLeaseLost
	}
	return proof, nil
}

// Original immutable dimensions are distinct from the old lease and the
// transaction-local replacement lease, neither of which survives completion.
func competitionExpiredControlImmutableMatches(settings *Settings, job *queuedJob, record competitionExpiredControlBridgeRecord) bool {
	if settings == nil || job == nil || job.StartedAt == nil || job.ExecutionDeadline == nil || job.RoundBaseline == nil || job.Round.Canceled || job.Round.FinalizedAt != nil || job.Round.AutomationSealedAt != nil {
		return false
	}
	started := job.StartedAt
	if job.EvaluationCycle > 0 {
		if job.CycleStartedAt == nil || job.CycleAttemptOffset < 1 || job.AttemptCount <= job.CycleAttemptOffset {
			return false
		}
		started = job.CycleStartedAt
	}
	actual := competitionagent.Identity{HostId: job.ExecutorHostId, RunId: competitionagent.RunId(job.JobId.String(), job.EvaluationCycle, job.AttemptCount),
		JobId: job.JobId.String(), RoundId: job.RoundId.String(), RoundEpoch: job.Round.Epoch, SourceEpoch: evaluationSourceEpoch(&job.Round),
		Cycle: job.EvaluationCycle, Attempt: job.AttemptCount, StartedAt: *started, DeadlineAt: *job.ExecutionDeadline}
	return sameCompetitionAgentIdentity(&actual, &record.Identity) && settings.automationRoundId == job.RoundId && settings.executorHostId == record.Identity.HostId && settings.executorSlotId == record.SlotId &&
		settings.AutomationPolicy.Environment == record.Environment && settings.CompetitionId == record.CompetitionId && settings.AutomationPolicy.SeasonId == record.SeasonId &&
		job.SlotId == record.SlotId && job.WorkerImageDigest == record.WorkerImageDigest && job.RoundBaseline.Sha256 == record.RoundBaselineSha256 &&
		settings.AutomationPolicy.SourceLockSha256 == record.SourceLockSha256 && competitionExecutionImageForJob(settings, job) == record.EvaluatorImageDigest &&
		competitionExecutorQualificationSha256(settings) == record.QualificationSha256
}

// Unknown commit is resolved only by the atomic terminal receipt. A restarted
// current supervisor may acknowledge it after the transaction-local lease has
// expired; it can never resume a running intermediate lease or complete again.
func (self PostgresStore) expiredCompetitionJobCompleted(ctx context.Context, settings *Settings, record competitionExpiredControlBridgeRecord, recordSha256 string) (completed bool, resultErr error) {
	connection, present := competitionExpiredControlConnection(settings)
	if !present || connection.ExistingControl != nil || settings.automationLease == nil || connection.ExpiredControlBridge.RecordSha256 != recordSha256 || !competitionDistributedEnabled(settings) {
		return false, ErrLeaseLost
	}
	found := false
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		if host.HostId == settings.executorHostId && host.SlotId == settings.executorSlotId {
			if err := validateCompetitionExpiredControlRecord(record, connection, host, settings.distributedCoordinator.Config.Settings.Limits); err != nil {
				return false, err
			}
			found = true
			break
		}
	}
	if !found {
		return false, ErrLeaseLost
	}
	jobId, err := server.ParseId(record.Identity.JobId)
	if err != nil {
		return false, ErrConflict
	}
	maximum := settings.distributedCoordinator.Config.Settings.Limits.DocumentBytes
	if maximum <= 0 || maximum > math.MaxInt64/3 {
		return false, ErrConflict
	}
	// One bounded record, signed response and outer metadata fit three existing
	// document ceilings. No event body is read when its SQL byte bound fails.
	maximum *= 3
	held := &competitionExpiredControlWitness{record: record}
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			completed = false
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			server.Raise(held.requireHeldInTx(ctx, tx, settings))
			var slotHost string
			var slotJob *server.Id
			var slotWorker *string
			var slotLease *time.Time
			var slotFence int64
			err := tx.QueryRow(ctx, `SELECT host_id,job_id,worker_id,lease_expires_at,fencing_token FROM competition_execution_slot WHERE slot_id=$1 FOR UPDATE`, record.SlotId).
				Scan(&slotHost, &slotJob, &slotWorker, &slotLease, &slotFence)
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			server.Raise(err)
			value := &queuedJob{}
			var baselineSha256 string
			var terminalExpired bool
			err = tx.QueryRow(ctx, `SELECT j.job_id,j.round_id,j.state,j.started_at,j.completed_at,j.attempt_count,COALESCE(j.lease_owner,''),j.lease_expires_at,
 COALESCE(j.worker_image_digest,''),j.api_image_digest,j.evaluation_cycle,j.cycle_attempt_offset,j.cycle_started_at,COALESCE(j.executor_host_id,''),
 COALESCE(j.executor_slot_id,0),COALESCE(j.executor_fencing_token,0),j.execution_deadline_at,
 r.competition_id,r.providers_sha256,r.policy_json,r.canceled,r.epoch_number,r.staging,r.finalized_at,r.automation_sealed_at,b.baseline_sha256,
 (j.state='failed' AND j.score_json IS NULL AND j.eval_error_json->>'code'='evaluation_time_budget_exhausted'
 AND j.eval_error_json->>'kind'='infrastructure' AND COALESCE((j.eval_error_json->>'retriable')::boolean,false)=false
 AND j.artifact_manifest_json IS NULL AND j.artifact_manifest_sha256 IS NULL)
 FROM competition_job j JOIN competition_round r ON r.round_id=j.round_id JOIN competition_round_baseline b ON b.round_id=j.round_id
 WHERE j.job_id=$1 AND j.round_id=$2 FOR UPDATE OF j`, jobId, settings.automationRoundId).
				Scan(&value.JobId, &value.RoundId, &value.State, &value.StartedAt, &value.CompletedAt, &value.AttemptCount, &value.LeaseOwner, &value.LeaseExpiresAt,
					&value.WorkerImageDigest, &value.ApiImageDigest, &value.EvaluationCycle, &value.CycleAttemptOffset, &value.CycleStartedAt, &value.ExecutorHostId,
					&value.SlotId, &value.SlotFencingToken, &value.ExecutionDeadline, &value.Round.CompetitionId, &value.Round.ProvidersSha256, &value.Round.PolicyJson,
					&value.Round.Canceled, &value.Round.Epoch, &value.Round.Staging, &value.Round.FinalizedAt, &value.Round.AutomationSealedAt, &baselineSha256, &terminalExpired)
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			server.Raise(err)
			value.Round.RoundId, value.Staging = value.RoundId, value.Round.Staging
			value.RoundBaseline = &roundBaselineRecord{Sha256: baselineSha256}
			value.EvaluatorImageDigest, err = evaluatorImageDigestFromPolicy(value.Round.PolicyJson)
			server.Raise(err)
			_, err = settingsForFrozenRound(settings, &value.Round)
			server.Raise(err)
			var eventCount int
			var eventId int64
			server.Raise(tx.QueryRow(ctx, `SELECT count(*),COALESCE(min(event_id),0) FROM competition_job_event
 WHERE job_id=$1 AND event_type='execution_reconciled' AND payload_json->'expired_control_bridge'->>'record_sha256'=$2`, jobId, recordSha256).Scan(&eventCount, &eventId))
			if eventCount == 0 && competitionExpiredControlRecordMatches(settings, value, record, self.nowUtc()) &&
				(&competitionExpiredControlWitness{record: record, jobId: jobId}).slotMatches(slotHost, slotJob, slotWorker, slotLease, slotFence) {
				return
			}
			if eventCount != 1 {
				panic(ErrLeaseLost)
			}
			var content []byte
			var expected, actor string
			var eventAt time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT CASE WHEN octet_length(payload_json::text)<=$2 THEN payload_json ELSE NULL END,payload_sha256,actor_id,event_at
 FROM competition_job_event WHERE event_id=$1 AND job_id=$3 AND event_type='execution_reconciled'`, eventId, maximum, jobId).Scan(&content, &expected, &actor, &eventAt))
			proof, err := validateCompetitionExpiredControlClaimEvent(settings, record, recordSha256, eventAt, actor, content, expected)
			server.Raise(err)
			now := self.nowUtc()
			if !competitionExpiredControlImmutableMatches(settings, value, record) || value.SlotFencingToken != proof.NewFence || now.Before(record.Identity.DeadlineAt) || slotHost != record.Identity.HostId || slotFence != proof.NewFence {
				panic(ErrLeaseLost)
			}
			// Any committed running replacement would violate the atomic bridge.
			// Never turn such a row into a second completion/reclaim permission.
			if !terminalExpired || value.CompletedAt == nil || value.CompletedAt.Before(eventAt) || value.CompletedAt.Before(record.Identity.DeadlineAt) ||
				slotJob != nil || slotWorker != nil || slotLease != nil || value.LeaseOwner != "" || value.LeaseExpiresAt != nil {
				panic(ErrLeaseLost)
			}
			var terminalCount int
			var terminalId int64
			server.Raise(tx.QueryRow(ctx, `SELECT count(*),COALESCE(min(event_id),0) FROM competition_job_event WHERE job_id=$1 AND event_id>$2 AND event_type='failed'
 AND actor_id=$3 AND event_at=$4 AND (payload_json->>'attempt')::integer=$5 AND payload_json->>'worker_image_digest'=$6`,
				jobId, eventId, proof.NewLeaseOwner, *value.CompletedAt, record.Identity.Attempt, record.WorkerImageDigest).Scan(&terminalCount, &terminalId))
			if terminalCount != 1 {
				panic(ErrLeaseLost)
			}
			var terminalContent []byte
			var terminalSha256 string
			server.Raise(tx.QueryRow(ctx, `SELECT CASE WHEN octet_length(payload_json::text)<=$2 THEN payload_json ELSE NULL END,payload_sha256
 FROM competition_job_event WHERE event_id=$1 AND job_id=$3`, terminalId, maximum, jobId).Scan(&terminalContent, &terminalSha256))
			server.Raise(validateCompetitionExpiredControlTerminalEvent(value, terminalContent, terminalSha256))
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			server.Raise(held.requireHeldInTx(ctx, tx, settings))
			completed = true
		}, server.OptReadWrite())
	})
	if resultErr != nil {
		return false, resultErr
	}
	return completed, nil
}

// A normal fixed-outcome failed event has exactly these existing fields. It
// cannot hide an artifact/archive field or a different terminal publication.
func validateCompetitionExpiredControlTerminalEvent(job *queuedJob, content []byte, expected string) error {
	var event struct {
		Attempt           int    `json:"attempt"`
		ApiImageDigest    string `json:"api_image_digest"`
		WorkerImageDigest string `json:"worker_image_digest"`
	}
	if job == nil || !sha256Pattern.MatchString(expected) || decodeFailureEvidenceJson(content, &event) != nil ||
		event.Attempt != job.AttemptCount || event.ApiImageDigest != job.ApiImageDigest || event.WorkerImageDigest != job.WorkerImageDigest {
		return ErrLeaseLost
	}
	canonical, err := json.Marshal(map[string]any{"attempt": event.Attempt, "api_image_digest": event.ApiImageDigest, "worker_image_digest": event.WorkerImageDigest})
	digest := sha256.Sum256(canonical)
	if err != nil || hex.EncodeToString(digest[:]) != expected {
		return ErrLeaseLost
	}
	return nil
}
