// Separates immutable reviewed-source generations from competition round
// mappings. A carry-forward creates a mapping, never a fictitious promotion.
package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server/model"
)

// Records a reviewed implementation. Approval and measurement identities are
// mandatory for new code; a reward decision is deliberately not an approval.
type reviewedSourceGeneration struct {
	Generation                    int                 `yaml:"generation" json:"generation"`
	Kind                          string              `yaml:"kind" json:"kind"`
	SourceLockSha256              string              `yaml:"source_lock_sha256" json:"source_lock_sha256"`
	SignificantImprovementPercent float64             `yaml:"significant_improvement_percent" json:"significant_improvement_percent"`
	ThresholdEvidenceSha256       string              `yaml:"threshold_evidence_sha256" json:"threshold_evidence_sha256"`
	SourceApprovalId              string              `yaml:"source_approval_id,omitempty" json:"source_approval_id,omitempty"`
	SourceApprovalDigest          string              `yaml:"source_approval_digest,omitempty" json:"source_approval_digest,omitempty"`
	EvaluatedBaseSourceLockSha256 string              `yaml:"evaluated_base_source_lock_sha256,omitempty" json:"evaluated_base_source_lock_sha256,omitempty"`
	PatchSha256                   string              `yaml:"patch_sha256,omitempty" json:"patch_sha256,omitempty"`
	ScoreSha256                   string              `yaml:"score_sha256,omitempty" json:"score_sha256,omitempty"`
	Significance                  *sourceSignificance `yaml:"significance,omitempty" json:"significance,omitempty"`
	Repositories                  sourceRepositories  `yaml:"repositories" json:"repositories"`
}

// Records the exact source choice for one future round. Its reward digest is
// provenance only, including when that round carries an unshipped winner.
type reviewedSourceRound struct {
	Epoch                         int     `yaml:"epoch" json:"epoch"`
	SourceGeneration              int     `yaml:"source_generation" json:"source_generation"`
	ActivationGeneration          int64   `yaml:"activation_generation" json:"activation_generation"`
	SourceDecisionDigest          string  `yaml:"source_decision_digest" json:"source_decision_digest"`
	Kind                          string  `yaml:"kind" json:"kind"`
	Reason                        string  `yaml:"reason" json:"reason"`
	RewardDecisionDigest          string  `yaml:"reward_decision_digest,omitempty" json:"reward_decision_digest,omitempty"`
	SourceLockSha256              string  `yaml:"source_lock_sha256" json:"source_lock_sha256"`
	SignificantImprovementPercent float64 `yaml:"significant_improvement_percent" json:"significant_improvement_percent"`
}

