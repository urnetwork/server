// Private review context binds measurement source to the activated image and
// frozen round policy. It does not create an activation or rewrite old evidence.
package controller

import (
	"errors"

	"github.com/urnetwork/server/model"
)

// An overlay's InstrumentedServerCommit is distinct from the selected product
// parent retained inside its proof. The full evaluation policy travels intact.
type CompetitionReviewInstrument struct {
	Schema            int                                      `json:"schema"`
	ServerCommit      string                                   `json:"server_commit"`
	ImageDigest       string                                   `json:"image_digest"`
	SimulatorSha256   string                                   `json:"simulator_sha256,omitempty"`
	ScorerSha256      string                                   `json:"scorer_sha256,omitempty"`
	EvaluationPolicy  model.CompetitionEvaluationPolicy        `json:"evaluation_policy"`
	TrustedInstrument *model.CompetitionTrustedInstrumentProof `json:"trusted_instrument,omitempty"`
}

// The authenticated activation/round reader owns provenance. This consistency
// check refuses substituted source/image/overlay identities at later boundaries.
func ValidateCompetitionReviewInstrument(packet *CompetitionSourceReviewPacket) error {
	if packet == nil || packet.Instrument == nil {
		return errors.New("review instrument provenance is unavailable")
	}
	value := packet.Instrument
	if value.Schema != 1 || !gitShaPattern.MatchString(value.ServerCommit) || value.ServerCommit != packet.BaseSha || value.ImageDigest != packet.EvaluatedImageDigest ||
		!imageDigestPattern.MatchString(value.ImageDigest) || value.SimulatorSha256 != value.EvaluationPolicy.SimulatorSha256 || value.ScorerSha256 != value.EvaluationPolicy.ScorerSha256 {
		return errors.New("review instrument differs from the frozen source or evaluated image")
	}
	if value.SimulatorSha256 != "" || value.ScorerSha256 != "" || value.EvaluationPolicy.G2VolumePolicy != model.CompetitionG2VolumePolicyLegacy {
		if !sha256Pattern.MatchString(value.SimulatorSha256) || !sha256Pattern.MatchString(value.ScorerSha256) {
			return errors.New("review instrument compiled binary identities are incomplete")
		}
	}
	if proof := value.TrustedInstrument; proof != nil {
		selectedLock, err := model.CompetitionSourceLockDigestForMode(proof.SelectedProductCommits, packet.Seal.Staging)
		if err != nil || proof.Schema != 1 || proof.InstrumentedServerCommit != value.ServerCommit || proof.InstrumentedServerCommit == proof.SelectedProductCommits["server"] ||
			selectedLock != proof.SelectedProductSourceLockSha256 || proof.Approval.G2VolumePolicy != value.EvaluationPolicy.G2VolumePolicy || ValidateCompetitionTrustedInstrumentApproval(&proof.Approval) != nil {
			return errors.New("review instrument overlay lost its approved product/source identity")
		}
		for repository, commit := range proof.SelectedProductCommits {
			if repository != "server" && packet.SourceCommits[repository] != commit {
				return errors.New("review instrument overlay changed a selected product dependency")
			}
		}
		receipt := proof.BuildReceipt
		if receipt == nil || receipt.EvaluatorImageDigest != value.ImageDigest || receipt.SimulatorSha256 != value.SimulatorSha256 || value.ScorerSha256 != value.SimulatorSha256 ||
			ValidateCompetitionTrustedInstrumentBuildReceipt(*receipt, proof.Approval.ApprovalSha256, proof.Approval.PatchSha256, packet.Entry.BaseSourceLockSha256, packet.Seal.Staging, receipt.WinnerReviewPolicy) != nil {
			return errors.New("review instrument overlay lacks its activated build identity")
		}
	}
	return nil
}
