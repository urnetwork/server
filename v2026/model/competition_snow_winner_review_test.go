// The Snow union is additive authority with complete per-member findings;
// retained single-model reviews keep their exact provenance and wire shape.
package model

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// Reuses synthetic evidence while attributing each complete criterion set.
func snowWinnerReviewFixture() (CompetitionAutomationPolicy, CompetitionIntegrityReviewRequest) {
	policy, review := winnerProviderReviewFixture(CompetitionSnowWinnerReviewPolicy)
	single := review.Findings
	review.Findings = nil
	for _, modelName := range CompetitionSnowWinnerReviewModels() {
		for _, finding := range single {
			finding.Model = modelName
			review.Findings = append(review.Findings, finding)
		}
	}
	return policy, review
}

// Literal tuples prohibit cross-policy reinterpretation and obsolete dual mode.
func TestSnowWinnerPolicyIsAdditiveAndPinsAllThreeMembers(t *testing.T) {
	if CompetitionSnowWinnerReviewPolicy != "elected_winner_snow_union_v1" || CompetitionSnowWinnerReviewModel != "snow-qwen-gemma-muse-union" || CompetitionSnowWinnerReviewReasoning != "provider-default" {
		t.Fatal("Snow aggregate identity changed")
	}
	modelName, reasoning, supported := CompetitionWinnerReviewProvider(CompetitionSnowWinnerReviewPolicy)
	if !supported || modelName != CompetitionSnowWinnerReviewModel || reasoning != CompetitionSnowWinnerReviewReasoning || !IsCompetitionWinnerReviewPolicy(CompetitionSnowWinnerReviewPolicy) {
		t.Fatal("Snow policy did not enter the elected-winner lifecycle")
	}
	wantModels := []string{"qwen3.8-27b", "gemma-4-26b-a4b", "muse-glimmer-30b"}
	models := CompetitionSnowWinnerReviewModels()
	if !slices.Equal(models, wantModels) {
		t.Fatal("Snow member census changed", models)
	}
	models[0] = "synthetic-unapproved-member"
	if !slices.Equal(CompetitionSnowWinnerReviewModels(), wantModels) {
		t.Fatal("caller mutated the process-wide Snow member census")
	}
	for _, unknown := range []string{"elected_winner_snow_dual_v1", "elected_winner_snow_union_v2"} {
		if IsCompetitionWinnerReviewPolicy(unknown) {
			t.Errorf("unknown Snow policy was adopted: %s", unknown)
		}
	}
}

// Approval requires all eighteen individual findings, never merged criteria.
func TestSnowWinnerReviewAcceptsCompleteThreeMemberApproval(t *testing.T) {
	policy, review := snowWinnerReviewFixture()
	if len(review.Findings) != 18 {
		t.Fatal("synthetic Snow fixture lost a member")
	}
	review.Findings[7].Disposition = "not_applicable"
	if err := ValidateCompetitionAutomationPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompetitionIntegrityReview(review, policy); err != nil {
		t.Fatal(err)
	}
}

// Unknown, repeated and missing model/criterion pairs cannot form a quorum.
func TestSnowWinnerReviewRejectsIncompleteOrCrossProviderFindings(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CompetitionIntegrityReviewRequest)
	}{
		{name: "missing finding", mutate: func(review *CompetitionIntegrityReviewRequest) { review.Findings = review.Findings[:17] }},
		{name: "missing third member", mutate: func(review *CompetitionIntegrityReviewRequest) { review.Findings = review.Findings[:12] }},
		{name: "extra finding", mutate: func(review *CompetitionIntegrityReviewRequest) {
			review.Findings = append(review.Findings, review.Findings[0])
		}},
		{name: "duplicate pair", mutate: func(review *CompetitionIntegrityReviewRequest) { review.Findings[17] = review.Findings[0] }},
		{name: "missing model attribution", mutate: func(review *CompetitionIntegrityReviewRequest) { review.Findings[0].Model = "" }},
		{name: "unknown member", mutate: func(review *CompetitionIntegrityReviewRequest) {
			review.Findings[0].Model = "synthetic-unapproved-member"
		}},
		{name: "cross-provider member", mutate: func(review *CompetitionIntegrityReviewRequest) {
			review.Findings[0].Model = CompetitionWinnerReviewModel
		}},
		{name: "single-model authority", mutate: func(review *CompetitionIntegrityReviewRequest) { review.ReviewerModel = CompetitionWinnerReviewModel }},
		{name: "member as aggregate", mutate: func(review *CompetitionIntegrityReviewRequest) { review.ReviewerModel = "qwen3.8-27b" }},
		{name: "mixed reasoning tuple", mutate: func(review *CompetitionIntegrityReviewRequest) { review.ReviewerReasoning = "xhigh" }},
		{name: "uncertain approval", mutate: func(review *CompetitionIntegrityReviewRequest) { review.Findings[0].Disposition = "uncertain" }},
		{name: "uncertain API authority", mutate: func(review *CompetitionIntegrityReviewRequest) {
			review.RewardDisposition, review.SourceDisposition = "unchanged", "deferred"
			review.ReasonCodes = []string{"SOURCE_NOT_READY"}
		}},
	}
	for _, fixture := range cases {
		policy, review := snowWinnerReviewFixture()
		fixture.mutate(&review)
		if ValidateCompetitionIntegrityReview(review, policy) == nil {
			t.Errorf("Snow union accepted %s", fixture.name)
		}
	}
}

