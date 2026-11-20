package model

import (
	"errors"
	"time"
)

// This selector is only a routing pin. Opening the late path also requires
// the independent full-request authorization and durable database binding.
func IsCompetitionEpochThreeSourceRecovery(value CompetitionOfficialReviewImport) bool {
	return value.RoundId.String() == "01a1c110-585b-ce38-4f29-ca4860a1c058" &&
		value.Review.PrivateEvidenceSha256 == "ce3876303e142c95f3af55fe1c97dc11326f65f201baccf090fa8fac0e92e808"
}

// ValidateCompetitionEpochThreeSourceRecoveryEvidence verifies the predecessor,
// not operator authorization. A durable recovery grant must independently
// authorize the complete new request and the exact expired checkpoint.
func ValidateCompetitionEpochThreeSourceRecoveryEvidence(prior *CompetitionIntegrityReviewResult, next CompetitionOfficialReviewImport, seal CompetitionEvaluationSeal, decision CompetitionRewardDecision, checkpoint *CompetitionWinnerReviewCheckpoint, policy CompetitionAutomationPolicy, now time.Time) error {
	if prior == nil || checkpoint == nil || policy.Staging || seal.Epoch != 2 || seal.Staging ||
		now.IsZero() || now.Before(checkpoint.Deadline) ||
		!CompetitionReviewWithinCheckpointForEpoch(prior, checkpoint, 2) {
		return errors.New("source recovery requires an expired epoch-two checkpoint and its in-window review")
	}
	request := next.Review
	original := prior.Request
	if decision.SelectedJobId == nil || *decision.SelectedJobId != request.JobId ||
		decision.RoundId != seal.RoundId || decision.Revision != request.ExpectedRevision ||
		decision.DecisionSha256 != request.ExpectedDecisionSha256 ||
		original.RewardDisposition != "approved" || original.SourceDisposition != "deferred" ||
		request.RewardDisposition != "approved" || request.SourceDisposition != "approved" ||
		request.ReviewerModel != "gpt-6.2-astra" || request.ReviewerReasoning != "max" {
		return errors.New("source recovery must preserve the approved reward and address only its deferred source")
	}
	if err := ValidateCompetitionOfficialReviewImportForEpoch(next, policy, 2); err != nil {
		return err
	}
	if request.ExpectedRevision != original.ExpectedRevision || request.ExpectedDecisionSha256 != original.ExpectedDecisionSha256 ||
		request.JobId != original.JobId || request.OfficialReviewOverride.CompletedAt.Before(checkpoint.Deadline) ||
		request.OfficialReviewOverride.CompletedAt.After(now) ||
		!competitionAutomationSha256.MatchString(request.PrivateEvidenceSha256) ||
		request.PrivateEvidenceSha256 == original.PrivateEvidenceSha256 || request.IdempotencyKey == original.IdempotencyKey ||
		!CompetitionOfficialReviewPrereviewMatchesSeal(CompetitionOfficialReviewImport{RoundId: prior.RoundId, Review: original}, seal) ||
		!CompetitionOfficialReviewPrereviewMatchesSeal(next, seal) {
		return errors.New("source recovery requires a new assessment of the same sealed winner")
	}
	return nil
}
