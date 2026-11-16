// Synthetic authority shapes do not approve a source, install a runtime,
// create a baseline or grant any real submission an execution clock.
package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The typed grant contains only source/rules/API digests, never private
// workload material or a counterfeit worker or qualification success bit.
func TestCompetitionIntakeFirstGrantHasNoGradingAuthority(t *testing.T) {
	value := CompetitionSourceIntakeActivation{Schema: 1, Kind: CompetitionIntakeFirstKind,
		Policy:      CompetitionIntakeFirstPolicy{Kind: CompetitionIntakeFirstKind, FromEpoch: 2, ApprovalSha256: strings.Repeat("a", 64)},
		Environment: "synthetic", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season", Generation: 2, ExpectedGeneration: 1,
		TargetEpoch: 2, TargetRoundId: server.NewId(), IntentSha256: strings.Repeat("b", 64), RoundPolicySha256: strings.Repeat("c", 64),
		ApiReadinessSha256: strings.Repeat("d", 64), ApiRuntimeImageDigest: "sha256:" + strings.Repeat("e", 64),
		SeasonEndsAt: time.Unix(2000, 0).UTC(), RetainUntil: time.Unix(3000, 0).UTC(), CreatedAt: time.Unix(1000, 0).UTC()}
	if err := ValidateCompetitionSourceIntakeActivation(&value); err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"worker", "ready", "baseline", "seed", "providers", "started_at", "deadline"} {
		if strings.Contains(string(content), forbidden) {
			t.Fatal("intake grant leaked or invented grading authority", forbidden)
		}
	}
	var replay CompetitionSourceIntakeActivation
	if err := json.Unmarshal(content, &replay); err != nil || replay != value {
		t.Fatal("intake grant lost exact independent value identity", err)
	}
	for _, mutate := range []func(*CompetitionSourceIntakeActivation){
		func(value *CompetitionSourceIntakeActivation) { value.TargetEpoch = 1 },
		func(value *CompetitionSourceIntakeActivation) { value.Policy.FromEpoch = 1 },
		func(value *CompetitionSourceIntakeActivation) { value.ApiRuntimeImageDigest = "pending" },
		func(value *CompetitionSourceIntakeActivation) { value.ApiReadinessSha256 = "" },
		func(value *CompetitionSourceIntakeActivation) { value.RoundPolicySha256 = "" },
		func(value *CompetitionSourceIntakeActivation) { value.IntentSha256 = "" },
		func(value *CompetitionSourceIntakeActivation) { value.ExpectedGeneration = value.Generation },
	} {
		changed := value
		mutate(&changed)
		if ValidateCompetitionSourceIntakeActivation(&changed) == nil {
			t.Fatal("incomplete or historical intake grant was accepted")
		}
	}
}

// Approval of a real product promotion cannot be replayed as a no-winner
// carry, nor can an empty ranking manufacture candidate/review authority.
func TestCompetitionIntakeFirstOperatorHandoffRequiresExactDisposition(t *testing.T) {
	job, review := server.NewId(), server.NewId()
	promotion := CompetitionSourceOperatorHandoffArgs{TargetEpoch: 2, RewardDecisionSha256: strings.Repeat("a", 64), SelectedJobId: &job, ReviewId: &review,
		PatchSha256: strings.Repeat("b", 64), ReviewDecisionSha256: strings.Repeat("c", 64), ProductSourceLockSha256: strings.Repeat("d", 64),
		Disposition: "approve_promotion", IdempotencyKey: "synthetic-handoff", EvidenceSha256: strings.Repeat("e", 64)}
	if err := ValidateCompetitionSourceOperatorHandoffArgs(promotion); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CompetitionSourceOperatorHandoffArgs){
		func(value *CompetitionSourceOperatorHandoffArgs) { value.TargetEpoch = 1 },
		func(value *CompetitionSourceOperatorHandoffArgs) { value.SelectedJobId = nil },
		func(value *CompetitionSourceOperatorHandoffArgs) { value.ReviewId = nil },
		func(value *CompetitionSourceOperatorHandoffArgs) { value.RewardDecisionSha256 = "" },
		func(value *CompetitionSourceOperatorHandoffArgs) { value.ReviewDecisionSha256 = "" },
		func(value *CompetitionSourceOperatorHandoffArgs) { value.ProductSourceLockSha256 = "pending" },
		func(value *CompetitionSourceOperatorHandoffArgs) { value.Disposition = "approve_no_winner_carry" },
	} {
		changed := promotion
		mutate(&changed)
		if ValidateCompetitionSourceOperatorHandoffArgs(changed) == nil {
			t.Fatal("handoff omitted or conflated its exact decision identity")
		}
	}
	carry := promotion
	carry.SelectedJobId, carry.ReviewId, carry.PatchSha256, carry.ReviewDecisionSha256, carry.Disposition = nil, nil, "", "", "approve_no_winner_carry"
	if err := ValidateCompetitionSourceOperatorHandoffArgs(carry); err != nil {
		t.Fatal("explicit no-winner carry was rejected", err)
	}
}
