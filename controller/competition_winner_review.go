// The opt-in frozen flow reviews only the published elected winner. Pending,
// unavailable or expired review never authorizes a different source candidate.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var ErrWinnerReviewPending = errors.New("elected winner review is pending; successor progression is blocked")
var ErrWinnerReviewTimedOut = errors.New("elected winner review deadline expired; operator attention is required")

// Authenticated state is reloaded after every broker invocation and before
// source publication/activation. An approval is not a different source choice.
type CompetitionWinnerReviewState struct {
	Policy           model.CompetitionAutomationPolicy       `json:"policy"`
	CurrentDecision  model.CompetitionRewardDecision         `json:"current_decision"`
	ApprovedReviewId *server.Id                              `json:"approved_review_id,omitempty"`
	ApprovedReview   *model.CompetitionIntegrityReviewResult `json:"approved_review,omitempty"`
}

// Trusted broker input binds the entire elected decision and durable deadline.
// Reviewer identity metadata is a pinned broker claim, not model attestation.
type CompetitionWinnerReviewRequest struct {
	RoundId              server.Id                          `json:"round_id"`
	JobId                server.Id                          `json:"job_id"`
	SealSha256           string                             `json:"seal_sha256"`
	RewardRevision       int                                `json:"reward_revision"`
	RewardDecisionSha256 string                             `json:"reward_decision_sha256"`
	CriteriaVersion      string                             `json:"criteria_version"`
	ReviewerModel        string                             `json:"reviewer_model"`
	ReviewerReasoning    string                             `json:"reviewer_reasoning"`
	StartedAt            time.Time                          `json:"started_at"`
	Deadline             time.Time                          `json:"deadline"`
	CurrentSource        *model.CompetitionSourceActivation `json:"current_source,omitempty"`
	Policy               model.CompetitionAutomationPolicy  `json:"policy"`
}

// Optional driver capability leaves historical supervisors and their hooks
// unchanged; an adopted policy without this capability fails closed.
type competitionSupervisorWinnerReviewDriver interface {
	ReviewWinner(context.Context, model.CompetitionSupervisorLease, CompetitionWinnerReviewRequest) (*CompetitionWinnerReviewState, error)
}

