// Deterministic fixtures use real scorer gate keys and distinct median/mean statistics.
package model

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Every enabled scope must be addressable by the exact same scoped-token grammar.
func TestCompetitionAutomationScopeMatchesCredentials(t *testing.T) {
	policy := CompetitionAutomationPolicy{Enabled: true, Version: 2, Environment: "test-env", SeasonId: "test-season", ElectionRule: CompetitionElectionRule,
		ReviewCriteriaVersion: "review/v2", SettlementPolicyVersion: "settlement/v2", BurnPolicyVersion: "burn/v2", RepeatRewardPolicy: "allow", SettlementMode: "shadow",
		RequiredApiCapability: CompetitionAutomationCapability, PolicyApprovalSha256: strings.Repeat("a", 64), SourceLockSha256: strings.Repeat("b", 64), SourceActivationGeneration: 1}
	if err := ValidateCompetitionAutomationPolicy(policy); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"/bad", "bad space", "-bad", "bad:scope", strings.Repeat("a", 129)} {
		bad := policy
		bad.Environment = invalid
		if ValidateCompetitionAutomationPolicy(bad) == nil {
			t.Fatalf("accepted unrepresentable environment %q", invalid)
		}
		bad = policy
		bad.SeasonId = invalid
		if ValidateCompetitionAutomationPolicy(bad) == nil {
			t.Fatalf("accepted unrepresentable season %q", invalid)
		}
	}
}

// Produces a valid R=9 result; the arithmetic mean deliberately differs from raw median latency.
func automationTestScore(raw float64) *CompetitionScoreResult {
	normalized, variance, minimum, required, pValue, recommendation := 120.0, 4.0, 2.0, 12.0, 0.01, 12.0
	return &CompetitionScoreResult{ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized, Placeable: true, TakeoverEligible: true,
		Gates: map[string]CompetitionGate{"G1_success": {Passed: true, Details: map[string]any{}}, "G2_volume": {Passed: true, Details: map[string]any{}}, "G3_path_integrity": {Passed: true, Details: map[string]any{}}, "G4_matchmaking": {Passed: true, Details: map[string]any{}}, "G5_stability": {Passed: true, Details: map[string]any{}}, "G6_resources": {Passed: true, Details: map[string]any{}}},
		Significance: &CompetitionScoreSignificance{Method: "one-sided-welch-t", Alpha: 0.05, ReplicateCount: 9, BaselineMeanRawScore: 102, CandidateMeanRawScore: raw + 1,
			BaselineSampleVariance: &variance, CandidateSampleVariance: &variance, ObservedImprovementPercent: 20, TakeoverMarginPercent: 12, MinimumSignificantImprovementPercent: &minimum, RequiredImprovementPercent: &required, OneSidedPValue: &pValue, StatisticallySignificant: true, NextEpochMinimumImprovementPercent: &minimum, RecommendedNextEpochTakeoverMarginPercent: &recommendation, RecommendedNextEpochTakeoverMarginSupported: true}}
}

// A production score with differing mean and median must remain eligible.
func TestCompetitionAutomationUsesMedianMarginAndWelchMeans(t *testing.T) {
	score := automationTestScore(88)
	if reason := CompetitionRewardEligibility(score, 100, 0.12); reason != "" {
		t.Fatalf("valid median boundary rejected: %s", reason)
	}
	above := math.Nextafter(88, math.Inf(1))
	score.RawScore = &above
	if reason := CompetitionRewardEligibility(score, 100, 0.12); reason != "below_takeover_margin" {
		t.Fatalf("above-boundary reason=%q", reason)
	}
}

// All six official gates are mandatory even though the historical schema allowed sparse fixtures.
func TestCompetitionAutomationRequiresEveryRealGate(t *testing.T) {
	for _, gateName := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		score := automationTestScore(80)
		delete(score.Gates, gateName)
		if reason := CompetitionRewardEligibility(score, 100, 0.12); reason != "gate_failed_or_missing" {
			t.Errorf("missing %s: %s", gateName, reason)
		}
		score = automationTestScore(80)
		score.Gates[gateName] = CompetitionGate{Passed: false, Details: map[string]any{}}
		if reason := CompetitionRewardEligibility(score, 100, 0.12); reason != "gate_failed_or_missing" {
			t.Errorf("failed %s: %s", gateName, reason)
		}
	}
}

// An unsupported next-incumbent recommendation is not source-ready reward eligibility.
func TestCompetitionAutomationRejectsUnsupportedRecommendation(t *testing.T) {
	score := automationTestScore(80)
	unsupported := 51.0
	score.Significance.RecommendedNextEpochTakeoverMarginPercent = &unsupported
	score.Significance.RecommendedNextEpochTakeoverMarginSupported = false
	score.TakeoverEligible = false
	if reason := CompetitionRewardEligibility(score, 100, 0.12); reason == "" {
		t.Fatal("unsupported recommendation became eligible")
	}
}

