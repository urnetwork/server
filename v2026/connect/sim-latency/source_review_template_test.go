package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// All immutable identities are prefilled, but copying the template into a
// request can never approve code without real explicit reviewer findings.
func TestSourceReviewTemplateRequiresEveryActualFinding(t *testing.T) {
	packet := &controller.CompetitionSourceReviewPacket{RewardRevision: 4, ReviewCriteriaVersion: "synthetic-criteria-v2",
		Seal: model.CompetitionEvaluationSeal{BaselineSha256: strings.Repeat("a", 64)},
		Entry: model.CompetitionSealedEntry{JobId: server.NewId(), PatchSha256: strings.Repeat("b", 64), BaseSourceLockSha256: strings.Repeat("c", 64),
			ScoreSha256: strings.Repeat("d", 64), ArtifactManifestSha256: strings.Repeat("e", 64)}}
	request := sourceReviewTemplate(packet)
	policy := model.CompetitionAutomationPolicy{ReviewCriteriaVersion: packet.ReviewCriteriaVersion}
	if request.ExpectedRevision != 4 || request.JobId != packet.Entry.JobId || request.BaselineSha256 != packet.Seal.BaselineSha256 ||
		request.ScoreSha256 != packet.Entry.ScoreSha256 || request.CriteriaVersion != packet.ReviewCriteriaVersion || len(request.Findings) != 6 ||
		request.ProductionBenefitAddendum == nil || *request.ProductionBenefitAddendum != *model.CompetitionProductionBenefitReviewAddendum() {
		t.Fatal("review template lost frozen evidence identities or required criteria")
	}
	if model.ValidateCompetitionIntegrityReview(request, policy) == nil {
		t.Fatal("blank template was accepted as a genuine review")
	}
	request.RewardDisposition, request.SourceDisposition = "approved", "approved"
	request.PublicExplanation, request.WithheldEvidenceExplanation = "Synthetic explicit review.", "Synthetic private reproduction retained."
	request.PrivateEvidenceSha256, request.ReasonCodes = strings.Repeat("f", 64), []string{"APPROVED"}
	for index := range request.Findings {
		request.Findings[index].Disposition = "pass"
		request.Findings[index].Explanation = "Synthetic criterion inspected with actual fixture evidence."
		request.Findings[index].ReasonCode = "APPROVED"
		request.Findings[index].EvidenceSha256 = strings.Repeat("1", 64)
	}
	if err := model.ValidateCompetitionIntegrityReview(request, policy); err != nil {
		t.Fatalf("fully completed structured review failed: %v", err)
	}
	for index := range request.Findings {
		incomplete := request
		incomplete.Findings = append([]model.CompetitionReviewFinding{}, request.Findings...)
		incomplete.Findings[index].EvidenceSha256 = ""
		if model.ValidateCompetitionIntegrityReview(incomplete, policy) == nil {
			t.Fatalf("criterion %s accepted without evidence", incomplete.Findings[index].Criterion)
		}
	}
}

// Frozen Snow membership changes the required finding census, never supplies
// member output. Historical three-member evidence remains a separate policy.
func TestSourceReviewTemplatePreservesFrozenProviderAndDecision(t *testing.T) {
	for _, fixture := range []struct {
		policy    string
		modelName string
		count     int
	}{
		{policy: model.CompetitionSnowWinnerReviewPolicy, modelName: model.CompetitionSnowWinnerReviewModel, count: 12},
		{policy: model.CompetitionSnowWinnerReviewPolicyV1, modelName: model.CompetitionSnowWinnerReviewModelV1, count: 18},
		{policy: model.CompetitionLegacyWinnerReviewPolicy, modelName: model.CompetitionLegacyWinnerReviewModel, count: 6},
	} {
		packet := &controller.CompetitionSourceReviewPacket{RewardRevision: 3, ReviewCriteriaVersion: "synthetic-review/v2", WinnerReviewPolicy: fixture.policy,
			RewardDecision: &model.CompetitionRewardDecision{Revision: 3, DecisionSha256: strings.Repeat("f", 64)},
			Seal:           model.CompetitionEvaluationSeal{BaselineSha256: strings.Repeat("a", 64)},
			Entry: model.CompetitionSealedEntry{JobId: server.NewId(), PatchSha256: strings.Repeat("b", 64), BaseSourceLockSha256: strings.Repeat("c", 64),
				ScoreSha256: strings.Repeat("d", 64), ArtifactManifestSha256: strings.Repeat("e", 64)}}
		request := sourceReviewTemplate(packet)
		if request.ExpectedDecisionSha256 != packet.RewardDecision.DecisionSha256 || request.ReviewerModel != fixture.modelName || len(request.Findings) != fixture.count {
			t.Fatalf("template lost policy-specific decision or membership: %s", fixture.policy)
		}
		if request.PublicExplanation != "" || request.PrivateEvidenceSha256 != "" || request.RewardDisposition == "approved" || request.SourceDisposition == "approved" ||
			model.ValidateCompetitionIntegrityReview(request, model.CompetitionAutomationPolicy{WinnerReviewPolicy: fixture.policy, ReviewCriteriaVersion: packet.ReviewCriteriaVersion}) == nil {
			t.Fatal("expected provider identities became a fabricated judgment")
		}
		census := map[string]int{}
		for _, finding := range request.Findings {
			if finding.Disposition != "" || finding.Explanation != "" || finding.EvidenceSha256 != "" || finding.ReasonCode != "" {
				t.Fatal("template invented a provider finding")
			}
			census[finding.Model]++
		}
		if model.IsCompetitionSnowWinnerReviewPolicy(fixture.policy) {
			for _, modelName := range model.CompetitionSnowWinnerReviewModelsForPolicy(fixture.policy) {
				if census[modelName] != 6 {
					t.Fatalf("template omitted six required criteria for %s", modelName)
				}
			}
		} else if census[""] != 6 {
			t.Fatal("historical single-provider finding format was relabeled")
		}
	}
}
