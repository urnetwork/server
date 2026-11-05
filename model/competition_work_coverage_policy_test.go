// Policy tests use synthetic scalar contracts only. They neither qualify a
// baseline nor activate any future instrument, round or workload.
package model

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// Optional leaves have value equality and independent copy ownership, not
// pointer equality. Historical nil shape and bytes remain unchanged.
func TestCompetitionWorkCoveragePolicyCopiesAndRoundTripsByValue(t *testing.T) {
	coverage := DefaultCompetitionWorkCoveragePolicy()
	original := CompetitionEvaluationPolicy{HardwareId: "synthetic-hardware", WorkCoveragePolicy: &coverage}
	content, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var replay CompetitionEvaluationPolicy
	if err := json.Unmarshal(content, &replay); err != nil || !original.Equal(replay) || original.WorkCoveragePolicy == replay.WorkCoveragePolicy {
		t.Fatal("independently decoded policy lost value equality", err)
	}
	cloned := original.Clone()
	if !cloned.Equal(original) || cloned.WorkCoveragePolicy == original.WorkCoveragePolicy {
		t.Fatal("cloned policy shared its optional mutable leaf")
	}
	cloned.WorkCoveragePolicy.MaximumByteCoverageLoss = .5
	if original.Equal(cloned) || original.WorkCoveragePolicy.MaximumByteCoverageLoss != coverage.MaximumByteCoverageLoss {
		t.Fatal("copy mutation escaped its owned policy")
	}
	replay.WorkCoveragePolicy = nil
	if original.Equal(replay) || !replay.Equal(replay.Clone()) || replay.Clone().WorkCoveragePolicy != nil {
		t.Fatal("nil historical policy was normalized into a prospective leaf")
	}
	receipt := CompetitionReviewedSourceBuildReceipt{Schema: 2, WorkCoveragePolicy: &coverage, SimulatorSha256: strings.Repeat("a", 64)}
	content, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var decodedReceipt CompetitionReviewedSourceBuildReceipt
	if err := json.Unmarshal(content, &decodedReceipt); err != nil || !receipt.Equal(decodedReceipt) || receipt.WorkCoveragePolicy == decodedReceipt.WorkCoveragePolicy {
		t.Fatal("independently decoded build receipt lost policy value identity", err)
	}
	copyReceipt := receipt.Clone()
	copyReceipt.WorkCoveragePolicy.MaximumPageCoverageLoss = .5
	if receipt.Equal(copyReceipt) || receipt.WorkCoveragePolicy.MaximumPageCoverageLoss != coverage.MaximumPageCoverageLoss {
		t.Fatal("returned receipt shared its mutable policy leaf")
	}
}

// The shared scorer/API contract cannot accept a v3 score with a missing
// gate, stripped policy, an old version, or a self-invented tolerance.
func TestCompetitionWorkCoverageScoreRequiresGateAndExactProvenance(t *testing.T) {
	policy := DefaultCompetitionWorkCoveragePolicy()
	makeScore := func() *CompetitionScoreResult {
		score := validCompetitionScoreForTest()
		score.Diagnostics = map[string]any{"g2_volume_policy": string(CompetitionG2VolumePolicyMinimumWork), "scorer_version": CompetitionWorkCoverageScorerVersion, "work_coverage_policy": policy}
		for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources", "G7_work_coverage"} {
			score.Gates[name] = CompetitionGate{Passed: true, Details: map[string]any{}}
		}
		return score
	}
	if err := ValidateCompetitionScore(makeScore()); err != nil {
		t.Fatal("versioned coverage score was rejected", err)
	}
	for _, mutate := range []func(*CompetitionScoreResult){
		func(score *CompetitionScoreResult) { delete(score.Gates, "G7_work_coverage") },
		func(score *CompetitionScoreResult) {
			score.Gates["G7_work_coverage"] = CompetitionGate{Passed: false, Details: map[string]any{}}
		},
		func(score *CompetitionScoreResult) { delete(score.Diagnostics, "work_coverage_policy") },
		func(score *CompetitionScoreResult) { delete(score.Diagnostics, "scorer_version") },
		func(score *CompetitionScoreResult) {
			score.Diagnostics["scorer_version"] = CompetitionMinimumWorkScorerVersion
		},
		func(score *CompetitionScoreResult) { score.Diagnostics["work_coverage_policy"] = nil },
		func(score *CompetitionScoreResult) {
			changed := policy
			changed.MaximumPageCoverageLoss = .5
			score.Diagnostics["work_coverage_policy"] = changed
		},
	} {
		score := makeScore()
		mutate(score)
		if err := ValidateCompetitionScore(score); err == nil {
			t.Fatal("missing or substituted coverage provenance was accepted")
		}
	}
	names, err := CompetitionScoreRequiredGateNames(makeScore())
	if err != nil || len(names) != 7 || names[6] != "G7_work_coverage" {
		t.Fatal("version-aware required gate census was incomplete", err)
	}
}