// A substantiated member failure rejects while retaining another member's
// uncertainty as a finding, not as independent authority or an omitted issue.
func TestSnowWinnerReviewRetainsUncertaintyAlongsideSubstantiatedRejection(t *testing.T) {
	policy, review := snowWinnerReviewFixture()
	review.RewardDisposition, review.SourceDisposition = "disqualified", "rejected"
	review.ReasonCodes = []string{"UNSAFE_BEHAVIOR"}
	review.Findings[0].Disposition, review.Findings[0].ReasonCode = "fail", "UNSAFE_BEHAVIOR"
	review.Findings[7].Disposition, review.Findings[7].ReasonCode = "uncertain", "SOURCE_NOT_READY"
	review.Findings[13].Disposition, review.Findings[13].ReasonCode = "uncertain", "PRODUCT_INTEGRATION_RISK"
	if err := ValidateCompetitionIntegrityReview(review, policy); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"no substantiated fail", "unmatched fail", "approval reason", "source-only reason"} {
		_, candidate := snowWinnerReviewFixture()
		candidate.RewardDisposition, candidate.SourceDisposition = "disqualified", "rejected"
		candidate.ReasonCodes = []string{"UNSAFE_BEHAVIOR"}
		candidate.Findings[0].Disposition, candidate.Findings[0].ReasonCode = "fail", "UNSAFE_BEHAVIOR"
		switch invalid {
		case "no substantiated fail":
			candidate.Findings[0].Disposition = "uncertain"
		case "unmatched fail":
			candidate.ReasonCodes = []string{"SCORE_TAMPERING"}
		case "approval reason":
			candidate.ReasonCodes = []string{"APPROVED"}
		case "source-only reason":
			candidate.ReasonCodes = []string{"SOURCE_NOT_READY"}
		}
		if ValidateCompetitionIntegrityReview(candidate, policy) == nil {
			t.Errorf("Snow rejection accepted %s", invalid)
		}
	}
}

// New attribution is absent from legacy JSON and cannot widen old criteria.
func TestSnowWinnerReviewDoesNotRelabelHistoricalSingleModelReviews(t *testing.T) {
	for _, policyName := range []string{CompetitionWinnerReviewPolicy, CompetitionLegacyWinnerReviewPolicy} {
		policy, review := winnerProviderReviewFixture(policyName)
		before, err := json.Marshal(review)
		if err != nil || strings.Contains(string(before), `"model":`) {
			t.Fatal("legacy wire acquired member attribution", err)
		}
		if err := ValidateCompetitionIntegrityReview(review, policy); err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"member", "uncertain", "aggregate"} {
			_, candidate := winnerProviderReviewFixture(policyName)
			switch change {
			case "member":
				candidate.Findings[0].Model = "qwen3.8-27b"
			case "uncertain":
				candidate.RewardDisposition, candidate.SourceDisposition = "disqualified", "rejected"
				candidate.ReasonCodes = []string{"UNSAFE_BEHAVIOR"}
				candidate.Findings[0].Disposition = "uncertain"
			case "aggregate":
				candidate.ReviewerModel, candidate.ReviewerReasoning = CompetitionSnowWinnerReviewModel, CompetitionSnowWinnerReviewReasoning
			}
			if ValidateCompetitionIntegrityReview(candidate, policy) == nil {
				t.Errorf("historical %s review accepted %s", policyName, change)
			}
		}
		after, err := json.Marshal(review)
		if err != nil || string(before) != string(after) {
			t.Fatal("validating Snow support mutated a historical receipt", err)
		}
	}
}
