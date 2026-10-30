// Source percentages and evaluator fractions must survive decimal-unit
// conversion without rejecting an otherwise identical approved baseline.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// All supported tenth-percentage-point choices agree with their independently
// encoded fraction, not only values whose binary multiplication round-trips.
func TestSourceMarginMatchesCanonicalDecimalUnits(t *testing.T) {
	for point := 0; point <= 500; point++ {
		percent, fraction := float64(point)/10, float64(point)/1000
		if !sourceMarginMatches(percent, fraction) {
			t.Errorf("equivalent units rejected: percent=%.17g fraction=%.17g", percent, fraction)
		}
	}
}

// Arbitrary measured recommendations preserve either original conversion
// direction without rounding away genuine score precision.
func TestSourceMarginMatchesMeasuredConversionUnits(t *testing.T) {
	for _, percent := range []float64{16.1, 20.21739836111888, 28.1, 29, 49.999999999} {
		if !sourceMarginMatches(percent, percent/100) {
			t.Fatal("percent-origin recommendation rejected", percent)
		}
	}
	for _, fraction := range []float64{0.161, 0.2021739836111888, 0.281, 0.29, 0.49999999999} {
		if !sourceMarginMatches(fraction*100, fraction) {
			t.Fatal("fraction-origin recommendation rejected", fraction)
		}
	}
}

// Equivalent units are not a numeric tolerance or permission to accept missing,
// non-finite, unsupported, underflowed-zero or genuinely changed policy values.
func TestSourceMarginRejectsDifferentOrInvalidUnits(t *testing.T) {
	for _, value := range []struct{ percent, fraction float64 }{
		{percent: 29, fraction: 0.29001},
		{percent: 29.001, fraction: 0.29},
		{percent: 29, fraction: math.Nextafter(0.29, 1)},
		{percent: 29, fraction: math.Nextafter(0.29, 0)},
		{percent: 0, fraction: math.SmallestNonzeroFloat64},
		{percent: math.SmallestNonzeroFloat64, fraction: 0},
		{percent: -1, fraction: -0.01},
		{percent: 51, fraction: 0.51},
		{percent: math.NaN(), fraction: 0.29},
		{percent: 29, fraction: math.NaN()},
		{percent: math.Inf(1), fraction: math.Inf(1)},
		{percent: math.Inf(-1), fraction: math.Inf(-1)},
	} {
		if sourceMarginMatches(value.percent, value.fraction) {
			t.Errorf("invalid/different units accepted: %+v", value)
		}
	}
}

// A supported whole percentage has an equivalent binary fraction even when
// multiplying that fraction back by one hundred does not reproduce its bits.
func TestBootstrapAcceptsEquivalentDecimalMarginUnits(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	manifest.EvaluationSource.Generations[0].SignificantImprovementPercent = 29
	manifest.EvaluationSource.Rounds[0].SignificantImprovementPercent = 29
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal("fresh baseline fixture is invalid before margin comparison", err)
	}
	generation := manifest.EvaluationSource.Generations[0]
	settings := &controller.Settings{
		BaseSha: generation.Repositories.Server.Commit,
		AutomationPolicy: model.CompetitionAutomationPolicy{
			Enabled: true, PolicyApprovalSha256: strings.Repeat("e", 64),
			SourceLockSha256: generation.SourceLockSha256, SourceActivationGeneration: 1,
		},
		EvaluationPolicy: model.CompetitionEvaluationPolicy{TakeoverMargin: 0.29},
	}
	if err := validateBootstrapSource(manifest, settings); err != nil {
		t.Fatal("equivalent approved margin rejected at the percent/fraction boundary", err)
	}
}

