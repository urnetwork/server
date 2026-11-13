// Two-member policy guards keep exact checkpoint/effort pins, both originals,
// strict missing/error behavior and immutable historical evidence replay.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// The native settings validator rejects old membership, arbitrary subsets,
// reordered members and changed checkpoint/effort even before any transport.
func TestWinnerSnowTwoMemberSettingsRejectLegacyAndSubstitutedMembers(t *testing.T) {
	provider := defaultWinnerSnowProviderSettings()
	if len(provider.Members) != 2 || provider.Members[0] != (winnerSnowMember{Name: "qwen", Model: "qwen3.8-27b", Revision: "f6b4e4c3c4119883d95c554ae69dc934d3d86433", Reasoning: "xhigh"}) || provider.Members[1] != (winnerSnowMember{Name: "gemma", Model: "gemma-4-26b-a4b", Revision: "a19cfe00be84568a6867111c9a68c9c44fdcffe6", Reasoning: "high"}) || validateWinnerSnowProviderSettings(provider) != nil {
		t.Fatal("current settings changed the approved two-member tuple")
	}
	legacy, supported := winnerSnowProviderSettingsForPolicy(model.CompetitionSnowWinnerReviewPolicyV1)
	if !supported || len(legacy.Members) != 3 || validateWinnerSnowProviderSettings(legacy) == nil {
		t.Fatal("old provider tuple entered current native execution")
	}
	for _, mutation := range []string{"missing", "extra", "order", "revision", "effort", "duplicate"} {
		candidate := defaultWinnerSnowProviderSettings()
		switch mutation {
		case "missing":
			candidate.Members = candidate.Members[:1]
		case "extra":
			candidate.Members = append(candidate.Members, legacy.Members[2])
		case "order":
			candidate.Members[0], candidate.Members[1] = candidate.Members[1], candidate.Members[0]
		case "revision":
			candidate.Members[1].Revision = strings.Repeat("0", 40)
		case "effort":
			candidate.Members[1].Reasoning = "xhigh"
		case "duplicate":
			candidate.Members[1] = candidate.Members[0]
		}
		if validateWinnerSnowProviderSettings(candidate) == nil {
			t.Errorf("current settings accepted %s", mutation)
		}
	}
}

// Each selected member can independently substantiate rejection. The complete
// other member still participates, and neither original is rewritten or dropped.
func TestWinnerSnowTwoMemberUnionRetainsEitherMembersSubstantiatedIssues(t *testing.T) {
	for rejectedIndex := range 2 {
		request, members := winnerSnowMemberFixture(t)
		judgment := members[rejectedIndex].Judgment
		judgment.Findings = append([]model.CompetitionReviewFinding{}, judgment.Findings...)
		judgment.Decision, judgment.ReasonCodes = "disqualify", []string{"UNSAFE_BEHAVIOR"}
		judgment.Findings[0].Disposition, judgment.Findings[0].ReasonCode = "fail", "UNSAFE_BEHAVIOR"
		content, err := json.Marshal(judgment)
		if err != nil {
			t.Fatal(err)
		}
		members[rejectedIndex].Judgment, members[rejectedIndex].ProviderOutput = judgment, string(content)
		before, err := json.Marshal(members)
		if err != nil {
			t.Fatal(err)
		}
		combined := aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request)
		result := winnerSnowBrokerResult(request, combined, []byte("Synthetic exact two-member retained evidence."))
		if combined.Decision != "disqualify" || len(combined.Findings) != 12 || model.ValidateCompetitionIntegrityReview(result.Review, request.Request.Policy) != nil {
			t.Fatal("selected member's substantiated issue was erased or lost native authority", rejectedIndex)
		}
		after, err := json.Marshal(members)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("aggregation changed a selected member's original", err)
		}
	}
}

