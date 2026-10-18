// Reconciles v2 epochs from durable state. Review and settlement are independent
// services; only reviewed source, exact runtime readiness, and drain gate progress.
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
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const competitionPreparationInterval = 16 * time.Hour

// Exposes the exact already-approved candidate to the trusted private source
// builder. Source approval comes from the persisted authenticated review.
type CompetitionReviewedSource struct {
	Review               model.CompetitionIntegrityReviewResult `json:"review"`
	Patch                []byte                                 `json:"patch"`
	Score                model.CompetitionScoreResult           `json:"score"`
	ScoreSha256          string                                 `json:"score_sha256"`
	SubmittedAt          time.Time                              `json:"submitted_at"`
	PatchPolicy          PatchPolicy                            `json:"patch_policy"`
	BaseSha              string                                 `json:"base_sha"`
	EvaluatedImageDigest string                                 `json:"evaluated_image_digest"`
}

// Source review cannot make an originally ineligible evaluation promotable.
// The score digest is the authenticated archived file, not reserialized JSON.
func reviewedSourceSealedEntry(review model.CompetitionIntegrityReviewResult, seal *model.CompetitionEvaluationSeal, patch []byte) (*model.CompetitionSealedEntry, error) {
	if seal == nil || seal.RoundId != review.RoundId || seal.BaselineSha256 != review.Request.BaselineSha256 {
		return nil, ErrConflict
	}
	patchDigest := sha256.Sum256(patch)
	for index := range seal.Entries {
		entry := &seal.Entries[index]
		if entry.JobId != review.Request.JobId {
			continue
		}
		if entry.State != "succeeded" || !entry.Eligible || model.CompetitionRewardEligibilityForMode(entry.Score, seal.BaselineRawScore, seal.TakeoverMargin, seal.Staging) != "" ||
			entry.PatchSha256 != review.Request.PatchSha256 || hex.EncodeToString(patchDigest[:]) != entry.PatchSha256 || entry.BaseSourceLockSha256 != review.Request.BaseSourceLockSha256 ||
			entry.ScoreSha256 != review.Request.ScoreSha256 || entry.ArtifactManifestSha256 != review.Request.ArtifactManifestSha256 {
			return nil, ErrConflict
		}
		return entry, nil
	}
	return nil, ErrConflict
}

