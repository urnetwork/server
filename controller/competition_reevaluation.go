// A narrowly evidenced operator recovery preserves a paid job and its original
// failure while granting one separately fenced, bounded execution cycle.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Keys are private so candidate input cannot manufacture an execution fence.
type competitionJobAuthorityKey struct{}

type competitionJobAuthority struct {
	JobId            server.Id
	Cycle            int
	Attempt          int
	ExecutorHostId   string
	SlotId           int
	SlotFencingToken int64
	ExecutorSettings *Settings
}

// Detached persistence and cleanup retain values without reviving evaluation.
func withCompetitionJobAuthority(ctx context.Context, job *queuedJob) context.Context {
	executorSettings := job.executorSettings
	if executorSettings != nil {
		copy := *executorSettings
		if copy.automationLease != nil {
			lease := *copy.automationLease
			copy.automationLease = &lease
		}
		executorSettings = &copy
	}
	return context.WithValue(ctx, competitionJobAuthorityKey{}, competitionJobAuthority{
		JobId: job.JobId, Cycle: job.EvaluationCycle, Attempt: job.AttemptCount,
		ExecutorHostId: job.ExecutorHostId, SlotId: job.SlotId, SlotFencingToken: job.SlotFencingToken,
		ExecutorSettings: executorSettings,
	})
}

// Legacy cycle-zero calls remain compatible. A recovered row requires the
// exact claim observed by the caller, even when worker and job ids are reused.
func requireCompetitionJobAuthorityInTx(ctx context.Context, tx server.PgTx, jobId server.Id, cycle, attempt int) error {
	authority, present := ctx.Value(competitionJobAuthorityKey{}).(competitionJobAuthority)
	if cycle > 0 && !present || present && (authority.JobId != jobId || authority.Cycle != cycle || authority.Attempt != attempt) {
		return ErrLeaseLost
	}
	return setCompetitionJobAuthorityInTx(ctx, tx, jobId, cycle, attempt)
}

// Claim supplies freshly locked database identity; other callers must first
// validate their typed context. SQL guards independently reject old binaries.
func setCompetitionJobAuthorityInTx(ctx context.Context, tx server.PgTx, jobId server.Id, cycle, attempt int) error {
	_, err := tx.Exec(ctx, `SELECT set_config('urnetwork.competition_job_authority_id',$1,true),
	 set_config('urnetwork.competition_job_authority_cycle',$2,true),set_config('urnetwork.competition_job_authority_attempt',$3,true)`, jobId.String(), strconv.Itoa(cycle), strconv.Itoa(attempt))
	return err
}

