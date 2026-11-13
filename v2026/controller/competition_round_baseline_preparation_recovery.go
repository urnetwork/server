// A private Root-pinned authorization can reconcile one proven pre-native,
// zero-sample failure without rewriting its original owner or reservation clock.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// This control-only capability is absent from API and frozen round policy.
// Approval/evidence are reviewed Root files, not inferred measurement booleans.
type roundBaselineRecoveryAuthorization struct {
	Schema                   int                         `json:"schema"`
	Kind                     string                      `json:"kind"`
	RoundId                  server.Id                   `json:"round_id"`
	JobId                    server.Id                   `json:"job_id"`
	Owner                    string                      `json:"owner"`
	PreviousContextSha256    string                      `json:"previous_context_sha256"`
	PreviousWorkerImage      string                      `json:"previous_worker_image_digest"`
	ReplacementWorkerImage   string                      `json:"replacement_worker_image_digest"`
	ReplacementProgramSha256 string                      `json:"replacement_program_sha256"`
	ApprovedAt               time.Time                   `json:"approved_at"`
	Approval                 roundBaselinePreparationPin `json:"approval"`
	Evidence                 roundBaselinePreparationPin `json:"evidence"`
	FailedProgram            roundBaselinePreparationPin `json:"failed_program"`
	HoldSha256               string                      `json:"hold_sha256"`
	SummarySha256            string                      `json:"stderr_summary_sha256"`
	RequestSha256            string                      `json:"request_sha256"`
	ExecutionClock           string                      `json:"execution_clock,omitempty"`
}

// This private operator capability changes only zero-sample setup execution.
// It never changes submission attempts, candidate clocks or frozen round policy.
const roundBaselineRecoveredExecutionClock = "operator_recovered_zero_sample_three_hour_v1"

// Startup authenticates sealed metadata and opaque input hashes without parsing
// seed/provider contents or retaining them in a durable event.
type roundBaselineRecoveryCapability struct {
	Authorization                roundBaselineRecoveryAuthorization
	AuthorizationSha256          string
	PatchSha256, ProvidersSha256 string
}

// One append-only transition retains both actual control versions and contexts.
// EventId is the database fence token, excluded from canonical payload bytes.
type roundBaselineRecoveryTransition struct {
	Schema                int        `json:"schema"`
	Kind                  string     `json:"kind"`
	Owner                 string     `json:"owner"`
	RoundId               server.Id  `json:"round_id"`
	JobId                 server.Id  `json:"job_id"`
	PreviousHoldId        int64      `json:"previous_hold_event_id"`
	PreviousContextSha256 string     `json:"previous_context_sha256"`
	ContextSha256         string     `json:"context_sha256"`
	PreviousWorkerId      string     `json:"previous_worker_id"`
	WorkerId              string     `json:"worker_id"`
	PreviousWorkerImage   string     `json:"previous_worker_image_digest"`
	WorkerImage           string     `json:"worker_image_digest"`
	StartedAt             time.Time  `json:"started_at"`
	Deadline              time.Time  `json:"deadline"`
	ExecutionClock        string     `json:"execution_clock,omitempty"`
	ExecutionStartedAt    *time.Time `json:"execution_started_at,omitempty"`
	ExecutionDeadline     *time.Time `json:"execution_deadline,omitempty"`
	AuthorizationSha256   string     `json:"authorization_sha256"`
	ApprovalSha256        string     `json:"approval_sha256"`
	EvidenceSha256        string     `json:"evidence_sha256"`
	EventId               int64      `json:"-"`
}

// The old held event used this map's canonical encoding, not jsonb key order.
type roundBaselineHeldEvent struct {
	Schema        int                          `json:"schema"`
	Owner         string                       `json:"owner"`
	ContextSha256 string                       `json:"context_sha256"`
	Hold          roundBaselinePreparationHold `json:"hold"`
}

