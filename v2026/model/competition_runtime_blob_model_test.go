// Runtime transport is an explicit prospective contract, never inferred from
// staging, a digest, an environment name or the presence of object storage.
package model

import (
	"strings"
	"testing"
)

// Both known versions validate; an unknown or omitted capability stays closed.
func TestCompetitionRuntimeBlobCapabilityIsExplicit(t *testing.T) {
	policy := CompetitionAutomationPolicy{Enabled: true, Version: 2, Environment: "synthetic-env", SeasonId: "synthetic-season", ElectionRule: CompetitionElectionRule,
		ReviewCriteriaVersion: "synthetic-review", SettlementPolicyVersion: "synthetic-settlement", BurnPolicyVersion: "synthetic-burn", RepeatRewardPolicy: "allow", SettlementMode: "shadow",
		PolicyApprovalSha256: strings.Repeat("a", 64), SourceLockSha256: strings.Repeat("b", 64), SourceActivationGeneration: 1}
	for _, capability := range []string{CompetitionAutomationCapability, CompetitionRuntimeBlobCapability} {
		policy.RequiredApiCapability = capability
		if err := ValidateCompetitionAutomationPolicy(policy); err != nil || !SupportsCompetitionAutomationCapability(capability) {
			t.Fatalf("supported capability %q failed: %v", capability, err)
		}
	}
	for _, capability := range []string{"", "competition.reward.v3", CompetitionRuntimeBlobCapability + ".unknown"} {
		policy.RequiredApiCapability = capability
		if ValidateCompetitionAutomationPolicy(policy) == nil || SupportsCompetitionAutomationCapability(capability) {
			t.Fatalf("unknown capability %q accepted", capability)
		}
	}
}