// Only fresh, scoped control owners reclaim a recovered slot. Both existing
// authority rows are locked before clock/fence checks; caller contexts never
// grant this maintenance capability to stale evaluators or cleanup callbacks.
func (self PostgresStore) clearExpiredCompetitionRecoverySlotInTx(ctx context.Context, tx server.PgTx, settings *Settings, slotJob *server.Id) (bool, error) {
	if slotJob == nil {
		return false, nil
	}
	var cycle, attempt int
	var jobExpiry *time.Time
	var state, owner string
	var policyJson []byte
	err := tx.QueryRow(ctx, `SELECT j.evaluation_cycle,j.attempt_count,j.lease_expires_at,j.state,COALESCE(j.lease_owner,''),r.policy_json
	 FROM competition_job j JOIN competition_round r ON r.round_id=j.round_id WHERE j.job_id=$1 FOR UPDATE OF j`, *slotJob).Scan(&cycle, &attempt, &jobExpiry, &state, &owner, &policyJson)
	if err != nil {
		return false, err
	}
	if cycle == 0 {
		return false, nil
	}
	if settings == nil || !settings.AutomationPolicy.Enabled || settings.automationLease == nil {
		return false, ErrLeaseLost
	}
	policy, err := decodeRoundPolicySnapshot(policyJson)
	if err != nil || policy.AutomationPolicy == nil || policy.CompetitionId != settings.CompetitionId || policy.AutomationPolicy.Environment != settings.AutomationPolicy.Environment ||
		policy.AutomationPolicy.SeasonId != settings.AutomationPolicy.SeasonId {
		return false, ErrLeaseLost
	}
	var slotExpiry *time.Time
	var slotOwner *string
	if err := tx.QueryRow(ctx, `SELECT worker_id,lease_expires_at FROM competition_worker_slot WHERE slot_id=1 AND job_id=$1`, *slotJob).Scan(&slotOwner, &slotExpiry); err != nil {
		return false, err
	}
	if err := self.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease); err != nil {
		return false, err
	}
	now := self.nowUtc()
	if slotExpiry == nil || now.Before(*slotExpiry) || jobExpiry != nil && now.Before(*jobExpiry) ||
		state == "running" && (jobExpiry == nil || slotOwner == nil || owner != *slotOwner) {
		return false, ErrLeaseLost
	}
	if err := setCompetitionJobAuthorityInTx(ctx, tx, *slotJob, cycle, attempt); err != nil {
		return false, err
	}
	updated, err := tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=NULL,job_id=NULL,lease_expires_at=NULL,heartbeat_at=$2
	 WHERE slot_id=1 AND job_id=$1 AND lease_expires_at<=$2`, *slotJob, now)
	if err != nil {
		return false, err
	}
	if updated.RowsAffected() != 1 {
		return false, ErrLeaseLost
	}
	return true, nil
}

// Canonical JSONB-object identity is independent of PostgreSQL key order and
// whitespace. Data keys are case-sensitive, not typed protocol fields. Repeated
// keys, nonobjects, invalid utf-8, excessive nesting and trailing data refuse.
func CompetitionJobReevaluationSnapshotDigest(content json.RawMessage) (string, error) {
	if !utf8.Valid(content) {
		return "", ErrConflict
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var readValue func(int) (any, error)
	readValue = func(depth int) (any, error) {
		if 64 < depth {
			return nil, ErrConflict
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, ErrConflict
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return token, nil
		}
		switch delimiter {
		case '{':
			fields := map[string]any{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok {
					return nil, ErrConflict
				}
				if _, duplicate := fields[key]; duplicate {
					return nil, ErrConflict
				}
				value, err := readValue(depth + 1)
				if err != nil {
					return nil, err
				}
				fields[key] = value
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
				return nil, ErrConflict
			}
			return fields, nil
		case '[':
			values := []any{}
			for decoder.More() {
				value, err := readValue(depth + 1)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
				return nil, ErrConflict
			}
			return values, nil
		default:
			return nil, ErrConflict
		}
	}
	value, err := readValue(0)
	if _, ok := value.(map[string]any); err != nil || !ok {
		return "", ErrConflict
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", ErrConflict
	}
	return model.CompetitionAutomationDigest(value)
}

// Only the privileged local CLI binds a separately pinned qualification. This
// is not inferred from version strings, old image receipts or a successful Go run.
func BindCompetitionJobReevaluationRuntime(settings *Settings, proof model.CompetitionJobReevaluationRuntime, digest string) error {
	if proof.Schema == 2 || proof.Kind == model.CompetitionDistributedJobReevaluationRuntimeKind {
		return bindCompetitionDistributedReevaluationRuntime(settings, proof, digest)
	}
	if settings == nil || proof.Schema != 1 || proof.Kind != model.CompetitionJobReevaluationRuntimeKind || !proof.Accepted ||
		proof.Capability != model.CompetitionJobReevaluationCapability || !proof.ApiCompatible || !proof.WorkerCompatible ||
		!gitShaPattern.MatchString(proof.SourceCommit) || !imageDigestPattern.MatchString(proof.ApiImageDigest) ||
		!imageDigestPattern.MatchString(proof.WorkerImageDigest) || !sha256Pattern.MatchString(proof.WorkerSha256) ||
		!sha256Pattern.MatchString(proof.EvidenceSha256) || !sha256Pattern.MatchString(digest) {
		return ErrConflict
	}
	seen := map[string]bool{}
	for _, class := range proof.IncidentClasses {
		if seen[class] || (class != model.CompetitionJobReevaluationHeartbeatBudget && class != model.CompetitionJobReevaluationBackingService && class != model.CompetitionJobReevaluationOperatorInterruption) {
			return ErrConflict
		}
		seen[class] = true
	}
	actual, err := model.CompetitionAutomationDigest(proof)
	if err != nil || actual != digest {
		return ErrConflict
	}
	copy := proof
	copy.IncidentClasses = append([]string(nil), proof.IncidentClasses...)
	settings.reevaluationRuntime = &copy
	settings.reevaluationRuntimeSha256 = digest
	return nil
}

// The sealed manifest is read before database locks. Only declarations are
// inspected: no omitted inventory content is opened, hashed or uploaded.
func validateCompetitionReevaluationInventory(settings *Settings, auth model.CompetitionJobReevaluationAuthorization) error {
	if auth.IncidentAttempt < 1 || auth.ExpectedAttemptCount < auth.IncidentAttempt || !sha256Pattern.MatchString(auth.FailureManifestSha256) ||
		(auth.RejectedArtifactPath != "failed-evidence/evaluation-sources.json" && auth.RejectedArtifactPath != "failed-evidence/evaluation-sources.json.new") {
		return ErrConflict
	}
	root := filepath.Join(settings.ArtifactRoot, auth.JobId.String(), fmt.Sprintf("attempt-%02d", auth.IncidentAttempt))
	content, artifact, err := readFailureArtifact(root, "failed-evidence-manifest.json", maxFailureManifestBytes)
	if err != nil || artifact.Sha256 != auth.FailureManifestSha256 {
		return ErrConflict
	}
	var manifest failedEvidenceManifest
	if decodeFailureEvidenceJson(content, &manifest) != nil || manifest.Schema != 1 || manifest.Kind != "sim-latency-failed-evidence-manifest" ||
		manifest.JobId != auth.JobId.String() || manifest.RoundId != auth.RoundId.String() || manifest.Attempt != auth.IncidentAttempt ||
		!manifest.Sanitized || len(manifest.Artifacts) == 0 || maxFailureArtifacts < len(manifest.Artifacts) {
		return ErrConflict
	}
	seen := map[string]bool{}
	var total int64
	for _, declared := range manifest.Artifacts {
		inventory := declared.Path == "failed-evidence/evaluation-sources.json" || declared.Path == "failed-evidence/evaluation-sources.json.new"
		if (!safeFailureEvidencePath(declared.Path) && !inventory) || seen[declared.Path] || declared.Bytes == nil || *declared.Bytes < 0 ||
			maxFailureEvidenceBytes-total < *declared.Bytes || !sha256Pattern.MatchString(declared.Sha256) {
			return ErrConflict
		}
		seen[declared.Path] = true
		total += *declared.Bytes
	}
	if !seen[auth.RejectedArtifactPath] || !seen["failed-evidence/failure.json"] {
		return ErrConflict
	}
	return nil
}

// Authorization is deliberately unavailable to submitters, evaluators and
// source promoters. Existing exact-scoped scheduler credentials are the v2
// operator control role; unscoped legacy operators are not wildcards.
func validateCompetitionReevaluationRequest(settings *Settings, principal *Principal, request model.CompetitionJobReevaluationRequest) error {
	a := request.Authorization
	if settings == nil || !settings.AutomationPolicy.Enabled ||
		(!competitionPrincipalAllows(principal, competitionSchedulerRole, settings.CompetitionId, settings.AutomationPolicy) &&
			!competitionPrincipalAllows(principal, "operator", settings.CompetitionId, settings.AutomationPolicy)) {
		return ErrAutomationDisabled
	}
	if !tokenNamePattern.MatchString(request.IdempotencyKey) || !sha256Pattern.MatchString(request.AuthorizationSha256) ||
		a.Schema != 1 || a.Kind != model.CompetitionJobReevaluationAuthorizationKind || a.Distributed != nil ||
		a.Environment != settings.AutomationPolicy.Environment || a.CompetitionId != settings.CompetitionId || a.SeasonId != settings.AutomationPolicy.SeasonId ||
		a.JobId == (server.Id{}) || a.RoundId == (server.Id{}) || a.ExpectedCycle != 0 || a.ExpectedAttemptCount < 1 || !a.SingleUse ||
		a.AuthorizedAt.IsZero() || !a.AuthorizedAt.Before(a.ExpiresAt) || !sha256Pattern.MatchString(a.EvidenceSha256) ||
		!sha256Pattern.MatchString(a.ExpectedTerminalSha256) || a.IncidentEventId < 1 || !sha256Pattern.MatchString(a.IncidentEventSha256) ||
		!sha256Pattern.MatchString(a.RoundPolicySha256) || !sha256Pattern.MatchString(a.BaselineSha256) {
		return ErrConflict
	}
	switch a.IncidentClass {
	case model.CompetitionJobReevaluationArchiveInventory:
		// The separately authenticated manifest remains mandatory for this class.
	case model.CompetitionJobReevaluationLegacyArchiveBudget:
		// Explicit operator approval covers an untyped earlier archive interruption,
		// not a fabricated inventory diagnosis or a generic expired execution.
		if a.IncidentAttempt < 1 || a.ExpectedAttemptCount <= a.IncidentAttempt || a.FailureManifestSha256 != "" || a.RejectedArtifactPath != "" {
			return ErrConflict
		}
	case model.CompetitionJobReevaluationHeartbeatBudget:
		if a.IncidentAttempt < 1 || a.ExpectedAttemptCount <= a.IncidentAttempt || a.FailureManifestSha256 != "" || a.RejectedArtifactPath != "" {
			return ErrConflict
		}
	case model.CompetitionJobReevaluationBackingService:
		if validateCompetitionReevaluationBackingServiceShape(settings, a) != nil {
			return ErrConflict
		}
	case model.CompetitionJobReevaluationOperatorInterruption:
		if validateCompetitionReevaluationOperatorInterruptionShape(a) != nil {
			return ErrConflict
		}
	default:
		return ErrConflict
	}
	if a.IncidentClass != model.CompetitionJobReevaluationBackingService && a.BackingServiceEvidence != nil {
		return ErrConflict
	}
	if a.IncidentClass != model.CompetitionJobReevaluationOperatorInterruption && a.OperatorInterruptionEvidence != nil {
		return ErrConflict
	}
	if err := validateCompetitionReevaluationPostCloseApproval(a); err != nil {
		return err
	}
	digest, err := model.CompetitionAutomationDigest(a)
	if err != nil || digest != request.AuthorizationSha256 {
		return ErrConflict
	}
	if digest, err := CompetitionJobReevaluationSnapshotDigest(a.ExpectedTerminalSnapshot); err != nil || digest != a.ExpectedTerminalSha256 {
		return ErrConflict
	}
	return nil
}

// Current deployment proof gates a new grant, never exact committed replay.
func validateCompetitionReevaluationRuntime(settings *Settings, a model.CompetitionJobReevaluationAuthorization) error {
	proof := settings.reevaluationRuntime
	if proof == nil || proof.Schema != 1 || proof.Kind != model.CompetitionJobReevaluationRuntimeKind || proof.Capability != model.CompetitionJobReevaluationCapability ||
		settings.reevaluationRuntimeSha256 != a.RuntimeQualificationSha256 || proof.SourceCommit != a.FixedSourceCommit ||
		proof.WorkerSha256 != a.FixedWorkerSha256 || proof.WorkerImageDigest != a.FixedWorkerImageDigest || proof.ApiImageDigest != a.ApiImageDigest {
		return ErrConflict
	}
	if (a.IncidentClass == model.CompetitionJobReevaluationHeartbeatBudget || a.IncidentClass == model.CompetitionJobReevaluationBackingService || a.IncidentClass == model.CompetitionJobReevaluationOperatorInterruption) &&
		!slices.Contains(proof.IncidentClasses, a.IncidentClass) {
		return ErrConflict
	}
	copySettings := *settings
	if BindCompetitionJobReevaluationRuntime(&copySettings, *proof, a.RuntimeQualificationSha256) != nil {
		return ErrConflict
	}
	return nil
}

// A replay reads immutable audit data only. Retained local files, current
// runtime and elapsed approval windows cannot grant or erase that receipt.
func readCompetitionReevaluationReceipt(ctx context.Context, settings *Settings, request model.CompetitionJobReevaluationRequest, requestDigest string) (receipt *model.CompetitionJobReevaluationReceipt, err error) {
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var key, digest string
			var content []byte
			loadErr := conn.QueryRow(ctx, `SELECT idempotency_key,request_sha256,receipt_json FROM competition_job_reevaluation WHERE job_id=$1`, request.Authorization.JobId).Scan(&key, &digest, &content)
			if errors.Is(loadErr, pgx.ErrNoRows) {
				return
			}
			server.Raise(loadErr)
			if key != request.IdempotencyKey || digest != requestDigest {
				panic(ErrConflict)
			}
			var result model.CompetitionJobReevaluationReceipt
			server.Raise(decodeFailureEvidenceJson(content, &result))
			server.Raise(model.ValidateCompetitionJobReevaluationCycle(&result.CompetitionJobReevaluationCycle))
			server.Raise(validateCompetitionDistributedRecoveryReceipt(&result))
			receipt = &result
		})
	})
	return receipt, err
}

// A terminal code alone cannot grant recovery. The typed class proves the
// exact inventory defect; explicit legacy approval binds a real earlier
// archive interruption without claiming its unknown backend cause.
func validateCompetitionReevaluationIncidentInTx(ctx context.Context, tx server.PgTx, auth model.CompetitionJobReevaluationAuthorization, started, completed time.Time) error {
	var at time.Time
	var eventType, digest string
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT event_at,event_type,payload_sha256,payload_json FROM competition_job_event WHERE event_id=$1 AND job_id=$2`, auth.IncidentEventId, auth.JobId).Scan(&at, &eventType, &digest, &payload)
	if err != nil {
		return ErrConflict
	}
	var event struct {
		Attempt      int                                `json:"attempt"`
		ErrorCode    json.RawMessage                    `json:"error_code"`
		Failure      *competitionArtifactArchiveFailure `json:"artifact_archive_failure"`
		Reason       string                             `json:"reason"`
		Category     string                             `json:"error_category"`
		DetailSha256 string                             `json:"error_sha256"`
	}
	if json.Unmarshal(payload, &event) != nil || digest != auth.IncidentEventSha256 || at.Before(started) || completed.Before(at) {
		return ErrConflict
	}
	if auth.IncidentClass == model.CompetitionJobReevaluationOperatorInterruption {
		if eventType != "failed" || event.Attempt != auth.ExpectedAttemptCount || event.Failure != nil || !at.Equal(completed) {
			return ErrConflict
		}
		if len(event.ErrorCode) != 0 {
			var code string
			if json.Unmarshal(event.ErrorCode, &code) != nil || code != "evaluation_time_budget_exhausted" {
				return ErrConflict
			}
		}
		return validateCompetitionReevaluationOperatorInterruptionInTx(ctx, tx, auth, started, completed)
	}
	if auth.IncidentClass == model.CompetitionJobReevaluationHeartbeatBudget {
		if eventType != "handed_back" || (event.Reason != "heartbeat_failed" && event.Reason != "host_heartbeat_failed") ||
			(event.Category != "deadline_exceeded" && event.Category != "lease_lost" && event.Category != "postgresql" && event.Category != "network") ||
			!sha256Pattern.MatchString(event.DetailSha256) || auth.IncidentAttempt < 1 || auth.IncidentAttempt >= auth.ExpectedAttemptCount {
			return ErrConflict
		}
		var claimedAttempt int
		if err := tx.QueryRow(ctx, `SELECT (payload_json->>'attempt')::integer FROM competition_job_event
		 WHERE job_id=$1 AND event_type='claimed' AND (event_at,event_id)<=($2,$3)
		 ORDER BY event_at DESC,event_id DESC LIMIT 1`, auth.JobId, at, auth.IncidentEventId).Scan(&claimedAttempt); err != nil || claimedAttempt != auth.IncidentAttempt {
			return ErrConflict
		}
		return nil
	}
	if event.Attempt != auth.IncidentAttempt || (eventType != "failed" && eventType != "infrastructure_retry") {
		return ErrConflict
	}
	if auth.IncidentClass == model.CompetitionJobReevaluationBackingService {
		if eventType != "failed" || event.Failure != nil {
			return ErrConflict
		}
		// Historical terminal events omit error_code; the separately locked
		// terminal snapshot binds its exact kind/code. Present fields must agree.
		if len(event.ErrorCode) != 0 {
			var errorCode string
			if json.Unmarshal(event.ErrorCode, &errorCode) != nil || errorCode != "run_unstable" {
				return ErrConflict
			}
		}
		return nil
	}
	if auth.IncidentClass == model.CompetitionJobReevaluationLegacyArchiveBudget {
		var errorCode string
		if json.Unmarshal(event.ErrorCode, &errorCode) != nil || eventType != "infrastructure_retry" || errorCode != "artifact_archive_failed" || event.Failure != nil ||
			event.Attempt < 1 || auth.ExpectedAttemptCount <= event.Attempt {
			return ErrConflict
		}
		return nil
	}
	if auth.IncidentClass != model.CompetitionJobReevaluationArchiveInventory || event.Failure == nil {
		return ErrConflict
	}
	wanted := sha256.Sum256([]byte("failure evidence manifest contains an unsafe artifact"))
	failure := event.Failure
	if failure.Phase != "authenticate_failed_evidence" || failure.Category != "other" || failure.DetailSha256 != hex.EncodeToString(wanted[:]) || failure.S3Code != "" || failure.HttpStatus != 0 {
		return ErrConflict
	}
	return nil
}