// Every new winner gets one durable bounded review opportunity. A process
// restart, infrastructure retry or unchanged decision cannot reset its clock.
func (self *CompetitionSupervisor) reconcileWinnerReview(ctx context.Context, lease model.CompetitionSupervisorLease, snapshot *CompetitionSupervisorSnapshot, transition *model.CompetitionSupervisorTransition, now time.Time) (*model.CompetitionSupervisorTransition, string, error, bool) {
	state := snapshot.WinnerReview
	if snapshot.Round == nil || snapshot.Seal == nil || state == nil || state.Policy.WinnerReviewPolicy != model.CompetitionWinnerReviewPolicy || model.ValidateCompetitionAutomationPolicy(state.Policy) != nil {
		return transition, "winner_review_blocked", ErrConflict, true
	}
	if now.Before(snapshot.Seal.PublicationAt) {
		return transition, "awaiting_publication", nil, true
	}
	decision := state.CurrentDecision
	save := func(nextState, reason string, checkpoint *model.CompetitionWinnerReviewCheckpoint) (*model.CompetitionSupervisorTransition, string, error, bool) {
		next := *transition
		next.ExpectedRevision, next.State, next.Reason, next.WinnerReview = transition.Revision, nextState, reason, checkpoint
		stored, err := self.Driver.SaveTransition(ctx, lease, next)
		if err != nil {
			return transition, "checkpoint_blocked", err, true
		}
		return stored, nextState, nil, true
	}
	block := func(reason string, failure error) (*model.CompetitionSupervisorTransition, string, error, bool) {
		if transition.State == "winner_review_blocked" && transition.Reason == reason {
			return transition, "winner_review_blocked", failure, true
		}
		stored, state, err, _ := save("winner_review_blocked", reason, transition.WinnerReview)
		if err != nil {
			return stored, state, err, true
		}
		return stored, "winner_review_blocked", failure, true
	}
	if decision.SelectedJobId == nil {
		if snapshot.Intent != nil && snapshot.Intent.Kind != "reviewed_source_carry_forward" {
			return block("winner_revoked_during_source_preparation", ErrConflict)
		}
		if snapshot.Round.Epoch >= 6 {
			if transition.State != "complete" {
				return save("complete", "no_reward_winner", nil)
			}
			return transition, "season_complete", nil, true
		}
		if transition.State == "winner_review_pending" || transition.State == "winner_review_blocked" {
			return save("source_choice_pending", "no_reward_winner", nil)
		}
		return transition, "", nil, false
	}
	checkpoint := transition.WinnerReview
	if state.ApprovedReviewId != nil && state.ApprovedReview != nil {
		if checkpoint == nil || checkpoint.JobId != *decision.SelectedJobId || checkpoint.RoundId != snapshot.Round.RoundId ||
			state.ApprovedReview.Request.ExpectedDecisionSha256 != checkpoint.RewardDecisionSha256 || state.ApprovedReview.Request.ExpectedRevision != checkpoint.RewardRevision ||
			state.ApprovedReview.CreatedAt.Before(checkpoint.StartedAt) || state.ApprovedReview.CreatedAt.After(checkpoint.Deadline) {
			return block("winner_review_identity_conflict", ErrConflict)
		}
		if snapshot.Round.Epoch >= 6 {
			if transition.State != "complete" {
				return save("complete", "elected_winner_approved", checkpoint)
			}
			return transition, "season_complete", nil, true
		}
		if transition.State == "winner_review_pending" || transition.State == "winner_review_blocked" {
			return save("source_choice_pending", "elected_winner_approved", checkpoint)
		}
		return transition, "", nil, false
	}
	if snapshot.Intent != nil {
		return block("source_approval_revoked_requires_recovery", errors.New("selected source approval was revoked; published-source recovery requires operator attention"))
	}
	if checkpoint == nil || checkpoint.JobId != *decision.SelectedJobId || checkpoint.RewardDecisionSha256 != decision.DecisionSha256 || checkpoint.RewardRevision != decision.Revision {
		return save("winner_review_pending", "review_current_elected_winner", &model.CompetitionWinnerReviewCheckpoint{
			RoundId: snapshot.Round.RoundId, JobId: *decision.SelectedJobId, RewardRevision: decision.Revision, RewardDecisionSha256: decision.DecisionSha256,
			StartedAt: now, Deadline: now.Add(time.Duration(state.Policy.WinnerReviewTimeoutSeconds) * time.Second),
		})
	}
	if !now.Before(checkpoint.Deadline) {
		if transition.State != "winner_review_blocked" {
			stored, _, err, _ := save("winner_review_blocked", "review_deadline_expired_support_required", checkpoint)
			if err != nil {
				return stored, "checkpoint_blocked", err, true
			}
			transition = stored
		}
		return transition, "winner_review_blocked", ErrWinnerReviewTimedOut, true
	}
	driver, ok := self.Driver.(competitionSupervisorWinnerReviewDriver)
	if !ok {
		return block("reviewer_integration_missing", errors.New("elected-winner reviewer integration is unavailable"))
	}
	request := CompetitionWinnerReviewRequest{RoundId: snapshot.Round.RoundId, JobId: checkpoint.JobId, SealSha256: snapshot.Seal.SealSha256,
		RewardRevision: checkpoint.RewardRevision, RewardDecisionSha256: checkpoint.RewardDecisionSha256, CriteriaVersion: state.Policy.ReviewCriteriaVersion,
		ReviewerModel: model.CompetitionWinnerReviewModel, ReviewerReasoning: model.CompetitionWinnerReviewReasoning,
		StartedAt: checkpoint.StartedAt, Deadline: checkpoint.Deadline, CurrentSource: snapshot.CurrentSource, Policy: state.Policy}
	reviewed, err := driver.ReviewWinner(ctx, lease, request)
	if err != nil {
		stored, _, checkpointErr, _ := save("winner_review_blocked", "qualified_review_unavailable_support_required", checkpoint)
		if checkpointErr != nil {
			return stored, "checkpoint_blocked", checkpointErr, true
		}
		return stored, "winner_review_blocked", err, true
	}
	if reviewed == nil {
		return block("reviewer_did_not_persist_judgment", ErrWinnerReviewPending)
	}
	// Reconcile reloads the official decision/review on the next pass. Never
	// select source using only a broker's claimed success or stale snapshot.
	return transition, "winner_review_recorded", nil, true
}