// Exact ties follow admission then id, and the caller's slice remains untouched.
func TestCompetitionAutomationRanksRawAdmissionAndId(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	low, _ := server.ParseId("00000000-0000-0000-0000-000000000001")
	high, _ := server.ParseId("00000000-0000-0000-0000-000000000002")
	fast, _ := server.ParseId("00000000-0000-0000-0000-000000000003")
	entries := []CompetitionSealedEntry{{JobId: high, State: "succeeded", SubmittedAt: at, CompletedAt: at.Add(time.Second), Score: automationTestScore(80)},
		{JobId: low, State: "succeeded", SubmittedAt: at, CompletedAt: at.Add(2 * time.Second), Score: automationTestScore(80)},
		{JobId: fast, State: "succeeded", SubmittedAt: at.Add(time.Second), CompletedAt: at.Add(3 * time.Second), Score: automationTestScore(79)}}
	ranked, err := RankCompetitionSealedEntries(entries, 100, 0.12, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []server.Id{fast, low, high} {
		if ranked[i].JobId != want || ranked[i].Rank != i+1 || ranked[i].EligibleCandidateRank != i+1 {
			t.Fatalf("rank %d=%+v", i, ranked[i])
		}
	}
	if entries[0].Rank != 0 || entries[0].JobId != high {
		t.Fatal("selector mutated source inventory")
	}
}

// Nonterminal work cannot be hidden by filtering the inventory to successful jobs.
func TestCompetitionAutomationRefusesActiveInventory(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, state := range []string{"queued", "running"} {
		_, err := RankCompetitionSealedEntries([]CompetitionSealedEntry{{JobId: server.NewId(), State: state, SubmittedAt: at, CompletedAt: at.Add(time.Second)}}, 100, 0.12, nil)
		if err == nil {
			t.Errorf("accepted %s inventory", state)
		}
	}
}

// A contradictory terminal score must be rejected before the ordering comparator dereferences it.
func TestCompetitionAutomationRejectsScoreOnFailedInventory(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, state := range []string{"failed", "canceled"} {
		_, err := RankCompetitionSealedEntries([]CompetitionSealedEntry{{JobId: server.NewId(), State: state, SubmittedAt: at, CompletedAt: at.Add(time.Second), Score: &CompetitionScoreResult{}},
			{JobId: server.NewId(), State: "succeeded", SubmittedAt: at, CompletedAt: at.Add(time.Second), Score: automationTestScore(80)}}, 100, .12, nil)
		if err == nil {
			t.Fatalf("accepted malformed scored %s", state)
		}
	}
}

// Replacements use the original ranking and exhaustion is distinct from weak measurement.
func TestCompetitionAutomationReplacementExhaustionAndRestoration(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	a, b := server.NewId(), server.NewId()
	entries, err := RankCompetitionSealedEntries([]CompetitionSealedEntry{{JobId: a, State: "succeeded", SubmittedAt: at, CompletedAt: at.Add(time.Second), Score: automationTestScore(70)},
		{JobId: b, State: "succeeded", SubmittedAt: at, CompletedAt: at.Add(2 * time.Second), Score: automationTestScore(80)}}, 100, 0.12, nil)
	if err != nil {
		t.Fatal(err)
	}
	disqualified := map[server.Id]bool{a: true}
	selected, reason := SelectCompetitionReward(entries, disqualified)
	if selected == nil || selected.JobId != b || reason != "first_eligible" {
		t.Fatalf("replacement=%+v %s", selected, reason)
	}
	disqualified[b] = true
	selected, reason = SelectCompetitionReward(entries, disqualified)
	if selected != nil || reason != "all_eligible_disqualified" {
		t.Fatalf("exhaustion=%+v %s", selected, reason)
	}
	delete(disqualified, a)
	selected, _ = SelectCompetitionReward(entries, disqualified)
	if selected == nil || selected.JobId != a {
		t.Fatal("appeal did not restore deterministic rank")
	}
}

// Duplicate entitlement policy cannot pretend a significant result was statistically insignificant.
func TestCompetitionAutomationRepeatedRewardReason(t *testing.T) {
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	patch := strings.Repeat("a", 64)
	entries, err := RankCompetitionSealedEntries([]CompetitionSealedEntry{{JobId: server.NewId(), PatchSha256: patch, State: "succeeded", SubmittedAt: at, CompletedAt: at.Add(time.Second), Score: automationTestScore(80)}}, 100, 0.12, map[string]bool{patch: true})
	if err != nil {
		t.Fatal(err)
	}
	selected, reason := SelectCompetitionReward(entries, nil)
	if selected != nil || reason != "no_new_reward_entitlement" {
		t.Fatalf("duplicate result=%+v %s", selected, reason)
	}
}

// No submissions, failing gates, and insignificant improvement have separate public reasons.
func TestCompetitionAutomationNoWinnerReasons(t *testing.T) {
	if selected, reason := SelectCompetitionReward(nil, nil); selected != nil || reason != "no_submissions" {
		t.Fatalf("empty result=%+v %s", selected, reason)
	}
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	score := automationTestScore(99)
	entries, err := RankCompetitionSealedEntries([]CompetitionSealedEntry{{JobId: server.NewId(), State: "succeeded", SubmittedAt: at, CompletedAt: at.Add(time.Second), Score: score}}, 100, 0.12, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := SelectCompetitionReward(entries, nil); reason != "no_significant_improvement" {
		t.Fatal(reason)
	}
	delete(score.Gates, "G6_resources")
	entries, err = RankCompetitionSealedEntries([]CompetitionSealedEntry{{JobId: server.NewId(), State: "succeeded", SubmittedAt: at, CompletedAt: at.Add(time.Second), Score: score}}, 100, 0.12, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := SelectCompetitionReward(entries, nil); reason != "no_gate_passing_candidate" {
		t.Fatal(reason)
	}
}

// Zero configuration retains v1, while an enabled policy cannot silently assume economic approval.
func TestCompetitionAutomationDisabledByDefaultAndRequiresApproval(t *testing.T) {
	if err := ValidateCompetitionAutomationPolicy(CompetitionAutomationPolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompetitionAutomationPolicy(CompetitionAutomationPolicy{Enabled: true}); err == nil {
		t.Fatal("empty policy enabled automation")
	}
}
