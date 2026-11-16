// A prospectively selected two-member policy never reinterprets retained
// three-member rounds or admits a partial/cross-version finding union.
package model

import (
	"encoding/json"
	"slices"
	"testing"
)

// Reuses only synthetic identities and evidence, with the exact selected
// member census appended to the unchanged six-criterion content.
func snowWinnerReviewForPolicyFixture(policyName string) (CompetitionAutomationPolicy, CompetitionIntegrityReviewRequest) {
	policy, review := winnerProviderReviewFixture(policyName)
	findings := append([]CompetitionReviewFinding{}, review.Findings...)
	review.Findings = nil
	for _, modelName := range CompetitionSnowWinnerReviewModelsForPolicy(policyName) {
		for _, finding := range findings {
			finding.Model = modelName
			review.Findings = append(review.Findings, finding)
		}
	}
	return policy, review
}

// Literal identities distinguish the new owner selection from the old round
// contract; every returned member slice is owned by its caller.
func TestSnowTwoMemberPolicyHasDistinctImmutableIdentity(t *testing.T) {
	if CompetitionSnowWinnerReviewPolicy != "elected_winner_snow_union_v2" || CompetitionSnowWinnerReviewModel != "snow-qwen-gemma-union" || CompetitionSnowWinnerReviewReasoning != "provider-default" {
		t.Fatal("two-member policy identity changed")
	}
	want := []string{"qwen3.8-27b", "gemma-4-26b-a4b"}
	got := CompetitionSnowWinnerReviewModels()
	if !slices.Equal(got, want) || !IsCompetitionSnowWinnerReviewPolicy(CompetitionSnowWinnerReviewPolicy) {
		t.Fatal("current policy does not require exactly Qwen and Gemma", got)
	}
	got[0] = "synthetic-unapproved-member"
	if !slices.Equal(CompetitionSnowWinnerReviewModels(), want) || CompetitionSnowWinnerReviewModelsForPolicy("synthetic-unknown-policy") != nil {
		t.Fatal("caller changed a member census or an unknown policy gained members")
	}
}

// Historical eighteen-finding and new twelve-finding approvals remain valid
// only under their own frozen provider/model/policy tuple.
func TestSnowTwoMemberAndHistoricalPolicyKeepSeparateCompleteReviews(t *testing.T) {
	for _, entry := range []struct {
		policyName string
		modelName  string
		count      int
	}{
		{policyName: CompetitionSnowWinnerReviewPolicyV1, modelName: CompetitionSnowWinnerReviewModelV1, count: 18},
		{policyName: CompetitionSnowWinnerReviewPolicy, modelName: CompetitionSnowWinnerReviewModel, count: 12},
	} {
		policy, review := snowWinnerReviewForPolicyFixture(entry.policyName)
		before, err := json.Marshal(review)
		if err != nil || len(review.Findings) != entry.count || review.ReviewerModel != entry.modelName || ValidateCompetitionAutomationPolicy(policy) != nil || ValidateCompetitionIntegrityReview(review, policy) != nil {
			t.Fatalf("complete %s review lost its immutable authority: %v", entry.policyName, err)
		}
		after, err := json.Marshal(review)
		if err != nil || string(before) != string(after) {
			t.Fatal("validation rewrote a retained review", err)
		}
		other := CompetitionSnowWinnerReviewPolicy
		if entry.policyName == other {
			other = CompetitionSnowWinnerReviewPolicyV1
		}
		policy.WinnerReviewPolicy = other
		if ValidateCompetitionIntegrityReview(review, policy) == nil {
			t.Fatal("cross-version policy relabeled a retained review")
		}
	}
}

// Neither version allows an absent/extra/repeated member or changed criterion,
// and no uncertain finding can become approval under the reduced membership.
func TestSnowTwoMemberAndHistoricalPoliciesRefusePartialOrMixedCensuses(t *testing.T) {
	for _, policyName := range []string{CompetitionSnowWinnerReviewPolicyV1, CompetitionSnowWinnerReviewPolicy} {
		for _, mutation := range []string{"missing", "extra", "wrong member", "duplicate", "uncertain", "wrong aggregate"} {
			policy, review := snowWinnerReviewForPolicyFixture(policyName)
			switch mutation {
			case "missing":
				review.Findings = review.Findings[:len(review.Findings)-1]
			case "extra":
				review.Findings = append(review.Findings, review.Findings[0])
			case "wrong member":
				review.Findings[0].Model = "synthetic-unapproved-member"
			case "duplicate":
				review.Findings[1] = review.Findings[0]
			case "uncertain":
				review.Findings[0].Disposition = "uncertain"
			case "wrong aggregate":
				review.ReviewerModel = CompetitionSnowWinnerReviewModel
				if policyName == CompetitionSnowWinnerReviewPolicy {
					review.ReviewerModel = CompetitionSnowWinnerReviewModelV1
				}
			}
			if ValidateCompetitionIntegrityReview(review, policy) == nil {
				t.Errorf("%s accepted %s", policyName, mutation)
			}
		}
	}
}
