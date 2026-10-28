// Source-build receipts retain the immutable round's Snow version; a valid
// historical three-member attestation is never relabeled as current dual proof.
package controller

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Both historical and current receipts validate only under their own policy
// and model; all ordinary build/containment fields remain mandatory.
func TestCompetitionSourceBuildSnowVersionsKeepExactProviderTuples(t *testing.T) {
	for _, policy := range []string{model.CompetitionSnowWinnerReviewPolicyV1, model.CompetitionSnowWinnerReviewPolicy} {
		modelName, reasoning, supported := model.CompetitionWinnerReviewProvider(policy)
		if !supported {
			t.Fatal("supported Snow version is absent")
		}
		digest := strings.Repeat("a", 64)
		reviewId := server.Id{0: 1}
		receipt := CompetitionReviewedSourceBuildReceipt{Schema: 1, ReviewId: reviewId, PatchSha256: digest, SourceLockSha256: digest,
			EvaluatorImageDigest: "sha256:" + digest, HostQualificationSha256: digest, ContainmentSha256: digest, TestEvidenceSha256: digest,
			SandboxId: "synthetic-owned-sandbox", Offline: true, NoMainCredentials: true, ReadOnlySource: true, Passed: true,
			TestModel: modelName, TestReasoning: reasoning, WinnerReviewPolicy: policy}
		if err := ValidateCompetitionReviewedSourceBuildReceiptForPolicy(receipt, reviewId, digest, digest, false, policy); err != nil {
			t.Fatal("matching Snow version receipt was refused", err)
		}
		other := model.CompetitionSnowWinnerReviewPolicy
		if policy == other {
			other = model.CompetitionSnowWinnerReviewPolicyV1
		}
		if ValidateCompetitionReviewedSourceBuildReceiptForPolicy(receipt, reviewId, digest, digest, false, other) == nil {
			t.Fatal("cross-version build receipt conferred source authority")
		}
	}
}
