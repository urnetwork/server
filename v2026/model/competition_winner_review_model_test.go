// New closing policy is opt-in and cannot silently reinterpret old digests.
package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// Minimal synthetic production authority with no deployment identity.
func winnerPolicyFixture() CompetitionAutomationPolicy {
	return CompetitionAutomationPolicy{Enabled: true, Version: 2, Environment: "test", SeasonId: "test-season", ElectionRule: CompetitionElectionRule, ReviewCriteriaVersion: "review/v2",
		SettlementPolicyVersion: "test-settlement", BurnPolicyVersion: "test-burn", RepeatRewardPolicy: "allow", SettlementMode: "shadow", RequiredApiCapability: CompetitionRuntimeBlobCapability,
		PolicyApprovalSha256: strings.Repeat("a", 64), SourceLockSha256: strings.Repeat("b", 64), SourceActivationGeneration: 1}
}

func TestWinnerReviewPolicyIsProspectiveAndBounded(t *testing.T) {
	policy := winnerPolicyFixture()
	if err := ValidateCompetitionAutomationPolicy(policy); err != nil {
		t.Fatal(err)
	}
	content, _ := json.Marshal(policy)
	if strings.Contains(string(content), "winner_review") {
		t.Fatal("legacy policy digest changed")
	}
	policy.WinnerReviewPolicy, policy.WinnerReviewTimeoutSeconds = CompetitionWinnerReviewPolicy, 1800
	if err := ValidateCompetitionAutomationPolicy(policy); err != nil {
		t.Fatal(err)
	}
	for _, timeout := range []int{-1, 0, 59, 86401} {
		candidate := policy
		candidate.WinnerReviewTimeoutSeconds = timeout
		if ValidateCompetitionAutomationPolicy(candidate) == nil {
			t.Fatal("unbounded review timeout accepted", timeout)
		}
	}
	policy.SourceChoiceDelaySeconds = 1
	if ValidateCompetitionAutomationPolicy(policy) == nil {
		t.Fatal("old independent source-choice delay survived adoption")
	}
}

func TestWinnerReviewTimeoutCannotActivateHistoricalFlow(t *testing.T) {
	policy := winnerPolicyFixture()
	policy.WinnerReviewTimeoutSeconds = 1800
	if ValidateCompetitionAutomationPolicy(policy) == nil {
		t.Fatal("timeout alone changed historical policy")
	}
	policy.WinnerReviewPolicy = "unsupported"
	if ValidateCompetitionAutomationPolicy(policy) == nil {
		t.Fatal("unknown closing policy accepted")
	}
}
