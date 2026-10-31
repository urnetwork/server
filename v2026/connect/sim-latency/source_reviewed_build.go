// Prospective image metadata binds reviewed code without inventing the next
// round's identity, activation, readiness or publication before a build exists.
package main

import (
	"errors"

	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Both image construction and later publication use the identical generation.
// Callers authenticate approval first; manifest validation checks its full pins.
func reviewedGenerationForProposal(current *model.CompetitionSourceActivation, proposal *reviewedSourceProposal) reviewedSourceGeneration {
	margin := proposal.Significance.RecommendedNextEpochTakeoverMarginPercent
	if current.Staging {
		margin = 0
	}
	return reviewedSourceGeneration{
		G2VolumePolicy: current.G2VolumePolicy,
		Generation:     current.SourceGeneration + 1, Kind: "reviewed_source_promotion", SourceLockSha256: proposal.SourceLockSha256,
		SignificantImprovementPercent: margin, ThresholdEvidenceSha256: proposal.Significance.ScoreSha256,
		SourceApprovalId: proposal.ReviewId.String(), SourceApprovalDigest: proposal.ReviewDecisionDigest,
		EvaluatedBaseSourceLockSha256: current.SourceLockSha256, PatchSha256: proposal.PatchSha256,
		ScoreSha256: proposal.Significance.ScoreSha256, Significance: proposal.Significance,
		Repositories: sourceRepositoriesFromCommits(proposal.Commits),
	}
}

// The immutable current intent, not a moving operator checkout, supplies history.
// An unused reviewed generation is valid image input; only the fenced supervisor
// can subsequently choose a round and append its authoritative mapping.
func reviewedSourceImageManifest(current *model.CompetitionSourceActivation, proposal *reviewedSourceProposal) ([]byte, error) {
	if current == nil || proposal == nil || proposal.Significance == nil || current.Staging != proposal.Staging ||
		current.SourceGeneration < 0 || maximumCompetitionEpoch <= current.SourceGeneration || proposal.BaseSourceLockSha256 != current.SourceLockSha256 {
		return nil, errors.New("reviewed image requires approval against the exact active source")
	}
	manifest, err := decodeSourceManifest(current.Manifest)
	if err != nil {
		return nil, err
	}
	if manifest.Schema != 2 || manifest.Staging != current.Staging || len(manifest.EvaluationSource.Generations) != current.SourceGeneration+1 {
		return nil, errors.New("reviewed image manifest does not end at the active generation")
	}
	locked, err := manifest.generation(current.SourceGeneration)
	if err != nil || locked.SourceLockSha256 != current.SourceLockSha256 || !sourceMarginMatches(locked.SignificantImprovementPercent, current.Threshold) {
		return nil, errors.New("reviewed image incumbent differs from its immutable manifest")
	}
	currentDigest, err := model.CompetitionSourceLockDigestForMode(current.Commits, current.Staging)
	if err != nil || currentDigest != current.SourceLockSha256 {
		return nil, errors.New("reviewed image incumbent commit map is incomplete or changed")
	}
	manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, reviewedGenerationForProposal(current, proposal))
	if err := validateSourceManifest(manifest); err != nil {
		return nil, err
	}
	return yaml.Marshal(manifest)
}

// No main configuration/credentials enter this request. Manifest bytes are
// prospective source identity, not evidence that the build or qualification ran.
type reviewedSourceBuildRequest struct {
	TrustedInstrument    *model.CompetitionTrustedInstrumentProof `json:"trusted_instrument,omitempty"`
	Operation            string                                   `json:"operation"`
	Proposal             *reviewedSourceProposal                  `json:"proposal"`
	SourceManifest       []byte                                   `json:"source_manifest"`
	SourceGeneration     int                                      `json:"source_generation"`
	ConfigLocalDirectory string                                   `json:"config_local_directory"`
	VaultLocalDirectory  string                                   `json:"vault_local_directory"`
	SandboxContract      string                                   `json:"sandbox_contract"`
}
