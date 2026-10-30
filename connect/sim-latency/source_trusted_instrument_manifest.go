// Pure manifest validation is shared with the frozen-source instrument image.
// Git publication, builds and Settings-dependent authority remain outside it.
package main

import (
	"errors"
	"reflect"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Manifests preserve review provenance while recording the protected child.
// Build evidence belongs to the activation and cannot replace product approval.
func validateTrustedInstrumentGeneration(generation, previous *reviewedSourceGeneration) error {
	proof := generation.TrustedInstrument
	if proof == nil {
		if generation.Kind == "trusted_instrument_transition" || generation.G2VolumePolicy != previous.G2VolumePolicy {
			return errors.New("changed generation instrument requires its explicit operator proof")
		}
		return nil
	}
	if proof.Schema != 1 || previous.G2VolumePolicy != model.CompetitionG2VolumePolicyLegacy || generation.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork {
		return errors.New("instrument generation must explicitly extend legacy policy with minimum-work")
	}
	if err := controller.ValidateCompetitionTrustedInstrumentApproval(&proof.Approval); err != nil {
		return err
	}
	if proof.Approval.InstrumentBaseSourceLockSha256 != previous.SourceLockSha256 || !reflect.DeepEqual(proof.Approval.InstrumentBaseCommits, previous.Repositories.commitsForMode(false)) {
		return errors.New("instrument generation provenance differs from the frozen legacy source")
	}
	lock, err := model.CompetitionSourceLockDigestForMode(proof.SelectedProductCommits, false)
	commits := generation.Repositories.commitsForMode(false)
	if err != nil || lock != proof.SelectedProductSourceLockSha256 || commits["server"] != proof.InstrumentedServerCommit ||
		!sourceGitShaPattern.MatchString(proof.InstrumentedServerCommit) || proof.InstrumentedServerCommit == proof.SelectedProductCommits["server"] {
		return errors.New("instrument generation does not bind the exact selected graph and child")
	}
	for repository, commit := range proof.SelectedProductCommits {
		if repository != "server" && commits[repository] != commit {
			return errors.New("instrument generation changed a selected product dependency")
		}
	}
	switch proof.SelectedProductKind {
	case "reviewed_source_carry_forward":
		if generation.Kind != "trusted_instrument_transition" || generation.SourceApprovalId != "" || generation.SourceApprovalDigest != "" ||
			generation.EvaluatedBaseSourceLockSha256 != "" || generation.PatchSha256 != "" || generation.ScoreSha256 != "" || generation.Significance != nil ||
			proof.SelectedProductReviewId != nil || proof.SelectedProductReviewDigest != "" || proof.SelectedProductPatchSha256 != "" ||
			generation.SignificantImprovementPercent != previous.SignificantImprovementPercent || generation.ThresholdEvidenceSha256 != previous.ThresholdEvidenceSha256 ||
			!reflect.DeepEqual(proof.SelectedProductCommits, previous.Repositories.commitsForMode(false)) {
			return errors.New("instrument-only generation cannot invent a product review, threshold or product change")
		}
	case "reviewed_source_promotion":
		if generation.Kind != "reviewed_source_promotion" || proof.SelectedProductReviewId == nil || generation.SourceApprovalId != proof.SelectedProductReviewId.String() ||
			generation.SourceApprovalDigest != proof.SelectedProductReviewDigest || generation.PatchSha256 != proof.SelectedProductPatchSha256 ||
			proof.SelectedProductSourceLockSha256 == previous.SourceLockSha256 {
			return errors.New("instrument generation substituted selected winner approval")
		}
	default:
		return errors.New("instrument generation has no explicit product selection")
	}
	return nil
}
