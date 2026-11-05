// Eligibility evidence is separately bound; old valid records do not gain new
// authority or change their serialized bytes when read by the new release.
package model

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Missing encoding remains readable but cannot pass the new-insert guard.
func TestCompetitionProductionBenefitHistoricalEncodingIsStable(t *testing.T) {
	policy, review := winnerProviderReviewFixture(CompetitionWinnerReviewPolicy)
	before, err := json.Marshal(review)
	if err != nil || bytes.Contains(before, []byte("production_benefit_addendum")) {
		t.Fatal("historical encoding changed", err)
	}
	digest, _ := CompetitionAutomationDigest(review)
	if err := ValidateCompetitionIntegrityReview(review, policy); err != nil {
		t.Fatal("historical review became unreadable", err)
	}
	if ValidateCompetitionProductionBenefitReview(review, true) == nil {
		t.Fatal("historical encoding authorized a new review")
	}
	after, _ := json.Marshal(review)
	afterDigest, _ := CompetitionAutomationDigest(review)
	if !bytes.Equal(before, after) || digest != afterDigest {
		t.Fatal("historical validation rewrote an immutable review")
	}
}

// Benign ineligibility has an explicit reason, not a repurposed fraud or
// source-readiness code, and requires the exact addendum plus matched evidence.
func TestCompetitionProductionBenefitReasonRequiresBoundEligibilityEvidence(t *testing.T) {
	for _, snow := range []bool{false, true} {
		makeReview := func() (CompetitionAutomationPolicy, CompetitionIntegrityReviewRequest) {
			policy, review := winnerProviderReviewFixture(CompetitionWinnerReviewPolicy)
			if snow {
				policy, review = snowWinnerReviewForPolicyFixture(CompetitionSnowWinnerReviewPolicy)
			}
			review.ProductionBenefitAddendum = CompetitionProductionBenefitReviewAddendum()
			review.RewardDisposition, review.SourceDisposition, review.ReasonCodes = "disqualified", "rejected", []string{"NO_PRODUCTION_BENEFIT"}
			for index := range review.Findings {
				if review.Findings[index].Criterion == "workload_generalization" {
					review.Findings[index].Disposition, review.Findings[index].ReasonCode = "fail", "NO_PRODUCTION_BENEFIT"
					review.Findings[index].Explanation = "Synthetic ordinary-comment patch changes no production behavior; ineligible, without an allegation of misconduct."
				}
			}
			return policy, review
		}
		policy, review := makeReview()
		if err := ValidateCompetitionIntegrityReview(review, policy); err != nil || ValidateCompetitionProductionBenefitReview(review, true) != nil {
			t.Fatal("supported explicit eligibility rejection refused", snow, err)
		}
		for _, mutate := range []func(*CompetitionIntegrityReviewRequest){
			func(r *CompetitionIntegrityReviewRequest) { r.ProductionBenefitAddendum = nil },
			func(r *CompetitionIntegrityReviewRequest) {
				r.ProductionBenefitAddendum.Version = "production-benefit/0"
			},
			func(r *CompetitionIntegrityReviewRequest) {
				r.ProductionBenefitAddendum.Sha256 = strings.Repeat("0", 64)
			},
			func(r *CompetitionIntegrityReviewRequest) { r.ReasonCodes = []string{"UNSAFE_BEHAVIOR"} },
			func(r *CompetitionIntegrityReviewRequest) { r.SourceDisposition = "approved" },
			func(r *CompetitionIntegrityReviewRequest) {
				for i := range r.Findings {
					if r.Findings[i].ReasonCode == "NO_PRODUCTION_BENEFIT" {
						r.Findings[i].Criterion = "source_safety"
					}
				}
			},
			func(r *CompetitionIntegrityReviewRequest) {
				for i := range r.Findings {
					if r.Findings[i].ReasonCode == "NO_PRODUCTION_BENEFIT" {
						r.Findings[i].Disposition = "pass"
					}
				}
			},
		} {
			policy, review := makeReview()
			mutate(&review)
			if ValidateCompetitionIntegrityReview(review, policy) == nil {
				t.Fatal("unbound or unsupported eligibility rejection accepted", snow)
			}
		}
	}
}

// Not-applicable is not an affirmative benefit, and the new reason must not
// weaken the existing prohibition on reward rejection for integration risk.
func TestCompetitionProductionBenefitRequiresAffirmativeGeneralization(t *testing.T) {
	policy, review := winnerProviderReviewFixture(CompetitionWinnerReviewPolicy)
	review.ProductionBenefitAddendum = CompetitionProductionBenefitReviewAddendum()
	if err := ValidateCompetitionIntegrityReview(review, policy); err != nil {
		t.Fatal(err)
	}
	review.Findings[1].Disposition = "not_applicable"
	if ValidateCompetitionIntegrityReview(review, policy) == nil {
		t.Fatal("no production assessment approved source")
	}
	review.Findings[1].Disposition, review.Findings[1].ReasonCode = "fail", "PRODUCT_INTEGRATION_RISK"
	review.RewardDisposition, review.SourceDisposition, review.ReasonCodes = "disqualified", "rejected", []string{"PRODUCT_INTEGRATION_RISK"}
	if err := ValidateCompetitionIntegrityReview(review, policy); err == nil || !strings.Contains(err.Error(), "cannot disqualify a reward") {
		t.Fatal("production addendum converted integration risk into reward misconduct", err)
	}
}