// Conversion compatibility must not permit a genuinely different policy.
func TestBootstrapRejectsDifferentDecimalMarginUnits(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	manifest.EvaluationSource.Generations[0].SignificantImprovementPercent = 29
	manifest.EvaluationSource.Rounds[0].SignificantImprovementPercent = 29
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal("fresh baseline fixture is invalid before margin comparison", err)
	}
	generation := manifest.EvaluationSource.Generations[0]
	settings := &controller.Settings{
		BaseSha: generation.Repositories.Server.Commit,
		AutomationPolicy: model.CompetitionAutomationPolicy{
			Enabled: true, PolicyApprovalSha256: strings.Repeat("e", 64),
			SourceLockSha256: generation.SourceLockSha256, SourceActivationGeneration: 1,
		},
		EvaluationPolicy: model.CompetitionEvaluationPolicy{TakeoverMargin: 0.29001},
	}
	if err := validateBootstrapSource(manifest, settings); err == nil {
		t.Fatal("different approved margin accepted at the percent/fraction boundary")
	}
}

// Runtime activation independently checks the same approved percent against
// its persisted fraction after authenticating the complete immutable manifest.
func TestActivationAcceptsEquivalentDecimalMarginUnits(t *testing.T) {
	current, _ := reviewedImageFixture(t, false)
	manifest, err := decodeSourceManifest(current.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EvaluationSource.Epochs = nil
	manifest.EvaluationSource.Generations[0].SignificantImprovementPercent = 29
	manifest.EvaluationSource.Rounds[0].SignificantImprovementPercent = 29
	current.Manifest, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(current.Manifest)
	current.ManifestSha256 = hex.EncodeToString(digest[:])
	current.Threshold = 0.29
	if err := validateSourceActivationMode(current, &controller.Settings{}); err != nil {
		t.Fatal("equivalent activation margin rejected", err)
	}
	current.Threshold = 0.29001
	if err := validateSourceActivationMode(current, &controller.Settings{}); err == nil {
		t.Fatal("different activation margin accepted")
	}
}

// Prospective image preparation must accept an unchanged incumbent margin,
// while preserving its independent recommendation and source-review gates.
func TestReviewedImageAcceptsEquivalentDecimalMarginUnits(t *testing.T) {
	current, proposal := reviewedImageFixture(t, false)
	manifest, err := decodeSourceManifest(current.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EvaluationSource.Epochs = nil
	manifest.EvaluationSource.Generations[0].SignificantImprovementPercent = 29
	manifest.EvaluationSource.Rounds[0].SignificantImprovementPercent = 29
	current.Manifest, err = yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	current.Threshold = 0.29
	proposal.Significance.TakeoverMarginPercent = 29
	proposal.Significance.RequiredImprovementPercent = 29
	proposal.Significance.RecommendedNextEpochTakeoverMarginPercent = 29
	if _, err := reviewedSourceImageManifest(current, proposal); err != nil {
		t.Fatal("equivalent incumbent margin rejected before reviewed build", err)
	}
}

// A measured recommendation equal to the current floor is not a downgrade
// merely because multiplying the floor creates a slightly larger binary value.
func TestReviewedSelectionPreservesEquivalentDecimalMarginFloor(t *testing.T) {
	checked := 0
	for point := 161; point <= 500; point++ {
		percent, fraction := float64(point)/10, float64(point)/1000
		if !(percent < fraction*100) {
			continue
		}
		checked++
		current, proposal, deadline := reviewedProposalFixture(t)
		current.Threshold = fraction
		proposal.Significance.TakeoverMarginPercent = percent
		proposal.Significance.RequiredImprovementPercent = percent
		proposal.Significance.RecommendedNextEpochTakeoverMarginPercent = percent
		proposal.Significance.ObservedImprovementPercent = percent + 1
		proposal.Significance.CandidateMeanRawScore = 100 - (percent + 1)
		if err := validateSourceSignificanceForMode(proposal.Significance, false); err != nil {
			t.Fatal("recommendation fixture is invalid before selection", err)
		}
		if selected := selectReviewedSourceProposal([]reviewedSourceProposal{proposal}, current, deadline); selected == nil {
			t.Errorf("equal recommended floor rejected: percent=%.17g fraction=%.17g", percent, fraction)
		}
	}
	if checked == 0 {
		t.Fatal("fixture did not exercise a rounded-up binary floor")
	}
}
