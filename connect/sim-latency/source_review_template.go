// Templates carry identity, never a prewritten judgment or invented evidence.
package main

import (
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Empty dispositions/explanations deliberately fail API validation until the
// authorized reviewer supplies every required finding and its actual evidence.
func sourceReviewTemplate(packet *controller.CompetitionSourceReviewPacket) model.CompetitionIntegrityReviewRequest {
	request := model.CompetitionIntegrityReviewRequest{
		IdempotencyKey: "review-" + server.NewId().String(), ExpectedRevision: packet.RewardRevision,
		JobId: packet.Entry.JobId, PatchSha256: packet.Entry.PatchSha256,
		BaseSourceLockSha256: packet.Entry.BaseSourceLockSha256, ScoreSha256: packet.Entry.ScoreSha256,
		BaselineSha256: packet.Seal.BaselineSha256, ArtifactManifestSha256: packet.Entry.ArtifactManifestSha256,
		CriteriaVersion: packet.ReviewCriteriaVersion, RewardDisposition: "unchanged", SourceDisposition: "deferred",
		ReasonCodes: []string{}, Findings: []model.CompetitionReviewFinding{},
	}
	for _, criterion := range []string{"measurement_integrity", "workload_generalization", "containment", "patch_scope", "reproducibility", "source_safety"} {
		request.Findings = append(request.Findings, model.CompetitionReviewFinding{Criterion: criterion})
	}
	return request
}
