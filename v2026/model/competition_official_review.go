// An explicitly pinned operator amendment permits a different official reviewer
// for production epoch one without relabeling Snow evidence or changing scores.
package model

import (
	"errors"
	"time"

	"github.com/urnetwork/server/v2026"
)

// This records a distinct human-authorized exception, never native model-owner
// custody. The retained authorization and review must remain independently bound.
type CompetitionOfficialReviewOverride struct {
	AuthorizationSha256               string    `json:"authorization_sha256"`
	CompletedAt                       time.Time `json:"completed_at"`
	NativeQualificationWaived         bool      `json:"native_qualification_waived"`
	UnacceptedCalibrationAcknowledged bool      `json:"unaccepted_calibration_acknowledged"`
}

// The entire request must match an exact operator-deployed configuration digest;
// a review_operator credential alone cannot authorize a reviewer substitution.
type CompetitionOfficialReviewImport struct {
	RoundId server.Id                         `json:"round_id"`
	Review  CompetitionIntegrityReviewRequest `json:"review"`
}

// This validates shape and findings, not authority. The controller and database
// separately require the exact configured grant and current sealed election.
func ValidateCompetitionOfficialReviewImport(value CompetitionOfficialReviewImport, policy CompetitionAutomationPolicy) error {
	request := value.Review
	proof := request.OfficialReviewOverride
	if value.RoundId == (server.Id{}) || proof == nil || request.HandoffId != "" ||
		request.ReviewerModel != "gpt-6-astra" || request.ReviewerReasoning != "max" ||
		!competitionAutomationSha256.MatchString(proof.AuthorizationSha256) || proof.CompletedAt.IsZero() ||
		!proof.NativeQualificationWaived || !proof.UnacceptedCalibrationAcknowledged ||
		request.RewardDisposition != "approved" || request.SourceDisposition != "approved" ||
		request.SupersedesReviewId != nil || request.AppealId != "" || request.ExpectedReplacementJobId != nil ||
		len(request.ReasonCodes) != 1 || request.ReasonCodes[0] != "APPROVED" ||
		!competitionAutomationSha256.MatchString(request.ExpectedDecisionSha256) {
		return errors.New("official epoch-one import requires the exact affirmative Astra review and explicit operator waiver")
	}
	for _, finding := range request.Findings {
		if finding.Model != "" || finding.Disposition != "pass" || finding.ReasonCode != "APPROVED" {
			return errors.New("official review must contain six independent affirmative findings, not fabricated Snow attribution")
		}
	}
	// Apply the unchanged common content and production-benefit requirements.
	// Provider/deadline authority belongs to the separately authenticated amendment.
	request.OfficialReviewOverride = nil
	policy.WinnerReviewPolicy = ""
	if err := validateCompetitionIntegrityReviewContent(request, policy, false); err != nil {
		return err
	}
	return ValidateCompetitionProductionBenefitReview(request, true)
}
