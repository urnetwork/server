// Scoring-policy changes are prospective. Inactive next-epoch pins never
// replace the policy, executable or image frozen by an existing round.
package controller

import (
	"errors"
	"reflect"

	"github.com/urnetwork/server/v2026/model"
)

// An approved overlay and deployment executables are staged without changing
// epoch one. Qualification supplies exact future image and binary identities.
// All workload, resource, significance and submission-clock settings are kept.
type CompetitionFutureEpochPolicy struct {
	WorkCoveragePolicy          *model.CompetitionWorkCoveragePolicy        `yaml:"work_coverage_policy,omitempty"`
	DistributedEvaluationPolicy *CompetitionDistributedEvaluationPolicy     `yaml:"distributed_evaluation_policy,omitempty"`
	TrustedInstrument           *model.CompetitionTrustedInstrumentApproval `yaml:"trusted_instrument,omitempty"`
	FromEpoch                   int                                         `yaml:"from_epoch"`
	G2VolumePolicy              model.CompetitionG2VolumePolicy             `yaml:"g2_volume_policy"`
	EvaluatorImageDigest        string                                      `yaml:"evaluator_image_digest"`
	SimulatorSha256             string                                      `yaml:"simulator_sha256"`
	ScorerSha256                string                                      `yaml:"scorer_sha256"`
	HostQualificationSha256     string                                      `yaml:"host_qualification_sha256"`
	EvaluatorCommand            string                                      `yaml:"evaluator_command"`
	EvaluatorCommandSha256      string                                      `yaml:"evaluator_command_sha256"`
	SelfCheckCommand            string                                      `yaml:"self_check_command"`
	SelfCheckCommandSha256      string                                      `yaml:"self_check_command_sha256"`
}

// Derived settings own the new optional policy leaves, including the nested
// approval. Existing source maps and command configuration remain read-only.
func (self *CompetitionFutureEpochPolicy) cloneWorkCoverage() *CompetitionFutureEpochPolicy {
	if self == nil {
		return nil
	}
	copy := *self
	copy.WorkCoveragePolicy = self.WorkCoveragePolicy.Clone()
	if self.TrustedInstrument != nil {
		approval := *self.TrustedInstrument
		approval.WorkCoveragePolicy = self.TrustedInstrument.WorkCoveragePolicy.Clone()
		copy.TrustedInstrument = &approval
	}
	return &copy
}

// Empty is intentionally legacy; the exported version-one constant remains
// unchanged for historical callers and artifacts.
func (self *Settings) scorerVersion() string {
	version, _ := model.CompetitionScorerVersionForWorkCoverage(self.EvaluationPolicy.G2VolumePolicy, self.EvaluationPolicy.WorkCoveragePolicy)
	return version
}

// Future configuration cannot silently relabel an existing version-one image.
func (self *Settings) validateFutureEpochPolicy() error {
	p := self.FutureEpochPolicy
	if p == nil {
		return nil
	}
	if _, err := model.CompetitionScorerVersionForWorkCoverage(p.G2VolumePolicy, p.WorkCoveragePolicy); err != nil ||
		p.WorkCoveragePolicy != nil && (p.TrustedInstrument == nil || !reflect.DeepEqual(p.WorkCoveragePolicy, p.TrustedInstrument.WorkCoveragePolicy)) {
		return errors.New("future coverage policy lacks its exact independent instrument approval")
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
	if err := validateCompetitionDistributedPolicy(p.DistributedEvaluationPolicy); err != nil {
		return err
	}
	return validatePinnedCommand(p.SelfCheckCommand, p.SelfCheckCommandSha256, "future self-check")
}

// Only source selection/preparation for a genuinely future epoch calls this.
// Public info and current-round grading continue to use their frozen settings.
func (self *Settings) ForCompetitionEpoch(epoch int) (*Settings, error) {
	return self.forCompetitionEpoch(epoch, true)
}

// Historical active source receipts retain their original profile. Only a
// genuinely new source choice acquires the prospective capability gate.
func (self *Settings) forCompetitionEpoch(epoch int, newActivation bool) (*Settings, error) {
	if self == nil || epoch < 1 || self.SeasonPolicy.EpochCount < epoch {
		return nil, errors.New("future epoch identity is invalid")
	}
	if err := self.validateFutureEpochPolicy(); err != nil {
		return nil, err
	}
	if newActivation && !self.AutomationPolicy.Staging && epoch >= 2 && (self.FutureEpochPolicy == nil || self.FutureEpochPolicy.WorkCoveragePolicy == nil) {
		return nil, errors.New("new production epoch source activation requires explicit qualified work coverage policy")
	}
	copy := *self
	copy.EvaluationPolicy = self.EvaluationPolicy.Clone()
	copy.FutureEpochPolicy = self.FutureEpochPolicy.cloneWorkCoverage()
	p := self.FutureEpochPolicy
	if p != nil && p.FromEpoch <= epoch {
		copy.DistributedEvaluationPolicy = p.DistributedEvaluationPolicy
		copy.distributedRoundEpoch = epoch
		copy.EvaluationPolicy.G2VolumePolicy = p.G2VolumePolicy
		copy.EvaluationPolicy.WorkCoveragePolicy = p.WorkCoveragePolicy.Clone()
		patchPolicy, err := competitionWorkCoveragePatchPolicy(self.PatchPolicy, p.WorkCoveragePolicy)
		if err != nil {
			return nil, err
		}
		copy.PatchPolicy = patchPolicy
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
func scoreMatchesFrozenG2Policy(score *ScoreResult, policy model.CompetitionG2VolumePolicy, coveragePolicies ...*model.CompetitionWorkCoveragePolicy) bool {
	if score == nil {
		return false
	}
	var coverage *model.CompetitionWorkCoveragePolicy
	if len(coveragePolicies) == 1 {
		coverage = coveragePolicies[0]
	}
	actualCoverage, err := model.CompetitionScoreWorkCoveragePolicy(score.Diagnostics)
	if err != nil || !reflect.DeepEqual(actualCoverage, coverage) {
		return false
	}
	version, err := model.CompetitionScorerVersionForWorkCoverage(policy, coverage)
	if err != nil {
		return false
	}
	policyValue, policyPresent := score.Diagnostics["g2_volume_policy"]
	versionValue, versionPresent := score.Diagnostics["scorer_version"]
	if policy == model.CompetitionG2VolumePolicyLegacy {
		return !policyPresent && !versionPresent
	}
	if coverage != nil {
		if _, present := score.Gates["G7_work_coverage"]; !present {
			return false
		}
	}
	return policyValue == string(policy) && versionValue == version
}
