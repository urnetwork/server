// Epoch two substitutes the official reviewer only. Prereview evidence keeps
// its actual completion time; election, import clocks and source gates remain.
package model

import (
	"errors"

	"github.com/urnetwork/server"
)

// Preserve the exact historical model ID; an accepted ID is not an import grant.
func isCompetitionEpochTwoOfficialReviewer(modelName, reasoning string) bool {
	return reasoning == "max" && (modelName == "gpt-6-astra" || modelName == "gpt-6.2-astra")
}

// Published epoch-one imports retain their original waiver contract. Later
// epochs cannot acquire this exception through an epoch-two-shaped request.
func ValidateCompetitionOfficialReviewImportForEpoch(value CompetitionOfficialReviewImport, policy CompetitionAutomationPolicy, epoch int) error {
	if epoch == 1 {
		return ValidateCompetitionOfficialReviewImport(value, policy)
	}
	request := value.Review
	proof := request.OfficialReviewOverride
	if epoch != 2 || policy.Staging || !IsCompetitionWinnerReviewPolicy(policy.WinnerReviewPolicy) || value.RoundId == (server.Id{}) || proof == nil || proof.NativeQualificationWaived ||
		request.HandoffId != "" || !isCompetitionEpochTwoOfficialReviewer(request.ReviewerModel, request.ReviewerReasoning) ||
		!competitionAutomationSha256.MatchString(proof.AuthorizationSha256) || proof.CompletedAt.IsZero() ||
		!competitionAutomationSha256.MatchString(request.ExpectedDecisionSha256) ||
		request.SupersedesReviewId != nil || request.AppealId != "" || request.ExpectedReplacementJobId != nil || request.RewardDisposition == "restored" {
		return errors.New("official epoch-two import substitutes Astra only, without native waiver or appeal authority")
	}
	// Calibration acknowledgement is informational, never acceptance or waiver.
	// Common content uses six unattributed findings; provider and paired-negative
	// authority are enforced here without forcing an affirmative source decision.
	request.OfficialReviewOverride = nil
	policy.WinnerReviewPolicy = ""
	if err := validateCompetitionIntegrityReviewContent(request, policy, false); err != nil {
		return err
	}
	if (request.RewardDisposition == "disqualified") != (request.SourceDisposition == "rejected") {
		return errors.New("official winner rejection must revoke both reward and source eligibility")
	}
	for _, finding := range request.Findings {
		if request.RewardDisposition == "approved" && finding.Disposition == "fail" {
			return errors.New("official honesty approval cannot contain a failed common finding, even with source deferred")
		}
	}
	if request.RewardDisposition == "disqualified" {
		supported := false
		for _, finding := range request.Findings {
			for _, reason := range request.ReasonCodes {
				supported = supported || finding.Disposition == "fail" && finding.ReasonCode == reason
			}
		}
		if !supported {
			return errors.New("official disqualification requires a matched substantive failed finding")
		}
	}
	return ValidateCompetitionProductionBenefitReview(request, true)
}

// A genuine prereview may predate publication, but not the successful score and
// artifacts it claims to have reviewed. The final seal must preserve every pin.
func CompetitionOfficialReviewPrereviewMatchesSeal(value CompetitionOfficialReviewImport, seal CompetitionEvaluationSeal) bool {
	request := value.Review
	proof := request.OfficialReviewOverride
	if seal.Epoch != 2 || seal.Staging || value.RoundId != seal.RoundId || proof == nil || proof.NativeQualificationWaived ||
		proof.CompletedAt.IsZero() || request.BaselineSha256 != seal.BaselineSha256 {
		return false
	}
	for _, entry := range seal.Entries {
		if entry.JobId == request.JobId {
			return entry.State == "succeeded" && !entry.CompletedAt.IsZero() && !proof.CompletedAt.Before(entry.CompletedAt) &&
				entry.PatchSha256 == request.PatchSha256 && entry.BaseSourceLockSha256 == request.BaseSourceLockSha256 &&
				entry.ScoreSha256 == request.ScoreSha256 && entry.ArtifactManifestSha256 == request.ArtifactManifestSha256
		}
	}
	return false
}

// Epoch two can adopt earlier immutable evidence, not an earlier or replacement
// import window. Epoch one's historical clock exception is unchanged.
func CompetitionReviewWithinCheckpointForEpoch(review *CompetitionIntegrityReviewResult, checkpoint *CompetitionWinnerReviewCheckpoint, epoch int) bool {
	if review == nil || review.Request.OfficialReviewOverride == nil {
		return CompetitionReviewWithinCheckpoint(review, checkpoint)
	}
	if epoch == 1 {
		return CompetitionReviewWithinCheckpoint(review, checkpoint)
	}
	proof := review.Request.OfficialReviewOverride
	return epoch == 2 && checkpoint != nil && !proof.NativeQualificationWaived && checkpoint.HandoffId == "" &&
		review.RoundId == checkpoint.RoundId && review.Request.JobId == checkpoint.JobId && review.Request.ExpectedRevision == checkpoint.RewardRevision &&
		review.Request.ExpectedDecisionSha256 == checkpoint.RewardDecisionSha256 && review.Request.HandoffId == "" && review.ModelCompletedAt == nil &&
		isCompetitionEpochTwoOfficialReviewer(review.Request.ReviewerModel, review.Request.ReviewerReasoning) &&
		competitionAutomationSha256.MatchString(proof.AuthorizationSha256) && !proof.CompletedAt.IsZero() && !proof.CompletedAt.After(review.CreatedAt) &&
		!review.CreatedAt.Before(checkpoint.StartedAt) && review.CreatedAt.Before(checkpoint.Deadline)
}
