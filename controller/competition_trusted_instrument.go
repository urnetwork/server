// A narrow operator-approved instrument overlay is separate authority from a
// selected product review. Neither can authorize changes belonging to the other.
package controller

import (
	"errors"
	"reflect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The configured approval is authority; a request repeating its own digest is
// not. Current-round policy and staging cannot acquire this transition.
func validateTrustedInstrumentSource(settings *Settings, intent, current *model.CompetitionSourceActivation) error {
	if settings == nil || intent == nil {
		return errors.New("trusted instrument source authorization is missing its configured scope")
	}
	// Future configuration is irrelevant to frozen epoch-one access. Every
	// actual successor must honor its validated prospective policy, including
	// proof-free carries that would otherwise silently retain the old scorer.
	if policy := settings.FutureEpochPolicy; policy != nil && intent.TargetEpoch >= 2 {
		if err := settings.validateFutureEpochPolicy(); err != nil {
			return err
		}
		if policy.FromEpoch <= intent.TargetEpoch && intent.G2VolumePolicy != policy.G2VolumePolicy {
			return errors.New("source successor does not use its prospectively approved scoring policy")
		}
	}
	proof := intent.TrustedInstrument
	if proof == nil {
		if intent.Kind == "trusted_instrument_transition" || (current != nil && intent.G2VolumePolicy != current.G2VolumePolicy) {
			return errors.New("changed instrument policy requires its independent operator proof")
		}
		return nil
	}
	if current == nil || settings.FutureEpochPolicy == nil || settings.FutureEpochPolicy.TrustedInstrument == nil || intent.Staging ||
		intent.TargetEpoch != 2 || current.TargetEpoch != 1 || current.G2VolumePolicy != model.CompetitionG2VolumePolicyLegacy ||
		intent.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork || proof.Schema != 1 ||
		proof.Approval.ApprovalSha256 != settings.FutureEpochPolicy.TrustedInstrument.ApprovalSha256 ||
		!reflect.DeepEqual(proof.Approval, *settings.FutureEpochPolicy.TrustedInstrument) {
		return errors.New("trusted instrument transition is not prospectively authorized for this successor")
	}
	if err := ValidateCompetitionTrustedInstrumentApproval(settings.FutureEpochPolicy.TrustedInstrument); err != nil {
		return err
	}
	if proof.Approval.InstrumentBaseSourceLockSha256 != current.SourceLockSha256 || proof.Approval.InstrumentBaseImageDigest != current.EvaluatorImageDigest || !reflect.DeepEqual(proof.Approval.InstrumentBaseCommits, current.Commits) {
		return errors.New("trusted instrument provenance is not the frozen epoch-one instrument")
	}
	selectedLock, err := model.CompetitionSourceLockDigestForMode(proof.SelectedProductCommits, false)
	if err != nil || selectedLock != proof.SelectedProductSourceLockSha256 || !gitShaPattern.MatchString(proof.InstrumentedServerCommit) ||
		proof.InstrumentedServerCommit != intent.Commits["server"] || proof.InstrumentedServerCommit == proof.SelectedProductCommits["server"] ||
		intent.SourceGeneration != current.SourceGeneration+1 || !sha256Pattern.MatchString(intent.SimulatorSha256) || intent.ScorerSha256 != intent.SimulatorSha256 {
		return errors.New("trusted instrument proof lost its exact selected graph, child or compiled instrument")
	}
	for repository, commit := range proof.SelectedProductCommits {
		if repository != "server" && intent.Commits[repository] != commit {
			return errors.New("trusted instrument transition changed a selected product dependency")
		}
	}
	switch proof.SelectedProductKind {
	case "reviewed_source_carry_forward":
		if intent.Kind != "trusted_instrument_transition" || intent.ReviewId != nil || proof.SelectedProductReviewId != nil || proof.SelectedProductReviewDigest != "" || proof.SelectedProductPatchSha256 != "" ||
			intent.ReviewDecisionDigest != "" || intent.ApprovedPatchSha256 != "" || !reflect.DeepEqual(proof.SelectedProductCommits, current.Commits) ||
			intent.Threshold != current.Threshold || intent.ThresholdEvidenceSha256 != current.ThresholdEvidenceSha256 {
			return errors.New("instrument-only carry must preserve actual incumbent product and threshold without review metadata")
		}
	case "reviewed_source_promotion":
		if intent.Kind != "reviewed_source_promotion" || intent.ReviewId == nil || proof.SelectedProductReviewId == nil || *proof.SelectedProductReviewId != *intent.ReviewId ||
			proof.SelectedProductReviewDigest != intent.ReviewDecisionDigest || proof.SelectedProductPatchSha256 != intent.ApprovedPatchSha256 ||
			proof.SelectedProductSourceLockSha256 == current.SourceLockSha256 {
			return errors.New("instrument overlay cannot substitute the selected winner's genuine approval")
		}
	default:
		return errors.New("trusted instrument proof has no authenticated product selection")
	}
	if proof.BuildReceipt == nil || proof.BuildReceipt.EvaluatorImageDigest != intent.EvaluatorImageDigest ||
		proof.BuildReceipt.HostQualificationSha256 != intent.HostQualificationSha256 || proof.BuildReceipt.SimulatorSha256 != intent.SimulatorSha256 {
		return errors.New("trusted instrument transition lacks its actual qualified image and binary pins")
	}
	return ValidateCompetitionTrustedInstrumentBuildReceipt(*proof.BuildReceipt, proof.Approval.ApprovalSha256, proof.Approval.PatchSha256, intent.SourceLockSha256, false, intent.WinnerReviewPolicy)
}

// Reuses independent tests/containment with an explicit instrument operation;
// zero review id means no product review is being fabricated by this receipt.
func ValidateCompetitionTrustedInstrumentBuildReceipt(receipt CompetitionReviewedSourceBuildReceipt, approvalDigest, patchDigest, lockDigest string, staging bool, policy string) error {
	if receipt.Schema != 2 || receipt.Operation != "trusted_instrument_build" || receipt.ReviewId != (server.Id{}) ||
		receipt.InstrumentApprovalSha256 != approvalDigest || !sha256Pattern.MatchString(approvalDigest) || !sha256Pattern.MatchString(receipt.SimulatorSha256) || receipt.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork {
		return errors.New("trusted instrument build receipt has a substituted authorization identity")
	}
	legacy := receipt
	legacy.Schema, legacy.Operation, legacy.InstrumentApprovalSha256, legacy.SimulatorSha256, legacy.G2VolumePolicy = 1, "", "", "", model.CompetitionG2VolumePolicyLegacy
	return ValidateCompetitionReviewedSourceBuildReceiptForPolicy(legacy, server.Id{}, patchDigest, lockDigest, staging, policy)
}