// Projects only the latest affirmative review of the current elected job.
// Independent approvals of another candidate never satisfy this boundary.
func winnerReviewState(result *model.CompetitionV2RoundResult, policy model.CompetitionAutomationPolicy) (*CompetitionWinnerReviewState, error) {
	if result == nil || len(result.Decisions) == 0 {
		return nil, ErrReviewNotReady
	}
	state := &CompetitionWinnerReviewState{Policy: policy, CurrentDecision: result.Decisions[len(result.Decisions)-1]}
	if state.CurrentDecision.SelectedJobId == nil {
		return state, nil
	}
	var reward, source *model.CompetitionIntegrityReviewResult
	for i := range result.IntegrityReviews {
		review := &result.IntegrityReviews[i]
		if review.Request.JobId != *state.CurrentDecision.SelectedJobId {
			continue
		}
		if review.Request.RewardDisposition != "unchanged" {
			reward = review
		}
		if review.Request.SourceDisposition != "unchanged" {
			source = review
		}
	}
	if reward != nil && source != nil && reward.ReviewId == source.ReviewId &&
		(reward.Request.RewardDisposition == "approved" || reward.Request.RewardDisposition == "restored") && source.Request.SourceDisposition == "approved" &&
		source.Request.ReviewerModel == model.CompetitionWinnerReviewModel && source.Request.ReviewerReasoning == model.CompetitionWinnerReviewReasoning && source.Request.ExpectedDecisionSha256 == state.CurrentDecision.DecisionSha256 {
		state.ApprovedReview, state.ApprovedReviewId = source, &source.ReviewId
	}
	return state, nil
}

// Reads authoritative review/decision state in one scope transaction. Nothing
// is kept locked while the model broker or build broker runs.
func (self PostgresStore) WinnerReviewState(ctx context.Context, settings *Settings, roundId server.Id) (state *CompetitionWinnerReviewState, err error) {
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			_, policy, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, roundId))
			result := automationResultInTx(ctx, tx, roundId)
			var stateErr error
			state, stateErr = winnerReviewState(result, *policy.AutomationPolicy)
			server.Raise(stateErr)
		})
	})
	return state, err
}

// A successful process exit alone is never an approval. The actual scoped
// review API must have persisted a valid review or a replacement decision.
func (self *PostgresCompetitionSupervisorDriver) ReviewWinner(ctx context.Context, lease model.CompetitionSupervisorLease, request CompetitionWinnerReviewRequest) (*CompetitionWinnerReviewState, error) {
	if _, err := self.Renew(ctx, lease, time.Minute); err != nil {
		return nil, err
	}
	state, err := self.Store.WinnerReviewState(ctx, self.Settings, request.RoundId)
	if err != nil {
		return nil, err
	}
	if state.CurrentDecision.DecisionSha256 != request.RewardDecisionSha256 || state.CurrentDecision.SelectedJobId == nil || *state.CurrentDecision.SelectedJobId != request.JobId {
		return nil, ErrConflict
	}
	if self.Hooks.ReviewWinner == nil {
		return nil, errors.New("qualified elected-winner review broker is not configured")
	}
	reviewCtx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	if err := self.Hooks.ReviewWinner(reviewCtx, lease, request); err != nil {
		return nil, err
	}
	state, err = self.Store.WinnerReviewState(ctx, self.Settings, request.RoundId)
	if err == nil && state.CurrentDecision.DecisionSha256 == request.RewardDecisionSha256 && state.ApprovedReviewId == nil {
		return nil, ErrWinnerReviewPending
	}
	return state, err
}