// One transaction records the complete old terminal projection before a
// narrowly guarded queue transition. Exact replay is receipt-only, not a reset.
func (self PostgresStore) AuthorizeJobReevaluation(ctx context.Context, settings *Settings, principal *Principal, request model.CompetitionJobReevaluationRequest) (receipt *model.CompetitionJobReevaluationReceipt, err error) {
	if request.Authorization.Schema == 2 || request.Authorization.Kind == model.CompetitionDistributedJobReevaluationAuthorizationKind {
		return self.authorizeDistributedJobReevaluation(ctx, settings, principal, request, competitionTransportForHost)
	}
	if err := validateCompetitionReevaluationRequest(settings, principal, request); err != nil {
		return nil, err
	}
	requestDigest, err := model.CompetitionAutomationDigest(request)
	if err != nil {
		return nil, err
	}
	a := request.Authorization
	if existing, loadErr := readCompetitionReevaluationReceipt(ctx, settings, request, requestDigest); loadErr != nil || existing != nil {
		return existing, loadErr
	}
	preflightError := validateCompetitionReevaluationRuntime(settings, a)
	if preflightError == nil && a.IncidentClass == model.CompetitionJobReevaluationArchiveInventory {
		preflightError = validateCompetitionReevaluationInventory(settings, a)
	}
	if preflightError == nil && a.IncidentClass == model.CompetitionJobReevaluationBackingService {
		preflightError = validateCompetitionReevaluationBackingService(settings, a, defaultCompetitionReevaluationEvidenceSettings())
	}
	// Replay remains possible after retained local files or the authorization
	// lifetime expire, but only after its exact immutable request has matched.
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			receipt = nil
			automationScopeLockInTx(ctx, tx, settings)
			var previousKey, previousDigest string
			var previousReceipt []byte
			loadErr := tx.QueryRow(ctx, `SELECT idempotency_key,request_sha256,receipt_json FROM competition_job_reevaluation WHERE job_id=$1`, a.JobId).Scan(&previousKey, &previousDigest, &previousReceipt)
			if loadErr == nil {
				if previousKey != request.IdempotencyKey || previousDigest != requestDigest {
					panic(ErrConflict)
				}
				var previous model.CompetitionJobReevaluationReceipt
				server.Raise(decodeFailureEvidenceJson(previousReceipt, &previous))
				server.Raise(model.ValidateCompetitionJobReevaluationCycle(&previous.CompetitionJobReevaluationCycle))
				receipt = &previous
				return
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			server.Raise(preflightError)
			var slotWorker *string
			var slotJob *server.Id
			var slotExpiry *time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT worker_id,job_id,lease_expires_at FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&slotWorker, &slotJob, &slotExpiry))
			var terminal []byte
			var state string
			var attempts, cycle int
			var started, completed *time.Time
			var score, errorJson []byte
			var leaseOwner *string
			var leaseExpiry *time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT to_jsonb(j),state,attempt_count,evaluation_cycle,started_at,completed_at,score_json,eval_error_json,lease_owner,lease_expires_at FROM competition_job j WHERE job_id=$1 AND round_id=$2 FOR UPDATE`, a.JobId, a.RoundId).Scan(&terminal, &state, &attempts, &cycle, &started, &completed, &score, &errorJson, &leaseOwner, &leaseExpiry))
			round, roundErr := scanRound(tx.QueryRow(ctx, `SELECT round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled,finalized_at,winner_job_id,admission_closed_at,automation_sealed_at,automation_publication_at FROM competition_round WHERE round_id=$1 FOR UPDATE`, a.RoundId))
			server.Raise(roundErr)
			server.Raise(validateCompetitionReevaluationRound(round))
			now := self.nowUtc()
			var running bool
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job WHERE state='running')`).Scan(&running))
			if running || slotWorker != nil && (slotExpiry == nil || now.Before(*slotExpiry)) || slotJob != nil {
				panic(ErrLeaseLost)
			}
			if !competitionReevaluationWithinApprovedWindow(a, round, now) || round.Canceled || round.FinalizedAt != nil || round.AutomationSealedAt != nil || round.AdmissionClosedAt != nil || round.WinnerJobId != nil ||
				round.CompetitionId != settings.CompetitionId || !storedPolicyMatches(settings, round.PolicyJson) {
				panic(ErrConflict)
			}
			if state != "failed" || cycle != 0 || attempts != a.ExpectedAttemptCount || started == nil || completed == nil || leaseOwner != nil || leaseExpiry != nil ||
				len(score) != 0 && string(score) != "null" {
				panic(ErrConflict)
			}
			var failure *CompetitionError
			server.Raise(json.Unmarshal(errorJson, &failure))
			if failure == nil || !model.CompetitionJobReevaluationFailureAllowed(a.IncidentClass, model.CompetitionEvaluationFailure{Kind: failure.Kind, Code: failure.Code, Retriable: failure.Retriable}) {
				panic(ErrConflict)
			}
			if a.IncidentClass == model.CompetitionJobReevaluationLegacyArchiveBudget && failure.Code != "evaluation_time_budget_exhausted" {
				panic(ErrConflict)
			}
			digest, digestErr := CompetitionJobReevaluationSnapshotDigest(terminal)
			server.Raise(digestErr)
			if digest != a.ExpectedTerminalSha256 {
				panic(ErrConflict)
			}
			policyDigest, digestErr := CompetitionJobReevaluationSnapshotDigest(round.PolicyJson)
			server.Raise(digestErr)
			if policyDigest != a.RoundPolicySha256 {
				panic(ErrConflict)
			}
			holds, holdErr := readCompetitionIntakeHolds(ctx, tx, settings)
			server.Raise(holdErr)
			if !holds.Evaluation || a.PostCloseApproval != nil && !holds.Finalization {
				panic(ErrAutomationHeld)
			}
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, a.RoundId))
			current, sourceErr := currentSourceActivationInTx(ctx, tx, settings)
			server.Raise(sourceErr)
			if current.ActivatedAt == nil || current.AbandonedAt != nil || current.TargetRoundId != a.RoundId || current.TargetEpoch != round.Epoch || current.Commits["server"] != settings.BaseSha || current.SourceLockSha256 != settings.AutomationPolicy.SourceLockSha256 {
				panic(ErrConflict)
			}
			baseline, baselineErr := scanRoundBaseline(tx.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,source_artifact_manifest_sha256,base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at FROM competition_round_baseline WHERE round_id=$1 FOR SHARE`, a.RoundId))
			server.Raise(baselineErr)
			server.Raise(validateRoundBaselineRecord(settings, round, baseline))
			if baseline.Sha256 != a.BaselineSha256 {
				panic(ErrConflict)
			}
			server.Raise(validateCompetitionReevaluationIncidentInTx(ctx, tx, a, *started, *completed))
			now = self.nowUtc()
			if !competitionReevaluationWithinApprovedWindow(a, round, now) {
				panic(ErrConflict)
			}
			cycleReceipt := model.CompetitionJobReevaluationCycle{Schema: 1, Environment: a.Environment, CompetitionId: a.CompetitionId, SeasonId: a.SeasonId, JobId: a.JobId, RoundId: a.RoundId, PatchSha256: "", RoundPolicySha256: a.RoundPolicySha256, BaselineSha256: a.BaselineSha256, Cycle: 1, AuthorizationSha256: request.AuthorizationSha256, EvidenceSha256: a.EvidenceSha256, PreviousTerminalSha256: a.ExpectedTerminalSha256, PreviousFailure: model.CompetitionEvaluationFailure{Kind: failure.Kind, Code: failure.Code, Retriable: false}, RuntimeQualificationSha256: a.RuntimeQualificationSha256, FixedSourceCommit: a.FixedSourceCommit, FixedWorkerSha256: a.FixedWorkerSha256, FixedWorkerImageDigest: a.FixedWorkerImageDigest, ApiImageDigest: a.ApiImageDigest, AuthorizedAt: now, ExecutionTimeoutSeconds: settings.EvaluationPolicy.ScoreTimeoutSeconds}
			if a.IncidentClass == model.CompetitionJobReevaluationHeartbeatBudget || a.IncidentClass == model.CompetitionJobReevaluationBackingService || a.IncidentClass == model.CompetitionJobReevaluationOperatorInterruption {
				cycleReceipt.IncidentClass = a.IncidentClass
			}
			server.Raise(tx.QueryRow(ctx, `SELECT patch_sha256 FROM competition_job WHERE job_id=$1`, a.JobId).Scan(&cycleReceipt.PatchSha256))
			cycleReceipt.ReceiptSha256, digestErr = model.CompetitionJobReevaluationCycleDigest(cycleReceipt)
			server.Raise(digestErr)
			server.Raise(model.ValidateCompetitionJobReevaluationCycle(&cycleReceipt))
			receipt = &model.CompetitionJobReevaluationReceipt{CompetitionJobReevaluationCycle: cycleReceipt, RequestSha256: requestDigest, AttemptOffset: attempts}
			authorizationJson, jsonErr := json.Marshal(a)
			server.Raise(jsonErr)
			receiptJson, jsonErr := json.Marshal(receipt)
			server.Raise(jsonErr)
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_reevaluation_operator',$1,true),set_config('urnetwork.competition_reevaluation_request',$2,true),set_config('urnetwork.competition_v2_worker','on',true)`, principal.Id, requestDigest))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_job_reevaluation(job_id,round_id,environment,competition_id,season_id,operator_id,idempotency_key,request_sha256,authorization_sha256,authorized_at,original_terminal_json,authorization_json,receipt_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12::jsonb,$13::jsonb)`, a.JobId, a.RoundId, a.Environment, a.CompetitionId, a.SeasonId, principal.Id, request.IdempotencyKey, requestDigest, request.AuthorizationSha256, now, string(terminal), string(authorizationJson), string(receiptJson)))
			updated, updateErr := tx.Exec(ctx, `UPDATE competition_job SET evaluation_cycle=1,cycle_attempt_offset=attempt_count,cycle_started_at=NULL,state='queued',available_at=$2,completed_at=NULL,score_json=NULL,eval_error_json=NULL,artifact_manifest_json=NULL,artifact_manifest_sha256=NULL WHERE job_id=$1 AND state='failed' AND evaluation_cycle=0`, a.JobId, now)
			server.Raise(updateErr)
			if updated.RowsAffected() != 1 {
				panic(ErrConflict)
			}
			appendEvent(ctx, tx, a.JobId, now, "reevaluation_authorized", principal.Id, receipt)
		})
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}