// Validates historical entries separately so upgrading a manifest never
// changes their interpretation or relabels an unreviewed winner as source.
func validateReviewedSourceManifest(manifest *sourceManifest) error {
	if manifest == nil || manifest.Schema != 2 || manifest.EvaluationSource.Branch != sourceBranch(manifest.Staging) {
		return errors.New("reviewed source config requires schema 2 and the explicit mode's source branch")
	}
	if manifest.Staging && len(manifest.EvaluationSource.Epochs) != 0 {
		return errors.New("staging requires a separate v2 baseline, not relabeled historical production epochs")
	}
	if _, err := time.Parse(time.RFC3339, manifest.FrozenAt); err != nil {
		return errors.New("source config frozen_at must be an RFC3339 timestamp")
	}
	if manifest.ControlPlaneIdentity.ApiBranch != "main" ||
		manifest.ControlPlaneIdentity.WorkerBranch != "main" ||
		manifest.ControlPlaneIdentity.FreezeMainCommits ||
		!manifest.ControlPlaneIdentity.PersistPerEvaluation ||
		manifest.ControlPlaneIdentity.RuntimeImageDigestEnvironment != "WARP_IMAGE_DIGEST" {
		return errors.New("control-plane identity policy is invalid")
	}
	if len(manifest.EvaluationSource.Epochs) > 0 {
		legacy := *manifest
		legacy.Schema = 1
		legacy.EvaluationSource.Generations = nil
		legacy.EvaluationSource.Rounds = nil
		if err := validateSourceManifest(&legacy); err != nil {
			return fmt.Errorf("historical source ledger: %w", err)
		}
	}
	generationRecords := manifest.EvaluationSource.Generations
	if len(generationRecords) == 0 || maximumCompetitionEpoch+1 < len(generationRecords) {
		return errors.New("reviewed source config requires baseline generation 0 and at most six promotions")
	}
	for index, generation := range generationRecords {
		if generation.Generation != index || !validSha256(generation.SourceLockSha256) ||
			!validSha256(generation.ThresholdEvidenceSha256) ||
			!validSourceMargin(generation.SignificantImprovementPercent, manifest.Staging) {
			return fmt.Errorf("source generation %d identity or threshold is invalid", index)
		}
		for repository, commit := range generation.Repositories.commits() {
			if !sourceGitShaPattern.MatchString(commit) {
				return fmt.Errorf("source generation %d repository %s has an invalid commit", index, repository)
			}
		}
		lockDigest, err := model.CompetitionSourceLockDigest(generation.Repositories.commits())
		if err != nil || lockDigest != generation.SourceLockSha256 {
			return fmt.Errorf("source generation %d lock digest does not bind the complete commit map", index)
		}
		if index == 0 {
			if generation.Kind != "baseline" || generation.SourceApprovalId != "" ||
				generation.SourceApprovalDigest != "" || generation.Significance != nil ||
				generation.EvaluatedBaseSourceLockSha256 != "" || generation.PatchSha256 != "" || generation.ScoreSha256 != "" {
				return errors.New("source generation 0 must be an approved baseline without promotion metadata")
			}
			if len(manifest.EvaluationSource.Epochs) > 0 {
				previous := manifest.EvaluationSource.Epochs[len(manifest.EvaluationSource.Epochs)-1]
				if generation.Repositories != previous.Repositories || generation.SignificantImprovementPercent != previous.SignificantImprovementPercent {
					return errors.New("v2 baseline must preserve the last reviewed legacy source and margin")
				}
			}
			continue
		}
		previous := generationRecords[index-1]
		if generation.Kind != "reviewed_source_promotion" || generation.SourceApprovalId == "" ||
			!validSha256(generation.SourceApprovalDigest) || !validSha256(generation.PatchSha256) ||
			!validSha256(generation.ScoreSha256) || generation.EvaluatedBaseSourceLockSha256 != previous.SourceLockSha256 ||
			generation.SourceLockSha256 == previous.SourceLockSha256 {
			return fmt.Errorf("source generation %d requires separate source approval on the exact incumbent", index)
		}
		if err := validateSourceSignificanceForMode(generation.Significance, manifest.Staging); err != nil {
			return fmt.Errorf("source generation %d significance: %w", index, err)
		}
		if generation.Significance.ScoreSha256 != generation.ScoreSha256 ||
			(!manifest.Staging && generation.SignificantImprovementPercent != generation.Significance.RecommendedNextEpochTakeoverMarginPercent) ||
			generation.SignificantImprovementPercent < previous.SignificantImprovementPercent {
			return fmt.Errorf("source generation %d recommendation does not preserve its authenticated calibrated floor", index)
		}
		for repository, commit := range generation.Repositories.commits() {
			if repository != "server" && commit != previous.Repositories.commits()[repository] {
				return fmt.Errorf("source generation %d changes protected dependency %s", index, repository)
			}
		}
		if generation.Repositories.Server == previous.Repositories.Server {
			return fmt.Errorf("source generation %d does not change the evaluated server", index)
		}
	}
	for index, round := range manifest.EvaluationSource.Rounds {
		if round.Epoch < 1 || maximumCompetitionEpoch < round.Epoch ||
			(index > 0 && round.Epoch != manifest.EvaluationSource.Rounds[index-1].Epoch+1) ||
			round.ActivationGeneration < 1 || !validSha256(round.SourceDecisionDigest) ||
			(round.RewardDecisionDigest != "" && !validSha256(round.RewardDecisionDigest)) || round.Reason == "" {
			return fmt.Errorf("source round mapping %d has an invalid identity", index)
		}
		generation, err := manifest.generation(round.SourceGeneration)
		if err != nil || round.SourceLockSha256 != generation.SourceLockSha256 ||
			round.SignificantImprovementPercent != generation.SignificantImprovementPercent {
			return fmt.Errorf("source round %d does not match its explicit reviewed generation and margin", round.Epoch)
		}
		previousGeneration := 0
		if index > 0 {
			previousRound := manifest.EvaluationSource.Rounds[index-1]
			previousGeneration = previousRound.SourceGeneration
			if round.ActivationGeneration <= previousRound.ActivationGeneration {
				return errors.New("source activation generations must increase")
			}
		}
		switch round.Kind {
		case "reviewed_source_carry_forward":
			if round.SourceGeneration != previousGeneration {
				return errors.New("reviewed source carry-forward must retain source generation and margin")
			}
		case "reviewed_source_promotion":
			if round.SourceGeneration != previousGeneration+1 || generation.SourceApprovalId == "" {
				return errors.New("source promotion must select the next separately approved generation")
			}
		default:
			return fmt.Errorf("source round %d has unsupported v2 choice kind %q", round.Epoch, round.Kind)
		}
	}
	return nil
}

// Looks up a source generation, never deriving it from a competition epoch.
func (self *sourceManifest) generation(generationNumber int) (*reviewedSourceGeneration, error) {
	if self.Schema != 2 || generationNumber < 0 || len(self.EvaluationSource.Generations) <= generationNumber ||
		self.EvaluationSource.Generations[generationNumber].Generation != generationNumber {
		return nil, fmt.Errorf("source config does not contain generation %d", generationNumber)
	}
	return &self.EvaluationSource.Generations[generationNumber], nil
}

// Looks up the immutable mapping for a competition epoch. No arithmetic
// fallback is allowed when a v2 mapping has not yet been activated.
func (self *sourceManifest) sourceRound(epochNumber int) (*reviewedSourceRound, error) {
	if self.Schema == 2 {
		for index := range self.EvaluationSource.Rounds {
			round := &self.EvaluationSource.Rounds[index]
			if round.Epoch == epochNumber {
				return round, nil
			}
		}
	}
	return nil, fmt.Errorf("source config has no v2 mapping for round %d", epochNumber)
}
