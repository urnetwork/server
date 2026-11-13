// Production benefit is an additional eligibility requirement, not an allegation
// of misconduct. Optional historical encoding cannot authorize a new review.
package model

import (
	"errors"

	"github.com/urnetwork/server/v2026"
)

// A public identity pins the separately published addendum without modifying a
// round's immutable scoring/criteria/provider policy or old review digests.
type CompetitionReviewAddendum struct {
	Version string `json:"version"`
	Sha256  string `json:"sha256"`
}

// Each caller owns its value; templates carry policy identity, never a verdict.
func CompetitionProductionBenefitReviewAddendum() *CompetitionReviewAddendum {
	return &CompetitionReviewAddendum{Version: server.CompetitionProductionBenefitVersion, Sha256: server.CompetitionProductionBenefitAddendumSha256()}
}

// Historical validation permits absence only without the new eligibility reason.
// New insertion requires the identity after exact idempotent replay is checked.
func ValidateCompetitionProductionBenefitReview(request CompetitionIntegrityReviewRequest, required bool) error {
	hasReason, supported := false, false
	for _, reason := range request.ReasonCodes {
		hasReason = hasReason || reason == "NO_PRODUCTION_BENEFIT"
	}
	for _, finding := range request.Findings {
		if finding.ReasonCode == "NO_PRODUCTION_BENEFIT" {
			if finding.Criterion != "workload_generalization" || finding.Disposition != "fail" || !hasReason {
				return errors.New("production-benefit ineligibility requires a matched workload-generalization failure")
			}
			supported = true
		}
	}
	addendum := request.ProductionBenefitAddendum
	if addendum == nil {
		if required || hasReason || supported {
			return errors.New("new review requires the production-benefit eligibility addendum")
		}
		return nil
	}
	if *addendum != *CompetitionProductionBenefitReviewAddendum() {
		return errors.New("production-benefit eligibility addendum identity is invalid")
	}
	if hasReason && (!supported || request.RewardDisposition != "disqualified" || request.SourceDisposition != "rejected") {
		return errors.New("production-benefit ineligibility must reject reward and source with supporting evidence")
	}
	if request.RewardDisposition == "approved" || request.RewardDisposition == "restored" || request.SourceDisposition == "approved" {
		found := false
		for _, finding := range request.Findings {
			if finding.Criterion == "workload_generalization" {
				found = true
				if finding.Disposition != "pass" {
					return errors.New("production-benefit approval requires affirmative workload generalization")
				}
			}
		}
		if !found {
			return errors.New("production-benefit approval is missing its workload-generalization finding")
		}
	}
	return nil
}
