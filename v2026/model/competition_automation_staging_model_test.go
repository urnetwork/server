// Staging exemptions require explicit authority and retain measured-safety gates.
package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// Environment names and a zero margin cannot silently opt production into staging.
func TestCompetitionAutomationStagingPolicyIsExplicitAndShadowOnly(t *testing.T) {
	policy := CompetitionAutomationPolicy{Enabled: true, Version: 2, Environment: "synthetic-control", SeasonId: "synthetic-stage-season", ElectionRule: CompetitionElectionRule,
		ReviewCriteriaVersion: "synthetic-review", SettlementPolicyVersion: "synthetic-settlement", BurnPolicyVersion: "synthetic-burn", RepeatRewardPolicy: "allow", SettlementMode: "shadow",
		RequiredApiCapability: CompetitionAutomationCapability, PolicyApprovalSha256: strings.Repeat("a", 64), SourceLockSha256: strings.Repeat("b", 64), SourceActivationGeneration: 1}
	content, err := json.Marshal(policy)
	if err != nil || strings.Contains(string(content), "staging") {
		t.Fatalf("default production wire identity changed: %s, %v", content, err)
	}
	policy.Staging, policy.StagingRoundWindowSeconds = true, 600
	if err := ValidateCompetitionAutomationPolicy(policy); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CompetitionAutomationPolicy){
		func(value *CompetitionAutomationPolicy) { value.SettlementMode = "live" },
		func(value *CompetitionAutomationPolicy) { value.Staging = false },
		func(value *CompetitionAutomationPolicy) { value.StagingRoundWindowSeconds = 59 },
		func(value *CompetitionAutomationPolicy) { value.StagingRoundWindowSeconds = 604801 },
		func(value *CompetitionAutomationPolicy) { value.StagingPreparationSeconds = -1 },
		func(value *CompetitionAutomationPolicy) { value.StagingPreparationSeconds = 57601 },
	} {
		changed := policy
		mutate(&changed)
		if ValidateCompetitionAutomationPolicy(changed) == nil {
			t.Fatalf("invalid staging authority accepted: %+v", changed)
		}
	}
}

// A noisy or slower safe candidate exercises winner automation without becoming production eligible.
func TestCompetitionAutomationStagingWinnerWaivesOnlySignificanceAndTakeover(t *testing.T) {
	score := automationTestScore(110)
	score.TakeoverEligible, score.Significance.StatisticallySignificant = false, false
	score.Significance.TakeoverMarginPercent = 0
	pValue := 0.5
	score.Significance.OneSidedPValue = &pValue
	if reason := CompetitionRewardEligibilityForMode(score, 100, 0, true); reason != "" {
		t.Fatalf("staging safe candidate rejected: %s", reason)
	}
	if reason := CompetitionRewardEligibility(score, 100, 0); reason == "" {
		t.Fatal("production inherited staging election rules")
	}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		gate := score.Gates[name]
		delete(score.Gates, name)
		if reason := CompetitionRewardEligibilityForMode(score, 100, 0, true); reason != "gate_failed_or_missing" {
			t.Fatalf("staging waived %s: %s", name, reason)
		}
		score.Gates[name] = gate
	}
	score.Significance.ReplicateCount = 3
	if reason := CompetitionRewardEligibilityForMode(score, 100, 0, true); reason != "statistical_identity_mismatch" {
		t.Fatalf("staging waived nine-replicate identity: %s", reason)
	}
}
