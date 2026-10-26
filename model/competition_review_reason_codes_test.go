// The provider schema and native validator share one immutable vocabulary.
package model

import (
	"slices"
	"testing"
)

// The returned slice is caller-owned and cannot retune subsequent native checks.
func TestCompetitionIntegrityReviewReasonCodesAreCanonicalFreshCopies(t *testing.T) {
	expected := []string{"APPROVED", "SCORE_TAMPERING", "MEASUREMENT_FABRICATION", "WORKLOAD_SPECIAL_CASING", "BENCHMARK_DETECTION", "SANDBOX_ESCAPE", "SECRET_ACCESS", "EXFILTRATION", "UNSAFE_BEHAVIOR", "PERSISTENCE", "DENIAL_OF_SERVICE", "PATCH_POLICY_VIOLATION", "UNRELATED_CHANGE", "REWARD_DUPLICATE", "REWARD_RULE_EVASION", "SOURCE_NOT_READY", "PRODUCT_INTEGRATION_RISK", "EVALUATION_INTEGRITY_INCIDENT", "APPEAL_UPHELD"}
	codes := CompetitionIntegrityReviewReasonCodes()
	if !slices.Equal(codes, expected) {
		t.Fatal("native review vocabulary changed", codes)
	}
	codes[0] = "SYNTHETIC_UNAPPROVED_REASON"
	if !slices.Equal(CompetitionIntegrityReviewReasonCodes(), expected) {
		t.Fatal("caller changed another owner's native review vocabulary")
	}
	policy, review := winnerProviderReviewFixture(CompetitionWinnerReviewPolicy)
	if err := ValidateCompetitionIntegrityReview(review, policy); err != nil {
		t.Fatal("returned-slice mutation changed native approval validation", err)
	}
	review.ReasonCodes = []string{codes[0]}
	if ValidateCompetitionIntegrityReview(review, policy) == nil {
		t.Fatal("returned-slice mutation authorized a new reason code")
	}
}
