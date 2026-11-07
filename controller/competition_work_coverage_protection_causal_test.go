// This composed settings regression also compiles against the prior release.
// Its failure there is acceptance of the old incomplete helper surface.
package controller

import (
	"testing"

	"github.com/urnetwork/server/model"
)

// A real coverage contract must not retain only the former single-leaf ban.
func TestCompetitionWorkCoverageSettingsRejectsPriorSingleHelperProtection(t *testing.T) {
	settings := validSettings()
	settings.CompetitionId = "synthetic-coverage-protection"
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	settings.EvaluationPolicy.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	settings.EvaluationPolicy.WorkCoveragePolicy = &coverage
	settings.EvaluationPolicy.ScorerSha256 = settings.EvaluationPolicy.SimulatorSha256
	settings.PatchPolicy.ForbiddenPaths = []string{
		"connect/sim-latency/**",
		"controller/competition_trusted_instrument_approval.go",
		"model/competition_g2_codec_test.go",
		"model/competition_g2_policy.go",
		"model/competition_trusted_instrument.go",
		"model/competition_work_coverage_policy.go",
		"model/competition_work_coverage_policy_test.go",
	}
	if err := settings.Validate(); err != nil {
		t.Fatal("complete synthetic coverage settings are invalid", err)
	}
	settings.PatchPolicy.ForbiddenPaths = []string{"connect/sim-latency/**", "model/competition_work_coverage_policy.go"}
	if err := settings.Validate(); err == nil {
		t.Fatal("coverage settings accepted prior single-helper protection")
	}
}
