// Scoring-policy changes are prospective. Inactive next-epoch pins never
// replace the policy, executable or image frozen by an existing round.
package controller

import (
	"errors"

	"github.com/urnetwork/server/v2026/model"
)

// An approved overlay and deployment executables are staged without changing
// epoch one. Qualification supplies exact future image and binary identities.
// All workload, resource, significance and submission-clock settings are kept.
type CompetitionFutureEpochPolicy struct {
	TrustedInstrument       *model.CompetitionTrustedInstrumentApproval `yaml:"trusted_instrument,omitempty"`
	FromEpoch               int                                         `yaml:"from_epoch"`
	G2VolumePolicy          model.CompetitionG2VolumePolicy             `yaml:"g2_volume_policy"`
	EvaluatorImageDigest    string                                      `yaml:"evaluator_image_digest"`
	SimulatorSha256         string                                      `yaml:"simulator_sha256"`
	ScorerSha256            string                                      `yaml:"scorer_sha256"`
	HostQualificationSha256 string                                      `yaml:"host_qualification_sha256"`
	EvaluatorCommand        string                                      `yaml:"evaluator_command"`
	EvaluatorCommandSha256  string                                      `yaml:"evaluator_command_sha256"`
	SelfCheckCommand        string                                      `yaml:"self_check_command"`
	SelfCheckCommandSha256  string                                      `yaml:"self_check_command_sha256"`
}

// Empty is intentionally legacy; the exported version-one constant remains
// unchanged for historical callers and artifacts.
func (self *Settings) scorerVersion() string {
	version, _ := model.CompetitionScorerVersionForG2Policy(self.EvaluationPolicy.G2VolumePolicy)
	return version
}

// Future configuration cannot silently relabel an existing version-one image.
func (self *Settings) validateFutureEpochPolicy() error {
	p := self.FutureEpochPolicy
	if p == nil {
		return nil
	}
	if self.AutomationPolicy.Staging || p.FromEpoch != 2 || p.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork ||
		(p.EvaluatorImageDigest != "" && (!imageDigestPattern.MatchString(p.EvaluatorImageDigest) || (p.EvaluatorImageDigest == self.EvaluatorImageDigest && self.EvaluationPolicy.G2VolumePolicy != p.G2VolumePolicy))) ||
		(p.SimulatorSha256 != "" && !sha256Pattern.MatchString(p.SimulatorSha256)) ||
		(p.ScorerSha256 != "" && !sha256Pattern.MatchString(p.ScorerSha256)) ||
		p.SimulatorSha256 != p.ScorerSha256 || (p.HostQualificationSha256 != "" && !sha256Pattern.MatchString(p.HostQualificationSha256)) {
		return errors.New("future_epoch_policy must pin the new minimum-work instrument for production epoch two and later")
	}
	if err := validatePinnedCommand(p.EvaluatorCommand, p.EvaluatorCommandSha256, "future evaluator"); err != nil {
		return err
	}
	if err := ValidateCompetitionTrustedInstrumentApproval(p.TrustedInstrument); err != nil {
		return err
	}
	return validatePinnedCommand(p.SelfCheckCommand, p.SelfCheckCommandSha256, "future self-check")
}

// Only source selection/preparation for a genuinely future epoch calls this.
// Public info and current-round grading continue to use their frozen settings.
func (self *Settings) ForCompetitionEpoch(epoch int) (*Settings, error) {
	if self == nil || epoch < 1 || self.SeasonPolicy.EpochCount < epoch {
		return nil, errors.New("future epoch identity is invalid")
	}
	if err := self.validateFutureEpochPolicy(); err != nil {
		return nil, err
	}
	copy := *self
	p := self.FutureEpochPolicy
	if p != nil && p.FromEpoch <= epoch {
		copy.EvaluationPolicy.G2VolumePolicy = p.G2VolumePolicy
		if p.SimulatorSha256 != "" {
			copy.EvaluationPolicy.SimulatorSha256, copy.EvaluationPolicy.ScorerSha256 = p.SimulatorSha256, p.ScorerSha256
		}
		if p.HostQualificationSha256 != "" {
			copy.EvaluationPolicy.HostQualificationSha256 = p.HostQualificationSha256
		}
		if p.EvaluatorImageDigest != "" {
			copy.EvaluatorImageDigest = p.EvaluatorImageDigest
		}
		copy.EvaluatorCommand, copy.EvaluatorCommandSha256 = p.EvaluatorCommand, p.EvaluatorCommandSha256
		copy.SelfCheckCommand, copy.SelfCheckCommandSha256 = p.SelfCheckCommand, p.SelfCheckCommandSha256
		if self.futureBaselinePreparation != nil {
			copy.roundBaselinePreparation = self.futureBaselinePreparation
		}
	}
	return &copy, nil
}

// Scorer output must name the exact policy/version when it is non-legacy.
// Omitted historical diagnostics retain their original version-one meaning.
func scoreMatchesFrozenG2Policy(score *ScoreResult, policy model.CompetitionG2VolumePolicy) bool {
	if score == nil {
		return false
	}
	version, err := model.CompetitionScorerVersionForG2Policy(policy)
	if err != nil {
		return false
	}
	policyValue, policyPresent := score.Diagnostics["g2_volume_policy"]
	versionValue, versionPresent := score.Diagnostics["scorer_version"]
	if policy == model.CompetitionG2VolumePolicyLegacy {
		return !policyPresent && !versionPresent
	}
	return policyValue == string(policy) && versionValue == version
}
