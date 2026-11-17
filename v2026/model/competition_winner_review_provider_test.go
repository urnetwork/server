// Reviewer migrations select provenance by immutable policy, preserving old
// receipt meaning while adopting the explicitly requested mini/xhigh provider.
package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Complete synthetic findings allow provider checks to run at the real review
// validation boundary instead of testing a metadata lookup alone.
func winnerProviderReviewFixture(policyName string) (CompetitionAutomationPolicy, CompetitionIntegrityReviewRequest) {
	policy := winnerPolicyFixture()
	policy.WinnerReviewPolicy, policy.WinnerReviewTimeoutSeconds = policyName, 1800
	modelName, reasoning, _ := CompetitionWinnerReviewProvider(policyName)
	digest := strings.Repeat("c", 64)
	review := CompetitionIntegrityReviewRequest{
		JobId: server.Id{0: 1}, ExpectedRevision: 1, IdempotencyKey: "synthetic-review", CriteriaVersion: policy.ReviewCriteriaVersion,
		PatchSha256: digest, BaseSourceLockSha256: digest, ScoreSha256: digest, BaselineSha256: digest, ArtifactManifestSha256: digest, PrivateEvidenceSha256: digest,
		ExpectedDecisionSha256: digest, ReviewerModel: modelName, ReviewerReasoning: reasoning,
		RewardDisposition: "approved", SourceDisposition: "approved", ReasonCodes: []string{"APPROVED"},
		PublicExplanation: "Synthetic complete review.", WithheldEvidenceExplanation: "Synthetic private findings retained.",
	}
	for _, criterion := range []string{"measurement_integrity", "workload_generalization", "containment", "patch_scope", "reproducibility", "source_safety"} {
		review.Findings = append(review.Findings, CompetitionReviewFinding{Criterion: criterion, Disposition: "pass", ReasonCode: "APPROVED", Explanation: "Synthetic evidence reviewed.", EvidenceSha256: digest})
	}
	return policy, review
}

// Literal expectations guard against accidentally changing both code and its
// fixture constants to a different sibling model or unsupported reasoning level.
func TestWinnerProviderPolicyPinsMiniXhighAndHistoricalSolMax(t *testing.T) {
	cases := []struct{ policy, modelName, reasoning string }{
		{policy: "elected_winner_gpt54mini_xhigh_v1", modelName: "gpt-5.4-mini", reasoning: "xhigh"},
		{policy: "elected_winner_sol61max_v1", modelName: "gpt-6.1-sol", reasoning: "max"},
	}
	for _, c := range cases {
		modelName, reasoning, supported := CompetitionWinnerReviewProvider(c.policy)
		if !supported || modelName != c.modelName || reasoning != c.reasoning {
			t.Errorf("provider %s = %s/%s supported=%t", c.policy, modelName, reasoning, supported)
		}
		policy, review := winnerProviderReviewFixture(c.policy)
		if err := ValidateCompetitionAutomationPolicy(policy); err != nil {
			t.Fatal(err)
		}
		if err := ValidateCompetitionIntegrityReview(review, policy); err != nil {
			t.Fatal(err)
		}
	}
	if CompetitionWinnerReviewPolicy != cases[0].policy || CompetitionWinnerReviewModel != cases[0].modelName || CompetitionWinnerReviewReasoning != cases[0].reasoning {
		t.Fatal("new-season default is not the requested mini/xhigh provider")
	}
}

// A migrated control binary cannot bless a current-policy review with old
// provenance, or reinterpret a retained old-policy review as a new one.
func TestWinnerProviderRejectsCrossPolicyAndSiblingModel(t *testing.T) {
	for _, policyName := range []string{CompetitionWinnerReviewPolicy, CompetitionLegacyWinnerReviewPolicy} {
		policy, review := winnerProviderReviewFixture(policyName)
		original, err := json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		other := CompetitionWinnerReviewPolicy
		if policyName == other {
			other = CompetitionLegacyWinnerReviewPolicy
		}
		wrongModel, wrongReasoning, _ := CompetitionWinnerReviewProvider(other)
		for _, candidate := range []CompetitionIntegrityReviewRequest{
			func() CompetitionIntegrityReviewRequest { r := review; r.ReviewerModel = wrongModel; return r }(),
			func() CompetitionIntegrityReviewRequest { r := review; r.ReviewerReasoning = wrongReasoning; return r }(),
			func() CompetitionIntegrityReviewRequest { r := review; r.ReviewerModel = "gpt-5.4"; return r }(),
		} {
			if ValidateCompetitionIntegrityReview(candidate, policy) == nil {
				t.Errorf("accepted changed provider for %s: %s/%s", policyName, candidate.ReviewerModel, candidate.ReviewerReasoning)
			}
		}
		retained, err := json.Marshal(review)
		if err != nil || string(retained) != string(original) {
			t.Fatal("provider validation mutated retained review identity", err)
		}
	}
	for _, policyName := range []string{"", "unsupported", "elected_winner_gpt54xhigh_v1"} {
		if IsCompetitionWinnerReviewPolicy(policyName) {
			t.Errorf("unknown elected-winner policy accepted: %q", policyName)
		}
	}
}