// A valid other member never grants partial approval when either selected
// reviewer is absent, errored or has no genuine completed judgment.
func TestWinnerSnowTwoMemberUnionRequiresBothCompleteApprovals(t *testing.T) {
	for failedIndex := range 2 {
		for _, mutation := range []string{"missing", "error", "empty output", "uncertain"} {
			request, members := winnerSnowMemberFixture(t)
			switch mutation {
			case "missing":
				members = append(members[:failedIndex], members[failedIndex+1:]...)
			case "error":
				members[failedIndex].Error = "synthetic completed member error"
			case "empty output":
				members[failedIndex].ProviderOutput = ""
			case "uncertain":
				judgment := members[failedIndex].Judgment
				judgment.Decision, judgment.ReasonCodes = "uncertain", []string{"SOURCE_NOT_READY"}
				content, err := json.Marshal(judgment)
				if err != nil {
					t.Fatal(err)
				}
				members[failedIndex].Judgment, members[failedIndex].ProviderOutput = judgment, string(content)
			}
			combined := aggregateWinnerSnow(members, defaultWinnerSnowProviderSettings(), request)
			if combined.Decision != "uncertain" {
				t.Errorf("member %d %s authorized a partial decision", failedIndex, mutation)
			}
		}
	}
}

// Native historical replay uses v1's original three-model identity and full
// eighteen findings. The same bytes cannot qualify the new two-member policy.
func TestWinnerSnowTwoMemberValidatorPreservesHistoricalThreeMemberEvidence(t *testing.T) {
	request, judgment := winnerSnowJudgmentFixture()
	request.Request.Policy.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicyV1
	request.Request.ReviewerModel, request.Template.ReviewerModel = model.CompetitionSnowWinnerReviewModelV1, model.CompetitionSnowWinnerReviewModelV1
	provider, supported := winnerSnowProviderSettingsForPolicy(request.Request.Policy.WinnerReviewPolicy)
	if !supported {
		t.Fatal("historical provider is unsupported")
	}
	output, err := json.Marshal(judgment)
	if err != nil {
		t.Fatal(err)
	}
	members := []winnerSnowMemberEvidence{}
	for index, member := range provider.Members {
		members = append(members, winnerSnowMemberEvidence{Name: member.Name, RequestedModel: member.Model, Model: member.Model, Revision: member.Revision, Reasoning: member.Reasoning,
			ProviderResponseId: fmt.Sprintf("synthetic-historical-response-%d", index), RequestSha256: strings.Repeat("1", 64), ResponseSha256: strings.Repeat("2", 64),
			Usage: json.RawMessage(`{"input_tokens":10,"output_tokens":10}`), Judgment: judgment, ProviderOutput: string(output)})
	}
	input, err := winnerModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	providerBytes, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, providerDigest := sha256.Sum256(input), sha256.Sum256(providerBytes)
	evidence := winnerSnowEvidence{Kind: "snow-model-union-review/v1", Model: model.CompetitionSnowWinnerReviewModelV1, PolicyModel: model.CompetitionSnowWinnerReviewModelV1,
		Reasoning: model.CompetitionSnowWinnerReviewReasoning, ProviderSettings: provider, ProviderSettingsSha256: hex.EncodeToString(providerDigest[:]), InputSha256: hex.EncodeToString(inputDigest[:]),
		Members: members, Judgment: aggregateWinnerSnow(members, provider, request)}
	content, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	result := winnerSnowBrokerResult(request, evidence.Judgment, content)
	if len(result.Review.Findings) != 18 || result.Model != model.CompetitionSnowWinnerReviewModelV1 || result.Review.WithheldEvidenceExplanation != "All three attributed model judgments and provider receipts are retained privately." || validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy, request) != nil {
		t.Fatal("historical three-member evidence was relabeled or refused")
	}
	request.Request.Policy.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	request.Request.ReviewerModel, request.Template.ReviewerModel = model.CompetitionSnowWinnerReviewModel, model.CompetitionSnowWinnerReviewModel
	if validateWinnerReviewBrokerResult(result, request.Template, request.Request.Policy, request) == nil {
		t.Fatal("historical evidence acquired current two-member authority")
	}
}
