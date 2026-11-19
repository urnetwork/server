// Member-content validation shares Snow disposition rules without weakening
// the public eighteen-finding quorum or inventing another member's findings.
package model

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Reproduces only the observed criterion/status/reason tuple; explanations and
// evidence are synthetic and contain no retained provider output or prompts.
func snowMixedMemberContentFixture() (CompetitionAutomationPolicy, CompetitionIntegrityReviewRequest) {
	policy, review := winnerProviderReviewFixture(CompetitionSnowWinnerReviewPolicy)
	review.RewardDisposition, review.SourceDisposition = "disqualified", "rejected"
	review.ReasonCodes = []string{"MEASUREMENT_FABRICATION"}
	review.Findings[0].Disposition, review.Findings[0].ReasonCode = "fail", "MEASUREMENT_FABRICATION"
	review.Findings[1].Disposition, review.Findings[1].ReasonCode = "uncertain", "SOURCE_NOT_READY"
	return policy, review
}

// The old policy-clearing workaround lost Snow's uncertainty-on-rejection
// semantics. The explicit content context accepts the real six, not a fake18.
func TestSnowMemberContentPreservesSupportedRejectionWithUncertainty(t *testing.T) {
	policy, review := snowMixedMemberContentFixture()
	before, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	legacyProjection := policy
	legacyProjection.WinnerReviewPolicy, legacyProjection.WinnerReviewTimeoutSeconds = "", 0
	if err := ValidateCompetitionIntegrityReview(review, legacyProjection); err == nil || !strings.Contains(err.Error(), "invalid finding disposition") {
		t.Fatal("old six-finding legacy projection did not reproduce the observed refusal", err)
	}
	if err := ValidateCompetitionSnowMemberContent(review, policy); err != nil {
		t.Fatal("genuine supported rejection lost its explicitly uncertain criterion", err)
	}
	if err := ValidateCompetitionIntegrityReview(review, policy); err == nil || !strings.Contains(err.Error(), "policy-specific finding census") {
		t.Fatal("single-member content acquired public Snow review authority", err)
	}
	after, err := json.Marshal(review)
	if err != nil || !bytes.Equal(before, after) || review.Findings[1].Disposition != "uncertain" {
		t.Fatal("content validation rewrote observed findings", err)
	}
}

// All identity/approval/support restrictions are the same shared body used by
// the public native API; the new context changes only its exact finding census.
func TestSnowMemberContentRejectsUncertainApprovalAndUnsupportedFailure(t *testing.T) {
	for _, sample := range []struct {
		name   string
		mutate func(*CompetitionIntegrityReviewRequest)
	}{
		{name: "uncertain approval", mutate: func(review *CompetitionIntegrityReviewRequest) {
			review.RewardDisposition, review.SourceDisposition = "approved", "approved"
			review.ReasonCodes = []string{"APPROVED"}
			review.Findings[0].Disposition, review.Findings[0].ReasonCode = "pass", "APPROVED"
		}},
		{name: "uncertainty without failed finding", mutate: func(review *CompetitionIntegrityReviewRequest) {
			review.Findings[0].Disposition, review.Findings[0].ReasonCode = "pass", "APPROVED"
		}},
		{name: "source only with unsupported summary", mutate: func(review *CompetitionIntegrityReviewRequest) {
			review.Findings[0].Disposition, review.Findings[0].ReasonCode = "pass", "APPROVED"
			review.Findings[5].Disposition, review.Findings[5].ReasonCode = "fail", "SOURCE_NOT_READY"
		}},
		{name: "unsupported failure reason", mutate: func(review *CompetitionIntegrityReviewRequest) { review.ReasonCodes = []string{"SCORE_TAMPERING"} }},
		{name: "missing criterion", mutate: func(review *CompetitionIntegrityReviewRequest) { review.Findings = review.Findings[:5] }},
		{name: "attributed member", mutate: func(review *CompetitionIntegrityReviewRequest) { review.Findings[0].Model = "muse-glimmer-30b" }},
		{name: "changed elected identity", mutate: func(review *CompetitionIntegrityReviewRequest) { review.ExpectedDecisionSha256 = "" }},
		{name: "changed reviewer", mutate: func(review *CompetitionIntegrityReviewRequest) { review.ReviewerModel = CompetitionWinnerReviewModel }},
	} {
		policy, review := snowMixedMemberContentFixture()
		sample.mutate(&review)
		if err := ValidateCompetitionSnowMemberContent(review, policy); err == nil {
			t.Errorf("%s acquired unsupported member content", sample.name)
		}
	}
}

// Existing non-Snow policies keep their original six-finding API semantics;
// only a full genuine Snow census may use its public uncertainty/rejection rule.
func TestSnowMemberContentDoesNotChangeLegacyOrPublicCensus(t *testing.T) {
	for _, policyName := range []string{CompetitionWinnerReviewPolicy, CompetitionLegacyWinnerReviewPolicy, "elected_winner_sol61max_v1"} {
		policy, review := winnerProviderReviewFixture(policyName)
		if err := ValidateCompetitionIntegrityReview(review, policy); err != nil {
			t.Fatal("legacy six-finding approval changed", policyName, err)
		}
		if err := ValidateCompetitionSnowMemberContent(review, policy); err == nil {
			t.Fatal("another model policy entered the Snow member context", policyName)
		}
		review.RewardDisposition, review.SourceDisposition, review.ReasonCodes = "disqualified", "rejected", []string{"MEASUREMENT_FABRICATION"}
		review.Findings[0].Disposition, review.Findings[0].ReasonCode = "fail", "MEASUREMENT_FABRICATION"
		review.Findings[1].Disposition, review.Findings[1].ReasonCode = "uncertain", "SOURCE_NOT_READY"
		if err := ValidateCompetitionIntegrityReview(review, policy); err == nil {
			t.Fatal("legacy policy acquired Snow uncertainty semantics", policyName)
		}
	}
	policy, full := snowWinnerReviewFixture()
	full.RewardDisposition, full.SourceDisposition, full.ReasonCodes = "disqualified", "rejected", []string{"MEASUREMENT_FABRICATION"}
	full.Findings[12].Disposition, full.Findings[12].ReasonCode = "fail", "MEASUREMENT_FABRICATION"
	full.Findings[13].Disposition, full.Findings[13].ReasonCode = "uncertain", "SOURCE_NOT_READY"
	if err := ValidateCompetitionIntegrityReview(full, policy); err != nil {
		t.Fatal("complete original eighteen-finding Snow rejection changed", err)
	}
	if err := ValidateCompetitionSnowMemberContent(full, policy); err == nil {
		t.Fatal("eighteen attributed findings entered a single-member census")
	}
}