// Reads genuine current source approval, scoped to the configured season.
// Later publication and activation independently recheck revocation races.
func (self PostgresStore) ReviewedSource(ctx context.Context, settings *Settings, reviewId server.Id) (result *CompetitionReviewedSource, err error) {
	if !settings.AutomationPolicy.Enabled {
		return nil, ErrAutomationDisabled
	}
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result = nil
			var reviewJson, sealJson, policyJson, patch []byte
			server.Raise(conn.QueryRow(ctx, `SELECT v.review_json,j.patch_bytes,s.seal_json,r.policy_json FROM competition_integrity_review v
		 JOIN competition_job j ON j.job_id=v.job_id JOIN competition_round r ON r.round_id=v.round_id JOIN competition_evaluation_seal s ON s.round_id=r.round_id
		 WHERE v.review_id=$1 AND r.competition_id=$2`, reviewId, settings.CompetitionId).Scan(&reviewJson, &patch, &sealJson, &policyJson))
			review := model.CompetitionIntegrityReviewResult{}
			server.Raise(json.Unmarshal(reviewJson, &review))
			policy, decodeErr := decodeRoundPolicySnapshot(policyJson)
			server.Raise(decodeErr)
			if policy.AutomationPolicy == nil || policy.AutomationPolicy.Staging != settings.AutomationPolicy.Staging || policy.AutomationPolicy.Environment != settings.AutomationPolicy.Environment ||
				policy.AutomationPolicy.SeasonId != settings.AutomationPolicy.SeasonId || review.Request.SourceDisposition != "approved" {
				panic(ErrConflict)
			}
			var invalidated bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=$1 AND invalidates_evaluation)`, review.RoundId).Scan(&invalidated))
			if invalidated {
				panic(ErrEvaluationInvalidated)
			}
			var latestReviewId server.Id
			server.Raise(conn.QueryRow(ctx, `SELECT review_id FROM competition_integrity_review WHERE round_id=$1 AND job_id=$2
		 AND review_json->'request'->>'source_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1`, review.RoundId, review.Request.JobId).Scan(&latestReviewId))
			if latestReviewId != reviewId {
				panic(ErrConflict)
			}
			var disposition string
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT review_json->'request'->>'reward_disposition' FROM competition_integrity_review WHERE round_id=$1 AND job_id=$2
		 AND review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1),'unchanged')`, review.RoundId, review.Request.JobId).Scan(&disposition))
			if disposition == "disqualified" {
				panic(ErrConflict)
			}
			var followsDisqualification bool
			server.Raise(conn.QueryRow(ctx, `SELECT v.review_cursor>COALESCE((SELECT max(d.review_cursor) FROM competition_integrity_review d
		 WHERE d.round_id=v.round_id AND d.job_id=v.job_id AND d.review_json->'request'->>'reward_disposition'='disqualified'),0)
		 FROM competition_integrity_review v WHERE v.review_id=$1`, reviewId).Scan(&followsDisqualification))
			if !followsDisqualification {
				panic(ErrConflict)
			}
			var seal model.CompetitionEvaluationSeal
			server.Raise(json.Unmarshal(sealJson, &seal))
			entry, entryErr := reviewedSourceSealedEntry(review, &seal, patch)
			server.Raise(entryErr)
			result = &CompetitionReviewedSource{Review: review, Patch: patch, Score: *entry.Score, ScoreSha256: entry.ScoreSha256, SubmittedAt: entry.SubmittedAt, PatchPolicy: policy.PatchPolicy, BaseSha: policy.BaseSha, EvaluatedImageDigest: policy.EvaluatorImageDigest}
		})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return result, err
}

// The trusted deployment adapter installs this exact envelope on every
// participating runtime. It contains no token, workload, patch, or seed.
type CompetitionRuntimeManifest struct {
	Staging                   bool      `json:"staging,omitempty"`
	StagingRoundWindowSeconds int       `json:"staging_round_window_seconds,omitempty"`
	StagingPreparationSeconds int       `json:"staging_preparation_seconds,omitempty"`
	Environment               string    `json:"environment"`
	CompetitionId             string    `json:"competition_id"`
	SeasonId                  string    `json:"season_id"`
	Generation                int64     `json:"generation"`
	TargetRoundId             server.Id `json:"target_round_id"`
	ManifestCommit            string    `json:"manifest_commit"`
	ManifestSha256            string    `json:"manifest_sha256"`
	Manifest                  []byte    `json:"manifest"`
	SourceLockSha256          string    `json:"source_lock_sha256"`
	EvaluatorImageDigest      string    `json:"evaluator_image_digest"`
	HostQualificationSha256   string    `json:"host_qualification_sha256"`
}

// Authenticates installed runtime content instead of accepting a caller's
// readiness boolean. Missing installation is an explicit source-not-ready gate.
func LoadCompetitionRuntimeManifest(settings *Settings, intent *model.CompetitionSourceActivation) error {
	if !sourceModeMatches(settings, intent) ||
		intent.Environment != settings.AutomationPolicy.Environment ||
		intent.CompetitionId != settings.CompetitionId || intent.SeasonId != settings.AutomationPolicy.SeasonId || intent.TargetRoundId == (server.Id{}) {
		return errors.New("source_not_ready: runtime manifest scope is invalid")
	}
	directory := os.Getenv("SIM_LATENCY_RUNTIME_MANIFEST_DIRECTORY")
	if !filepath.IsAbs(directory) {
		return errors.New("source_not_ready: exact runtime manifest installer is not configured")
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 || directoryInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("source_not_ready: runtime manifest directory is unsafe")
	}
	path := filepath.Join(directory, fmt.Sprintf("generation-%d.json", intent.Generation))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Size() <= 0 || info.Size() > 2*1024*1024 {
		return errors.New("source_not_ready: exact runtime manifest is absent or unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return errors.New("source_not_ready: runtime manifest changed while opening")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 2*1024*1024+1))
	decoder.DisallowUnknownFields()
	var installed CompetitionRuntimeManifest
	if err := decoder.Decode(&installed); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("source_not_ready: runtime manifest has trailing content")
	}
	digest := sha256.Sum256(installed.Manifest)
	lockDigest, err := model.CompetitionSourceLockDigest(intent.Commits)
	if err != nil || lockDigest != intent.SourceLockSha256 ||
		installed.Staging != intent.Staging || installed.StagingRoundWindowSeconds != intent.StagingRoundWindowSeconds || installed.StagingPreparationSeconds != intent.StagingPreparationSeconds ||
		installed.Environment != intent.Environment || installed.CompetitionId != intent.CompetitionId || installed.SeasonId != intent.SeasonId ||
		installed.Generation != intent.Generation || installed.TargetRoundId != intent.TargetRoundId || installed.ManifestCommit != intent.ManifestCommit ||
		installed.ManifestSha256 != intent.ManifestSha256 || hex.EncodeToString(digest[:]) != intent.ManifestSha256 ||
		!bytes.Equal(installed.Manifest, intent.Manifest) || installed.SourceLockSha256 != intent.SourceLockSha256 ||
		installed.EvaluatorImageDigest != intent.EvaluatorImageDigest || installed.HostQualificationSha256 != intent.HostQualificationSha256 {
		return errors.New("source_not_ready: installed runtime manifest does not match the immutable source intent")
	}
	return nil
}

// Carries only authenticated durable state; the live reward winner is never a
// source choice. Pending source approvals can be included only by ChooseSource.
type CompetitionSupervisorSnapshot struct {
	Round         *model.CompetitionRoundResult
	Seal          *model.CompetitionEvaluationSeal
	CurrentSource *model.CompetitionSourceActivation
	Transition    *model.CompetitionSupervisorTransition
	Intent        *model.CompetitionSourceActivation
}

// External work is outside database transactions. Every mutating method must
// recheck the supplied fence; source methods separately enforce source authority.
type CompetitionSupervisorDriver interface {
	Acquire(context.Context, string, time.Duration) (*model.CompetitionSupervisorLease, error)
	Renew(context.Context, model.CompetitionSupervisorLease, time.Duration) (*model.CompetitionSupervisorLease, error)
	Snapshot(context.Context, model.CompetitionSupervisorLease) (*CompetitionSupervisorSnapshot, error)
	SaveTransition(context.Context, model.CompetitionSupervisorLease, model.CompetitionSupervisorTransition) (*model.CompetitionSupervisorTransition, error)
	RunWorker(context.Context, model.CompetitionSupervisorLease, model.CompetitionRoundResult) error
	Seal(context.Context, model.CompetitionSupervisorLease, server.Id) error
	ChooseSource(context.Context, model.CompetitionSupervisorLease, *CompetitionSupervisorSnapshot, model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error)
	PublishSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error
	PrepareSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error
	ActivateSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error
	CreateRound(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation, time.Time) (*model.CompetitionRoundResult, error)
}

// Runs one season without interactive review. A fresh lease protects every
// reconciliation; its renewal owner cancels in-flight external work on fence loss.
type CompetitionSupervisor struct {
	Enabled       bool
	OwnerId       string
	Driver        CompetitionSupervisorDriver
	Now           func() time.Time
	PollInterval  time.Duration
	LeaseDuration time.Duration
}

// Deployment-owned callbacks are pinned by the executable harness. They may
// not acknowledge their own source approval or manufacture runtime readiness.
type CompetitionSupervisorHooks struct {
	RunWorker      func(context.Context, model.CompetitionSupervisorLease, model.CompetitionRoundResult) error
	ChooseSource   func(context.Context, model.CompetitionSupervisorLease, *CompetitionSupervisorSnapshot, model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error)
	PublishSource  func(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error
	PrepareSource  func(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error
	FallbackSource func(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation, string) (bool, error)
}

// A failed promotion may be abandoned only when independently proven to have
// published no refs. Absence of this integration preserves the blocked intent.
type competitionSupervisorFallbackDriver interface {
	FallbackSource(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation, string) (bool, error)
}

// The publisher proves external non-publication before the store appends a
// one-way abandonment. Neither the state machine nor a failure implies proof.
func (self *PostgresCompetitionSupervisorDriver) FallbackSource(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation, reason string) (bool, error) {
	if self.Hooks.FallbackSource == nil {
		return false, nil
	}
	return self.Hooks.FallbackSource(ctx, lease, intent, reason)
}

// Binds the state machine to the authoritative database, not a shell ledger.
type PostgresCompetitionSupervisorDriver struct {
	Settings *Settings
	Store    PostgresStore
	Hooks    CompetitionSupervisorHooks
}

// Acquires a durable fence scoped to this environment and season.
func (self *PostgresCompetitionSupervisorDriver) Acquire(ctx context.Context, owner string, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	return self.Store.AcquireAutomationLease(ctx, self.Settings, owner, ttl)
}

// Extends only the exact live fence, never reviving an expired owner.
func (self *PostgresCompetitionSupervisorDriver) Renew(ctx context.Context, lease model.CompetitionSupervisorLease, ttl time.Duration) (*model.CompetitionSupervisorLease, error) {
	return self.Store.RenewAutomationLease(ctx, self.Settings, lease, ttl)
}

// Reconstructs every checkpoint after a crash, including a created round whose
// final journal update was lost. The current round always wins over local state.
func (self *PostgresCompetitionSupervisorDriver) Snapshot(ctx context.Context, lease model.CompetitionSupervisorLease) (*CompetitionSupervisorSnapshot, error) {
	snapshot := &CompetitionSupervisorSnapshot{}
	round, err := self.Store.CurrentAutomationRound(ctx, self.Settings)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	targetEpoch := 1
	if round != nil {
		if !roundUsesAutomation(round) {
			return nil, ErrUnsupportedDecisionMode
		}
		copy := round.RoundResult
		copy.DecisionMode = model.CompetitionDecisionModeV2
		snapshot.Round = &copy
		targetEpoch = round.Epoch + 1
		snapshot.Seal, err = self.Store.AutomationSeal(ctx, self.Settings, round.RoundId)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	snapshot.CurrentSource, err = self.Store.CurrentSourceActivation(ctx, self.Settings)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if targetEpoch <= 6 {
		snapshot.Transition, err = self.Store.SupervisorTransition(ctx, self.Settings, targetEpoch)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if snapshot.Transition != nil && snapshot.Transition.SourceActivationGeneration != 0 {
			snapshot.Intent, err = self.Store.SourceActivation(ctx, self.Settings, snapshot.Transition.SourceActivationGeneration)
			if err != nil {
				return nil, err
			}
		}
	}
	return snapshot, nil
}

// Appends one expected-revision checkpoint under the current fence.
func (self *PostgresCompetitionSupervisorDriver) SaveTransition(ctx context.Context, lease model.CompetitionSupervisorLease, transition model.CompetitionSupervisorTransition) (*model.CompetitionSupervisorTransition, error) {
	return self.Store.RecordSupervisorTransition(ctx, self.Settings, lease, transition)
}

// Starts/resumes the existing one-shot singleton evaluator; no extra slot is added.
func (self *PostgresCompetitionSupervisorDriver) RunWorker(ctx context.Context, lease model.CompetitionSupervisorLease, round model.CompetitionRoundResult) error {
	if self.Hooks.RunWorker == nil {
		return errors.New("worker launch integration is not configured")
	}
	return self.Hooks.RunWorker(ctx, lease, round)
}

// Seals and elects idempotently; the store independently rechecks full drain.
func (self *PostgresCompetitionSupervisorDriver) Seal(ctx context.Context, lease model.CompetitionSupervisorLease, roundId server.Id) error {
	if _, err := self.Renew(ctx, lease, time.Minute); err != nil {
		return err
	}
	_, err := self.Store.SealAndElect(ctx, self.Settings, roundId)
	return err
}

// Persists the selected intent before the first publication side effect.
func (self *PostgresCompetitionSupervisorDriver) ChooseSource(ctx context.Context, lease model.CompetitionSupervisorLease, snapshot *CompetitionSupervisorSnapshot, transition model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error) {
	if self.Hooks.ChooseSource == nil {
		return nil, errors.New("reviewed source choice integration is not configured")
	}
	intent, err := self.Hooks.ChooseSource(ctx, lease, snapshot, transition)
	if err != nil {
		return nil, err
	}
	if intent.State == "active" {
		return intent, nil
	}
	return self.Store.RecordSourceIntent(ctx, self.Settings, lease, *intent)
}

// Keeps Git side effects outside database transactions and activation last.
func (self *PostgresCompetitionSupervisorDriver) PublishSource(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	if intent.State == "active" {
		return nil
	}
	if self.Hooks.PublishSource == nil {
		return errors.New("source publication integration is not configured")
	}
	return self.Hooks.PublishSource(ctx, lease, intent)
}

// Requires real runtime acknowledgments, production qualification and archive
// availability; a no-op hook cannot satisfy the subsequent store activation guard.
func (self *PostgresCompetitionSupervisorDriver) PrepareSource(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	if self.Hooks.PrepareSource == nil {
		return errors.New("runtime source propagation integration is not configured")
	}
	if err := self.Hooks.PrepareSource(ctx, lease, intent); err != nil {
		return err
	}
	return self.Store.SourcePreparationReady(ctx, self.Settings, intent.Generation)
}

// Rechecks current source authority and independently recorded runtime receipts.
func (self *PostgresCompetitionSupervisorDriver) ActivateSource(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	_, err := self.Store.ActivateSource(ctx, self.Settings, lease, intent.Generation, intent.ExpectedGeneration)
	return err
}

// Creates exactly one full seven-day successor under the same durable fence.
func (self *PostgresCompetitionSupervisorDriver) CreateRound(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation, opensAt time.Time) (*model.CompetitionRoundResult, error) {
	round, err := self.Store.CreateAutomationRound(ctx, self.Settings, lease, intent.Generation, opensAt)
	if err != nil {
		return nil, err
	}
	return round, nil
}

// Performs one durable step. Retryable failures retain a visible checkpoint;
// a failed preparation never opens a round or rewrites a committed schedule.
func (self *CompetitionSupervisor) Reconcile(ctx context.Context) (state string, err error) {
	if !self.Enabled {
		return "disabled", errors.New("competition automation is disabled")
	}
	if self.Driver == nil || self.OwnerId == "" {
		return "blocked", errors.New("supervisor driver and owner are required")
	}
	leaseDuration := self.LeaseDuration
	if leaseDuration == 0 {
		leaseDuration = time.Minute
	}
	if leaseDuration < 15*time.Second || leaseDuration > 5*time.Minute {
		return "blocked", errors.New("supervisor lease must be 15 seconds through five minutes")
	}
	lease, err := self.Driver.Acquire(ctx, self.OwnerId, leaseDuration)
	if err != nil {
		return "lease_unavailable", err
	}
	operationCtx, cancel := context.WithCancel(ctx)
	renewed := make(chan error, 1)
	go func() {
		current := *lease
		for {
			select {
			case <-operationCtx.Done():
				renewed <- nil
				return
			case <-time.After(leaseDuration / 3):
				next, renewErr := self.Driver.Renew(operationCtx, current, leaseDuration)
				if renewErr != nil {
					cancel()
					renewed <- renewErr
					return
				}
				current = *next
			}
		}
	}()
	defer func() {
		cancel()
		if renewErr := <-renewed; renewErr != nil {
			state, err = "lease_lost", renewErr
		}
	}()
	snapshot, err := self.Driver.Snapshot(operationCtx, *lease)
	if err != nil {
		return "state_unavailable", err
	}
	now := time.Now().UTC()
	if self.Now != nil {
		now = self.Now().UTC()
	}
	if snapshot.Round != nil && snapshot.Round.Status == "canceled" {
		return "incident_hold", errors.New("canceled round requires an authorized incident resolution")
	}
	if snapshot.Round != nil && snapshot.Seal == nil {
		if snapshot.Round.DecisionMode != model.CompetitionDecisionModeV2 {
			return "legacy_mode", errors.New("supervisor cannot convert or advance a legacy round")
		}
		if err := self.Driver.RunWorker(operationCtx, *lease, *snapshot.Round); err != nil {
			return "evaluation_blocked", err
		}
		if now.Before(snapshot.Round.ClosesAt) {
			return "evaluating", nil
		}
		if err := self.Driver.Seal(operationCtx, *lease, snapshot.Round.RoundId); err != nil {
			return "draining", err
		}
		return "sealed", nil
	}
	targetEpoch := 1
	choiceAt := now
	if snapshot.Round != nil {
		if snapshot.Round.Epoch >= 6 {
			return "season_complete", nil
		}
		targetEpoch = snapshot.Round.Epoch + 1
		choiceAt = snapshot.Seal.SourceChoiceAt
	}
	transition := snapshot.Transition
	if transition == nil {
		transition, err = self.Driver.SaveTransition(operationCtx, *lease, model.CompetitionSupervisorTransition{
			TargetEpoch: targetEpoch, ExpectedRevision: 0, State: "source_choice_pending", SourceChoiceAt: choiceAt,
		})
		if err != nil {
			return "checkpoint_blocked", err
		}
		return transition.State, nil
	}
	if transition.TargetEpoch != targetEpoch {
		return "blocked", errors.New("supervisor checkpoint belongs to another epoch")
	}
	checkpoint := func(nextState, reason string, intent *model.CompetitionSourceActivation, opensAt time.Time) (string, error) {
		next := *transition
		next.ExpectedRevision = transition.Revision
		next.State, next.Reason = nextState, reason
		if intent != nil {
			next.SourceActivationGeneration = intent.Generation
			next.PreparationStartedAt = intent.PreparedAt
		}
		if !opensAt.IsZero() {
			next.OpensAt = opensAt
		}
		result, saveErr := self.Driver.SaveTransition(operationCtx, *lease, next)
		if saveErr != nil {
			return "checkpoint_blocked", saveErr
		}
		return result.State, nil
	}
	fallback := func(reason string) (bool, string, error) {
		if snapshot.Intent == nil || snapshot.Intent.Kind != "reviewed_source_promotion" {
			return false, "", nil
		}
		driver, ok := self.Driver.(competitionSupervisorFallbackDriver)
		if !ok {
			return false, "", nil
		}
		abandoned, fallbackErr := driver.FallbackSource(operationCtx, *lease, snapshot.Intent, reason)
		if fallbackErr != nil || !abandoned {
			return false, "", fallbackErr
		}
		next := *transition
		next.ExpectedRevision, next.State, next.Reason = transition.Revision, "source_choice_pending", "safe_carry_forward_after_unpublished_promotion"
		next.SourceActivationGeneration, next.PreparationStartedAt, next.OpensAt = 0, time.Time{}, time.Time{}
		result, saveErr := self.Driver.SaveTransition(operationCtx, *lease, next)
		if saveErr != nil {
			return true, "checkpoint_blocked", saveErr
		}
		return true, result.State, nil
	}
	switch transition.State {
	case "source_choice_pending":
		if now.Before(transition.SourceChoiceAt) {
			return "source_choice_pending", nil
		}
		intent, choiceErr := self.Driver.ChooseSource(operationCtx, *lease, snapshot, *transition)
		if choiceErr != nil {
			return "source_choice_blocked", choiceErr
		}
		if intent == nil || intent.TargetEpoch != targetEpoch {
			return "source_choice_blocked", errors.New("source choice returned the wrong target epoch")
		}
		return checkpoint("source_publication_pending", "", intent, time.Time{})
	case "source_publication_pending", "publication_blocked":
		if snapshot.Intent == nil {
			return "publication_blocked", errors.New("durable source intent is missing")
		}
		if publishErr := self.Driver.PublishSource(operationCtx, *lease, snapshot.Intent); publishErr != nil {
			if abandoned, state, fallbackErr := fallback("publication_failed"); abandoned {
				return state, fallbackErr
			}
			state, checkpointErr := checkpoint("publication_blocked", "source publication unavailable; retry exact intent", snapshot.Intent, time.Time{})
			if checkpointErr != nil {
				return state, checkpointErr
			}
			return state, publishErr
		}
		return checkpoint("preparation_pending", "", snapshot.Intent, time.Time{})
	case "preparation_pending", "preparation_blocked":
		if snapshot.Intent == nil {
			return "preparation_blocked", errors.New("durable source intent is missing")
		}
		if prepareErr := self.Driver.PrepareSource(operationCtx, *lease, snapshot.Intent); prepareErr != nil {
			if abandoned, state, fallbackErr := fallback("readiness_failed"); abandoned {
				return state, fallbackErr
			}
			state, checkpointErr := checkpoint("preparation_blocked", "runtime propagation or production qualification unavailable", snapshot.Intent, time.Time{})
			if checkpointErr != nil {
				return state, checkpointErr
			}
			return state, prepareErr
		}
		if now.Before(snapshot.Intent.PreparedAt.Add(sourcePreparationInterval(snapshot.Intent))) {
			return checkpoint("preparation_ready", "", snapshot.Intent, time.Time{})
		}
		if activationErr := self.Driver.ActivateSource(operationCtx, *lease, snapshot.Intent); activationErr != nil {
			return "activation_blocked", activationErr
		}
		return checkpoint("source_active", "", snapshot.Intent, time.Time{})
	case "preparation_ready":
		if snapshot.Intent == nil {
			return "preparation_blocked", errors.New("durable source intent is missing")
		}
		if now.Before(snapshot.Intent.PreparedAt.Add(sourcePreparationInterval(snapshot.Intent))) {
			return "preparation_ready", nil
		}
		if activationErr := self.Driver.ActivateSource(operationCtx, *lease, snapshot.Intent); activationErr != nil {
			return "activation_blocked", activationErr
		}
		return checkpoint("source_active", "", snapshot.Intent, time.Time{})
	case "source_active":
		if snapshot.Intent == nil {
			return "activation_blocked", errors.New("durable source intent is missing")
		}
		// Open only after readiness, with the full seven-day window. The time
		// becomes immutable when CreateRound commits; slow prep never shortens it.
		opensAt := now
		round, createErr := self.Driver.CreateRound(operationCtx, *lease, snapshot.Intent, opensAt)
		if createErr != nil {
			return "round_creation_blocked", createErr
		}
		next := *transition
		next.ExpectedRevision, next.State, next.RoundId, next.OpensAt = transition.Revision, "round_created", &round.RoundId, round.OpensAt
		result, err := self.Driver.SaveTransition(operationCtx, *lease, next)
		if err != nil {
			return "checkpoint_blocked", err
		}
		return result.State, nil
	case "round_created":
		return "round_created", nil
	default:
		return "blocked", fmt.Errorf("unsupported durable supervisor phase %q", transition.State)
	}
}

// Rate-limits both stable and failing states; cancellation joins every renewal
// owner. Report receives redacted states, never unpublished evaluation details.
func (self *CompetitionSupervisor) Run(ctx context.Context, report func(string, error)) error {
	interval := self.PollInterval
	if interval == 0 {
		interval = 15 * time.Second
	}
	if interval < time.Second {
		return errors.New("supervisor poll interval must be at least one second")
	}
	for {
		state, err := self.Reconcile(ctx)
		if report != nil {
			report(state, err)
		}
		if state == "disabled" {
			return err
		}
		if state == "season_complete" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
