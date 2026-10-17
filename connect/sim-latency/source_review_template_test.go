package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
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
		request.ScoreSha256 != packet.Entry.ScoreSha256 || request.CriteriaVersion != packet.ReviewCriteriaVersion || len(request.Findings) != 6 {
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