// Integer totals and matching fractions cannot be replaced by NaN, infinity,
// negative values or a zero denominator.
func TestCompetitionWorkCoverageSummaryRejectsMalformedCounts(t *testing.T) {
	valid := CompetitionWorkCoverageSummary{PlanSha256: strings.Repeat("a", 64), OfferedRoots: 3, ExpectedPages: 100, ExpectedBytes: 1000, CompletedPages: 80, CompletedBytes: 800, PageCoverage: .8, ByteCoverage: .8}
	if err := ValidateCompetitionWorkCoverageSummary(&valid); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CompetitionWorkCoverageSummary){
		func(value *CompetitionWorkCoverageSummary) { value.ExpectedPages = 0 },
		func(value *CompetitionWorkCoverageSummary) { value.OfferedRoots = 0 },
		func(value *CompetitionWorkCoverageSummary) { value.CompletedPages = -1 },
		func(value *CompetitionWorkCoverageSummary) { value.CompletedPages = 101 },
		func(value *CompetitionWorkCoverageSummary) { value.CompletedBytes = 0 },
		func(value *CompetitionWorkCoverageSummary) { value.ByteCoverage = math.Inf(1) },
		func(value *CompetitionWorkCoverageSummary) { value.PageCoverage = math.NaN() },
		func(value *CompetitionWorkCoverageSummary) { value.PlanSha256 = strings.Repeat("A", 64) },
	} {
		changed := valid
		mutate(&changed)
		if err := ValidateCompetitionWorkCoverageSummary(&changed); err == nil {
			t.Fatal("malformed summary was accepted")
		}
	}
}

// The new policy is explicit; old artifacts keep their exact version mapping.
func TestCompetitionWorkCoveragePolicyPreservesHistoricalVersionMappings(t *testing.T) {
	for _, g2 := range []CompetitionG2VolumePolicy{CompetitionG2VolumePolicyLegacy, CompetitionG2VolumePolicyMinimumWork} {
		expected, err := CompetitionScorerVersionForG2Policy(g2)
		actual, coverageErr := CompetitionScorerVersionForWorkCoverage(g2, nil)
		if err != nil || coverageErr != nil || actual != expected {
			t.Fatal("omitted coverage changed historical scorer identity", g2, actual, coverageErr)
		}
	}
	content, err := json.Marshal(CompetitionEvaluationPolicy{})
	if err != nil || bytes.Contains(content, []byte("work_coverage_policy")) {
		t.Fatal("legacy evaluation policy gained coverage bytes", err)
	}
}

// The source contract fixes inclusive one-point losses, not a candidate knob.
func TestCompetitionWorkCoveragePolicyRequiresExactVersionedContract(t *testing.T) {
	policy := DefaultCompetitionWorkCoveragePolicy()
	if version, err := CompetitionScorerVersionForWorkCoverage(CompetitionG2VolumePolicyMinimumWork, &policy); err != nil || version != CompetitionWorkCoverageScorerVersion {
		t.Fatal("explicit versioned coverage contract was rejected", version, err)
	}
	if _, err := CompetitionScorerVersionForWorkCoverage(CompetitionG2VolumePolicyLegacy, &policy); err == nil {
		t.Fatal("coverage silently retained the old upper-volume band")
	}
	for _, mutate := range []func(*CompetitionWorkCoveragePolicy){
		func(value *CompetitionWorkCoveragePolicy) { value.Kind = "unknown/v1" },
		func(value *CompetitionWorkCoveragePolicy) { value.MaximumPageCoverageLoss = 0 },
		func(value *CompetitionWorkCoveragePolicy) { value.MaximumByteCoverageLoss = 0 },
		func(value *CompetitionWorkCoveragePolicy) { value.MaximumPageCoverageLoss = 0.05 },
		func(value *CompetitionWorkCoveragePolicy) { value.MaximumByteCoverageLoss = math.NaN() },
		func(value *CompetitionWorkCoveragePolicy) { value.MaximumPageCoverageLoss = math.Inf(1) },
	} {
		changed := policy
		mutate(&changed)
		if ValidateCompetitionWorkCoveragePolicy(&changed) == nil {
			t.Fatal("changed policy acquired the versioned v1 identity", changed)
		}
	}
}

// A typed object cannot hide duplicate/case aliases, unknown fields, omissions
// or a second value behind otherwise valid frozen coverage policy bytes.
func TestCompetitionWorkCoveragePolicyRejectsAmbiguousWireShapes(t *testing.T) {
	policy := DefaultCompetitionWorkCoveragePolicy()
	content, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CompetitionWorkCoveragePolicy
	if err := json.Unmarshal(content, &decoded); err != nil || decoded != policy {
		t.Fatal("canonical policy did not round trip", err)
	}
	for _, invalid := range [][]byte{
		[]byte("null"), []byte("{}"),
		bytes.Replace(content, []byte(`"kind":`), []byte(`"Kind":`), 1),
		bytes.Replace(content, []byte(`"kind":`), []byte(`"kind":"seeded-crawl-coverage/v1","kind":`), 1),
		append([]byte(`{"allow_missing_work":true,`), content[1:]...),
		append(append([]byte(nil), content...), []byte("{}")...),
		[]byte(`{"kind":"seeded-crawl-coverage/v1","maximum_page_coverage_loss":0.01}`),
	} {
		if err := json.Unmarshal(invalid, &decoded); err == nil {
			t.Fatal("ambiguous policy was accepted", string(invalid))
		}
	}
}