// Reads only bounded Root-protected, immutable approval/configuration bytes.
func readRoundBaselineRecoveryPin(pin roundBaselinePreparationPin, maximum int64) ([]byte, error) {
	if !sha256Pattern.MatchString(pin.Sha256) {
		return nil, ErrConflict
	}
	if err := competitionQualificationRootPath(pin.Path, false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(pin.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 {
		return nil, ErrConflict
	}
	content, err := readCompetitionPreparationFile(pin.Path, maximum, false)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != pin.Sha256 {
		return nil, ErrConflict
	}
	return content, nil
}

// The six-file census excludes even a partial control/evidence/completion tree.
// The three inputs are streamed only into hashes; they are never JSON/YAML parsed.
func authenticateRoundBaselineRecoveryFiles(settings *Settings, authorization roundBaselineRecoveryAuthorization) (string, string, error) {
	directory := filepath.Join(settings.ArtifactRoot, "baseline-preparations", authorization.RoundId.String(), authorization.Owner)
	root, err := openFailureArtifactPath(directory, "", true)
	if err != nil {
		return "", "", err
	}
	entries, readErr := root.ReadDir(7)
	closeErr := root.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil || len(entries) != 6 {
		return "", "", ErrConflict
	}
	expected := map[string]bool{"worker-request.json": true, "providers.yml": true, "canonical.patch": true,
		"round-baseline-preparation.hold.json": true, "baseline-preparation.stderr-summary.json": true, "baseline-preparation.stderr.log": true}
	digests := map[string]string{}
	limit := settings.roundBaselinePreparation.Settings.MaximumRecordBytes
	for _, entry := range entries {
		if !expected[entry.Name()] || entry.IsDir() {
			return "", "", ErrConflict
		}
		delete(expected, entry.Name())
		file, err := openFailureArtifactPath(directory, entry.Name(), false)
		if err != nil {
			return "", "", err
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 ||
			info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Size() < 0 || info.Size() > limit {
			file.Close()
			return "", "", ErrConflict
		}
		hash := sha256.New()
		count, hashErr := io.Copy(hash, io.LimitReader(file, limit+1))
		closeErr := file.Close()
		if hashErr != nil || closeErr != nil || count != info.Size() {
			return "", "", ErrConflict
		}
		digests[entry.Name()] = hex.EncodeToString(hash.Sum(nil))
		if entry.Name() == "baseline-preparation.stderr.log" && count != 0 {
			return "", "", ErrConflict
		}
	}
	if len(expected) != 0 || digests["worker-request.json"] != authorization.RequestSha256 ||
		digests["round-baseline-preparation.hold.json"] != authorization.HoldSha256 ||
		digests["baseline-preparation.stderr-summary.json"] != authorization.SummarySha256 {
		return "", "", ErrConflict
	}
	content, _, err := readRoundBaselinePreparationArtifact(directory, "round-baseline-preparation.hold.json", limit)
	if err != nil {
		return "", "", err
	}
	var hold struct {
		Schema            int       `json:"schema"`
		Kind              string    `json:"kind"`
		Owner             string    `json:"owner"`
		ContextSha256     string    `json:"context_sha256"`
		JobId             server.Id `json:"job_id"`
		RoundId           server.Id `json:"round_id"`
		SubmissionStarted *bool     `json:"submission_started"`
		CandidateExecuted *bool     `json:"candidate_executed"`
	}
	if decodeCompetitionPreparationJson(bytes.NewReader(content), limit, &hold) != nil ||
		hold.Schema != 1 || hold.Kind != "sim-latency-round-baseline-preparation-infrastructure-hold" ||
		hold.Owner != authorization.Owner || hold.ContextSha256 != authorization.PreviousContextSha256 ||
		hold.JobId != authorization.JobId || hold.RoundId != authorization.RoundId ||
		hold.SubmissionStarted == nil || *hold.SubmissionStarted || hold.CandidateExecuted == nil || *hold.CandidateExecuted {
		return "", "", ErrConflict
	}
	content, _, err = readRoundBaselinePreparationArtifact(directory, "baseline-preparation.stderr-summary.json", limit)
	if err != nil {
		return "", "", err
	}
	var summary struct {
		Schema    int    `json:"schema"`
		Kind      string `json:"kind"`
		Limit     int64  `json:"limit_bytes"`
		Observed  *int64 `json:"observed_bytes"`
		Retained  *int64 `json:"retained_bytes"`
		Truncated *bool  `json:"truncated"`
	}
	if decodeCompetitionPreparationJson(bytes.NewReader(content), limit, &summary) != nil ||
		summary.Schema != 1 || summary.Kind != "sim-latency-round-baseline-preparation-log" || summary.Limit <= 0 ||
		summary.Observed == nil || *summary.Observed != 0 || summary.Retained == nil || *summary.Retained != 0 ||
		summary.Truncated == nil || *summary.Truncated {
		return "", "", ErrConflict
	}
	return digests["canonical.patch"], digests["providers.yml"], nil
}

// Called only after ordinary pinned caller/dependency configuration succeeds.
func configureRoundBaselineRecovery(settings *Settings, profile *roundBaselinePreparationProfile) error {
	if profile.Config.ZeroSampleRecovery == nil {
		return nil
	}
	pin := *profile.Config.ZeroSampleRecovery
	content, err := readRoundBaselineRecoveryPin(pin, profile.Settings.MaximumRecordBytes)
	if err != nil {
		return err
	}
	var authorization roundBaselineRecoveryAuthorization
	if decodeCompetitionPreparationJson(bytes.NewReader(content), profile.Settings.MaximumRecordBytes, &authorization) != nil ||
		authorization.Schema != 1 || authorization.Kind != "sim-latency-round-baseline-zero-sample-recovery" ||
		authorization.RoundId == (server.Id{}) || authorization.JobId == (server.Id{}) ||
		!workerIdPattern.MatchString(authorization.Owner) || !sha256Pattern.MatchString(authorization.PreviousContextSha256) ||
		!imageDigestPattern.MatchString(authorization.PreviousWorkerImage) || !imageDigestPattern.MatchString(authorization.ReplacementWorkerImage) ||
		authorization.ReplacementProgramSha256 != profile.Config.Program.Sha256 ||
		authorization.FailedProgram.Sha256 == profile.Config.Program.Sha256 ||
		authorization.ApprovedAt.IsZero() || authorization.ApprovedAt.After(profile.Settings.Now()) ||
		!sha256Pattern.MatchString(authorization.HoldSha256) || !sha256Pattern.MatchString(authorization.SummarySha256) ||
		!sha256Pattern.MatchString(authorization.RequestSha256) {
		return ErrConflict
	}
	if authorization.ExecutionClock != "" && authorization.ExecutionClock != roundBaselineRecoveredExecutionClock {
		return ErrConflict
	}
	for _, evidence := range []roundBaselinePreparationPin{authorization.Approval, authorization.Evidence, authorization.FailedProgram} {
		if _, err := readRoundBaselineRecoveryPin(evidence, profile.Settings.MaximumRecordBytes); err != nil {
			return err
		}
	}
	copy := *settings
	copy.roundBaselinePreparation = profile
	patch, providers, err := authenticateRoundBaselineRecoveryFiles(&copy, authorization)
	if err != nil {
		return err
	}
	profile.Recovery = &roundBaselineRecoveryCapability{Authorization: authorization, AuthorizationSha256: pin.Sha256, PatchSha256: patch, ProvidersSha256: providers}
	return nil
}

// Every hold remains effective except the exact one reconciled by the transition.
func roundBaselinePreparationHeldInTx(ctx context.Context, tx server.PgTx, preparation *roundBaselinePreparation) (bool, error) {
	var ignored any
	if preparation.Recovery != nil {
		ignored = preparation.Recovery.PreviousHoldId
	}
	var held bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job_event
 WHERE job_id=$1 AND event_type='round_baseline_preparation_held' AND payload_json->>'owner'=$2
 AND ($3::bigint IS NULL OR event_id<>$3))`, preparation.JobId, preparation.Owner, ignored).Scan(&held)
	return held, err
}

// Validates the original held event rather than treating an arbitrary event id
// or a decoded false value as an audited recovery.
func readRoundBaselineHeldEvent(ctx context.Context, tx server.PgTx, preparation *roundBaselinePreparation, eventId any, maximum int64) (int64, error) {
	var id int64
	var content []byte
	var digest, actor string
	err := tx.QueryRow(ctx, `SELECT event_id,payload_json,payload_sha256,actor_id FROM competition_job_event
 WHERE job_id=$1 AND event_type='round_baseline_preparation_held' AND payload_json->>'owner'=$2
 AND ($3::bigint IS NULL OR event_id=$3) ORDER BY event_id LIMIT 1`, preparation.JobId, preparation.Owner, eventId).Scan(&id, &content, &digest, &actor)
	if err != nil {
		return 0, err
	}
	var hold roundBaselineHeldEvent
	if decodeCompetitionPreparationJson(bytes.NewReader(content), maximum, &hold) != nil {
		return 0, ErrConflict
	}
	canonical, err := json.Marshal(map[string]any{"schema": hold.Schema, "owner": hold.Owner, "context_sha256": hold.ContextSha256, "hold": hold.Hold})
	if err != nil {
		return 0, err
	}
	hash := sha256.Sum256(canonical)
	if hex.EncodeToString(hash[:]) != digest || actor != preparation.Owner || hold.Schema != 1 ||
		hold.Owner != preparation.Owner || hold.ContextSha256 != preparation.ContextSha256 || hold.Hold.Stage != "producer_or_archive" {
		return 0, ErrConflict
	}
	return id, nil
}

// Derives current control provenance from one authenticated append-only event.
// The original reservation payload, owner and clock are never rewritten.
func applyRoundBaselineRecoveryInTx(ctx context.Context, tx server.PgTx, preparation *roundBaselinePreparation, maximum int64) error {
	var content []byte
	var digest, actor string
	var id int64
	var eventAt time.Time
	err := tx.QueryRow(ctx, `SELECT event_id,payload_json,payload_sha256,actor_id,event_at FROM competition_job_event
 WHERE job_id=$1 AND event_type='round_baseline_preparation_zero_sample_recovered' ORDER BY event_id LIMIT 1`, preparation.JobId).Scan(&id, &content, &digest, &actor, &eventAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var transition roundBaselineRecoveryTransition
	if decodeCompetitionPreparationJson(bytes.NewReader(content), maximum, &transition) != nil {
		return ErrConflict
	}
	canonical, err := json.Marshal(transition)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(canonical)
	if id <= 0 || transition.PreviousHoldId <= 0 || hex.EncodeToString(hash[:]) != digest || actor != preparation.Owner || transition.Schema != 1 ||
		transition.Kind != "sim-latency-round-baseline-zero-sample-recovered" ||
		transition.Owner != preparation.Owner || transition.RoundId != preparation.RoundId || transition.JobId != preparation.JobId ||
		transition.PreviousContextSha256 != preparation.ContextSha256 || transition.PreviousWorkerId != preparation.WorkerId ||
		transition.PreviousWorkerImage != preparation.WorkerImage || !transition.StartedAt.Equal(preparation.StartedAt) ||
		!transition.Deadline.Equal(preparation.Deadline) || !workerIdPattern.MatchString(transition.WorkerId) ||
		!imageDigestPattern.MatchString(transition.WorkerImage) || !sha256Pattern.MatchString(transition.ContextSha256) ||
		!sha256Pattern.MatchString(transition.AuthorizationSha256) || !sha256Pattern.MatchString(transition.ApprovalSha256) ||
		!sha256Pattern.MatchString(transition.EvidenceSha256) {
		return ErrConflict
	}
	if _, err := readRoundBaselineHeldEvent(ctx, tx, preparation, transition.PreviousHoldId, maximum); err != nil {
		return err
	}
	var repeated bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job_event WHERE job_id=$1
 AND event_type='round_baseline_preparation_zero_sample_recovered' AND event_id<>$2)`, preparation.JobId, id).Scan(&repeated); err != nil {
		return err
	}
	if repeated {
		return ErrConflict
	}
	transition.EventId = id
	clock := *preparation
	clock.Recovery = &transition
	if roundBaselinePreparationExecutionDeadline(&clock).IsZero() ||
		transition.ExecutionClock != "" && (!transition.ExecutionStartedAt.Equal(eventAt) || eventAt.Before(preparation.StartedAt)) {
		return ErrConflict
	}
	preparation.WorkerId, preparation.WorkerImage, preparation.ContextSha256 = transition.WorkerId, transition.WorkerImage, transition.ContextSha256
	preparation.Recovery = &transition
	return nil
}

// A queued, never-started original source can reconcile once. Only the explicit
// private clock capability permits a fresh bounded setup execution interval.
func recoverRoundBaselinePreparationInTx(ctx context.Context, tx server.PgTx, settings *Settings, original *roundBaselinePreparation, job *queuedJob, workerId, workerImage string, now time.Time) (*roundBaselinePreparation, error) {
	capability := settings.roundBaselinePreparation.Recovery
	if capability == nil || original.Recovery != nil || job == nil {
		return nil, errRoundBaselinePreparationHeld
	}
	a := capability.Authorization
	freshClock := a.ExecutionClock == roundBaselineRecoveredExecutionClock
	if a.Owner != original.Owner || a.RoundId != original.RoundId || a.JobId != original.JobId ||
		a.PreviousContextSha256 != original.ContextSha256 || a.PreviousWorkerImage != original.WorkerImage ||
		a.ReplacementWorkerImage != workerImage || now.Before(a.ApprovedAt) || a.ApprovedAt.Before(original.StartedAt) ||
		a.ExecutionClock != "" && !freshClock || !freshClock && !now.Before(original.Deadline) ||
		original.Deadline.Sub(original.StartedAt) != settings.roundBaselinePreparation.Settings.TotalTimeout ||
		job.JobId != original.JobId || job.RoundId != original.RoundId || job.State != "queued" || job.StartedAt != nil || job.AttemptCount != 0 ||
		job.PatchSha256 != original.PatchSha256 || job.PatchSha256 != capability.PatchSha256 || job.Round.ProvidersSha256 != capability.ProvidersSha256 ||
		job.Round.Canceled || job.Round.FinalizedAt != nil || job.Round.AutomationSealedAt != nil ||
		now.Before(job.Round.OpensAt) || job.SubmittedAt.Before(job.Round.OpensAt) || !job.SubmittedAt.Before(job.Round.ClosesAt) {
		return nil, errRoundBaselinePreparationHeld
	}
	frozen, err := settingsForFrozenRound(settings, &job.Round)
	if err != nil {
		return nil, err
	}
	if settings.AutomationPolicy.Enabled {
		if settings.automationRoundId != original.RoundId {
			return nil, ErrLeaseLost
		}
		if err := ensureAutomationRoundValidInTx(ctx, tx, original.RoundId); err != nil {
			return nil, err
		}
	}
	var baselineExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_round_baseline WHERE round_id=$1)`, original.RoundId).Scan(&baselineExists); err != nil {
		return nil, err
	}
	if baselineExists {
		return nil, errRoundBaselinePreparationHeld
	}
	job.WorkerImageDigest = original.WorkerImage
	oldContext, err := roundBaselinePreparationContext(frozen, job)
	if err != nil || oldContext != original.ContextSha256 {
		return nil, ErrConflict
	}
	holdId, err := readRoundBaselineHeldEvent(ctx, tx, original, nil, settings.roundBaselinePreparation.Settings.MaximumRecordBytes)
	if err != nil {
		return nil, err
	}
	job.WorkerImageDigest = workerImage
	newContext, err := roundBaselinePreparationContext(frozen, job)
	if err != nil {
		return nil, err
	}
	transition := roundBaselineRecoveryTransition{Schema: 1, Kind: "sim-latency-round-baseline-zero-sample-recovered",
		Owner: original.Owner, RoundId: original.RoundId, JobId: original.JobId, PreviousHoldId: holdId,
		PreviousContextSha256: original.ContextSha256, ContextSha256: newContext, PreviousWorkerId: original.WorkerId, WorkerId: workerId,
		PreviousWorkerImage: original.WorkerImage, WorkerImage: workerImage, StartedAt: original.StartedAt, Deadline: original.Deadline,
		AuthorizationSha256: capability.AuthorizationSha256, ApprovalSha256: a.Approval.Sha256, EvidenceSha256: a.Evidence.Sha256}
	if freshClock {
		executionStartedAt := now
		executionDeadline := now.Add(settings.roundBaselinePreparation.Settings.TotalTimeout)
		transition.ExecutionClock, transition.ExecutionStartedAt, transition.ExecutionDeadline = a.ExecutionClock, &executionStartedAt, &executionDeadline
	}
	appendEvent(ctx, tx, original.JobId, now, "round_baseline_preparation_zero_sample_recovered", original.Owner, transition)
	copy := *original
	if err := applyRoundBaselineRecoveryInTx(ctx, tx, &copy, settings.roundBaselinePreparation.Settings.MaximumRecordBytes); err != nil {
		return nil, err
	}
	held, err := roundBaselinePreparationHeldInTx(ctx, tx, &copy)
	if err != nil || held {
		return nil, errors.Join(err, errRoundBaselinePreparationHeld)
	}
	copy.SourceJob = job
	return &copy, nil
}

// Invalid or partial transition clocks return zero and therefore fail closed.
// Original reservation times remain immutable; omission preserves old JSON and
// the old execution clock exactly. Database reconstruction binds start to event_at.
func roundBaselinePreparationExecutionDeadline(preparation *roundBaselinePreparation) time.Time {
	if preparation == nil {
		return time.Time{}
	}
	recovery := preparation.Recovery
	if recovery == nil || recovery.ExecutionClock == "" && recovery.ExecutionStartedAt == nil && recovery.ExecutionDeadline == nil {
		return preparation.Deadline
	}
	if recovery.ExecutionClock != roundBaselineRecoveredExecutionClock || recovery.ExecutionStartedAt == nil || recovery.ExecutionDeadline == nil ||
		recovery.ExecutionStartedAt.IsZero() || recovery.ExecutionStartedAt.Before(preparation.StartedAt) ||
		!preparation.StartedAt.Before(preparation.Deadline) ||
		!recovery.ExecutionDeadline.Equal(recovery.ExecutionStartedAt.Add(preparation.Deadline.Sub(preparation.StartedAt))) {
		return time.Time{}
	}
	return *recovery.ExecutionDeadline
}

// Local/retained namespaces are fresh, while native receipt owner stays original.
func roundBaselinePreparationArtifactOwner(preparation *roundBaselinePreparation) string {
	if preparation.Recovery == nil {
		return preparation.Owner
	}
	return preparation.Owner + ".recovery." + strconv.FormatInt(preparation.Recovery.EventId, 10)
}

// A transition token also fences obsolete cleanup when context bytes coincide.
func roundBaselineRecoveryEventId(preparation *roundBaselinePreparation) int64 {
	if preparation.Recovery == nil {
		return 0
	}
	return preparation.Recovery.EventId
}

// READ COMMITTED must acquire the row lock BEFORE a separate fresh-snapshot
// transition read. A single UPDATE's waiting snapshot can miss a same-owner recovery.
func releaseRoundBaselinePreparationInTx(ctx context.Context, tx server.PgTx, preparation *roundBaselinePreparation, now time.Time) error {
	var slotId int
	if err := tx.QueryRow(ctx, `SELECT slot_id FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&slotId); err != nil {
		return err
	}
	var recovered any
	if preparation.Recovery != nil {
		recovered = preparation.Recovery.EventId
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM competition_job_event WHERE job_id=$1
 AND event_type='round_baseline_preparation_zero_sample_recovered' AND ($2::bigint IS NULL OR event_id<>$2))
 AND ($2::bigint IS NULL OR EXISTS(SELECT 1 FROM competition_job_event WHERE job_id=$1
 AND event_type='round_baseline_preparation_zero_sample_recovered' AND event_id=$2))`,
		preparation.JobId, recovered).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=NULL,job_id=NULL,lease_expires_at=NULL,heartbeat_at=$2
 WHERE slot_id=1 AND worker_id=$1 AND job_id IS NULL`, preparation.Owner, now)
	return err
}