// Reviews cannot be applied to a different revision, before publication, or
// after their durable deadline. The reviewer cannot mint its own time window.
func validateWinnerReviewSubmissionInTx(ctx context.Context, tx server.PgTx, result *model.CompetitionV2RoundResult, request model.CompetitionIntegrityReviewRequest, now time.Time) error {
	if result == nil || len(result.Decisions) == 0 || now.Before(result.Seal.PublicationAt) {
		return ErrReviewNotReady
	}
	decision := result.Decisions[len(result.Decisions)-1]
	if request.ExpectedDecisionSha256 != decision.DecisionSha256 {
		return ErrConflict
	}
	// Retrospective appeals retain their separate authenticated authority. A
	// restored recipient receives a fresh closing review of the new decision.
	if request.RewardDisposition == "restored" {
		return nil
	}
	if decision.SelectedJobId == nil || request.JobId != *decision.SelectedJobId {
		return ErrConflict
	}
	var content []byte
	if err := tx.QueryRow(ctx, `SELECT transition_json FROM competition_supervisor_transition WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 ORDER BY revision DESC LIMIT 1`, result.Seal.Environment, result.Seal.CompetitionId, result.Seal.SeasonId, result.Seal.Epoch+1).Scan(&content); err != nil {
		return err
	}
	var transition model.CompetitionSupervisorTransition
	if err := json.Unmarshal(content, &transition); err != nil {
		return err
	}
	checkpoint := transition.WinnerReview
	if checkpoint == nil || checkpoint.RoundId != result.Seal.RoundId || checkpoint.JobId != request.JobId || checkpoint.RewardRevision != request.ExpectedRevision || checkpoint.RewardDecisionSha256 != request.ExpectedDecisionSha256 || now.Before(checkpoint.StartedAt) || !now.Before(checkpoint.Deadline) {
		return ErrWinnerReviewTimedOut
	}
	return nil
}

// Source publication and activation recheck this under the same scope lock.
// Pending review, unrelated candidates, and fallback while a winner exists fail.
func validateWinnerSourceInTx(ctx context.Context, tx server.PgTx, settings *Settings, intent *model.CompetitionSourceActivation, predecessorId server.Id) error {
	var policyBytes []byte
	if err := tx.QueryRow(ctx, `SELECT policy_json FROM competition_round WHERE round_id=$1`, predecessorId).Scan(&policyBytes); err != nil {
		return err
	}
	policy, err := decodeRoundPolicySnapshot(policyBytes)
	if err != nil || policy.AutomationPolicy == nil {
		return ErrConflict
	}
	if policy.AutomationPolicy.WinnerReviewPolicy == "" {
		if intent.WinnerReviewPolicy != "" || intent.WinnerReview != nil {
			return ErrConflict
		}
		return nil
	}
	if intent.WinnerReviewPolicy != model.CompetitionWinnerReviewPolicy || policy.AutomationPolicy.WinnerReviewPolicy != intent.WinnerReviewPolicy {
		return ErrConflict
	}
	result := automationResultInTx(ctx, tx, predecessorId)
	state, err := winnerReviewState(result, *policy.AutomationPolicy)
	if err != nil {
		return err
	}
	if intent.RewardDecisionDigest != state.CurrentDecision.DecisionSha256 {
		return ErrConflict
	}
	if state.CurrentDecision.SelectedJobId == nil {
		if intent.Kind != "reviewed_source_carry_forward" || intent.Reason != "no_reward_winner" || intent.WinnerReview != nil {
			return ErrConflict
		}
		return nil
	}
	if intent.Kind != "reviewed_source_promotion" || intent.ReviewId == nil || state.ApprovedReviewId == nil || *intent.ReviewId != *state.ApprovedReviewId ||
		intent.WinnerReview == nil || intent.WinnerReview.JobId != *state.CurrentDecision.SelectedJobId || intent.WinnerReview.RoundId != predecessorId {
		return ErrWinnerReviewPending
	}
	var checkpointBytes []byte
	if err := tx.QueryRow(ctx, `SELECT transition_json FROM competition_supervisor_transition WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 ORDER BY revision DESC LIMIT 1`, intent.Environment, intent.CompetitionId, intent.SeasonId, intent.TargetEpoch).Scan(&checkpointBytes); err != nil {
		return err
	}
	var transition model.CompetitionSupervisorTransition
	if err := json.Unmarshal(checkpointBytes, &transition); err != nil {
		return err
	}
	if transition.WinnerReview == nil || *transition.WinnerReview != *intent.WinnerReview ||
		state.ApprovedReview.CreatedAt.Before(intent.WinnerReview.StartedAt) || state.ApprovedReview.CreatedAt.After(intent.WinnerReview.Deadline) ||
		state.ApprovedReview.Request.ExpectedDecisionSha256 != intent.WinnerReview.RewardDecisionSha256 || state.ApprovedReview.Request.ExpectedRevision != intent.WinnerReview.RewardRevision ||
		intent.BuildReadyAt.IsZero() || intent.BuildReadyAt.After(intent.PreparedAt) {
		return ErrConflict
	}
	return nil
}
