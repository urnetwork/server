// Synthetic calibration checks exercise production reducers without services,
// sleeps, production identities or treating bootstrap draws as measurements.
package main

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// Constructs independent, healthy R9 cohorts with constant scores.
func newCalibrationAnalysisFixture(settings calibrationAnalysisSettings) []calibrationAnalysisCohort {
	cohorts := make([]calibrationAnalysisCohort, settings.Cohorts)
	for i := range cohorts {
		cohort := &cohorts[i]
		cohort.BaselineSha256 = strings.Repeat(fmt.Sprint(i+1), 64)
		cohort.Baseline = &ScoreBaselineManifest{
			ScoreSchema: apexScoreSchema, Kind: scoreBaselineKind, ScorerVersion: officialScorerVersion,
			RoundId: fmt.Sprintf("synthetic-round-%d", i), ConfigSha256: strings.Repeat("a", 64),
			RequestTimeoutMs: 120000, TakeoverMargin: settings.MarginFloor,
			RunFlags: map[string]string{"duration": "3m", "impairment": "true"},
		}
		for j := 0; j < settings.Replicates; j++ {
			cohort.ControlEvaluationIds = append(cohort.ControlEvaluationIds, fmt.Sprintf("synthetic-control-%d-%d", i, j))
			cohort.Baseline.Replicates = append(cohort.Baseline.Replicates, ScoreBaselineReplicate{
				RawScore: 100, SuccessRate: 1, RequestCount: 100, ReceivedBytes: 10000,
				FindProvidersLoadP95Ms: 10, FindProvidersPoolP05: 100, FindProvidersSampleSpanFraction: 0.95,
			})
			cohort.Candidates = append(cohort.Candidates, candidateReplicate{
				diagnostics: ScoreReplicateDiagnostics{
					EvaluationId: fmt.Sprintf("synthetic-candidate-%d-%d", i, j),
					RawScore:     100, SuccessRate: 1, RequestCount: 100, ReceivedBytes: 10000,
					AccountingCoverage: 1, FindProvidersSamples: 4, FindProvidersLoadP95Ms: 10,
					FindProvidersPoolP05: 100, FindProvidersSampleSpanFraction: 0.95,
				},
				stabilityOK: true, resourcesOK: true,
			})
		}
		rebuildCalibrationAnalysisFixture(cohort)
	}
	return cohorts
}

// Recomputes expected synthetic output after a test changes one input axis.
func rebuildCalibrationAnalysisFixture(cohort *calibrationAnalysisCohort) {
	baseline := aggregateBaseline(cohort.Baseline.Replicates)
	candidate := aggregateCandidate(cohort.Candidates)
	gates := buildScoreGates(baseline, candidate, cohort.Candidates)
	cohort.Result = &ScoreResult{
		ScoreSchema: apexScoreSchema, RawScore: candidate.RawScore, Placeable: allScoreGatesPass(gates),
		Gates: gates, Significance: scoreSignificance(cohort.Baseline.Replicates, cohort.Candidates, cohort.Baseline.TakeoverMargin),
		Diagnostics: ScoreDiagnostics{
			RoundId: cohort.Baseline.RoundId, BaselineSha256: cohort.BaselineSha256,
			ReplicateCount: len(cohort.Candidates), Baseline: &baseline.ScoreAggregateDiagnostics, Candidate: &candidate,
		},
	}
	for _, replicate := range cohort.Candidates {
		cohort.Result.Diagnostics.Replicates = append(cohort.Result.Diagnostics.Replicates, replicate.diagnostics)
	}
}

// Fixed RNG inputs reproduce identical results while preserving actual counts.
func TestCalibrationAnalysisIsDeterministicAndKeepsObservedCounts(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	settings.BootstrapDraws = 128
	cohorts := newCalibrationAnalysisFixture(settings)
	first, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil {
		t.Fatal(err)
	}
	second, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("nonrepeatable bootstrap: %v", err)
	}
	if !first.Accepted || first.ObservedReplicates != 90 || first.BootstrapDraws != 128 ||
		first.Placeability != 1 || first.RecommendedMargin != settings.MarginFloor || len(first.GatePassRates) != 6 {
		t.Fatalf("unexpected constant-score analysis: %+v", first)
	}
	for name, rate := range first.GatePassRates {
		if rate != 1 {
			t.Errorf("gate %s rate = %v", name, rate)
		}
	}
}

// An absent failed cohort remains part of the declared sample, never a smaller
// all-successful bootstrap that can pass a launch gate.
func TestCalibrationAnalysisRetainsDeclaredIncompleteCohort(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	settings.BootstrapDraws = 64
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[0] = calibrationAnalysisCohort{FailureCode: "synthetic_infrastructure_failure"}
	report, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil || report.Accepted || report.Cohorts != 5 || report.ObservedReplicates != 72 ||
		report.BootstrapDraws != 0 || len(report.Reasons) != 1 {
		t.Fatalf("failed cohort was discarded: %+v, %v", report, err)
	}
}

// A failed cohort cannot bypass validation of other records while the report
// labels their observations validated.
func TestCalibrationAnalysisValidatesOtherCohortsDespiteDeclaredFailure(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[0] = calibrationAnalysisCohort{FailureCode: "synthetic_infrastructure_failure"}
	cohorts[1].Candidates[0].diagnostics.RawScore = math.NaN()
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("declared failure bypassed validation of another cohort")
	}
}

// Partial observations from a failed cohort are retained in original evidence,
// not added to the count of validated complete R9 cohorts.
func TestCalibrationAnalysisDoesNotCountFailedPartialObservationsAsValidated(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[0].FailureCode = "synthetic_infrastructure_failure"
	cohorts[0].Candidates = cohorts[0].Candidates[:1]
	report, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil || report.Accepted || report.ObservedReplicates != 72 || report.BootstrapDraws != 0 {
		t.Fatalf("failed partial observations labeled validated: %+v, %v", report, err)
	}
}

// Missing one observed candidate cannot silently become an R8 evaluation.
func TestCalibrationAnalysisRejectsReducedReplicateCount(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[0].Candidates = cohorts[0].Candidates[:8]
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("reduced replicate count accepted")
	}
}

// Nonfinite values never pass sorting/comparisons as healthy observations.
func TestCalibrationAnalysisRejectsNonfiniteObservations(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	for _, candidate := range []bool{false, true} {
		cohorts := newCalibrationAnalysisFixture(settings)
		if candidate {
			cohorts[0].Candidates[0].diagnostics.RawScore = math.Inf(1)
		} else {
			cohorts[0].Baseline.Replicates[0].RawScore = math.NaN()
		}
		if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
			t.Errorf("nonfinite candidate=%v accepted", candidate)
		}
	}
}

// A round identity is part of the cohort boundary, not a replaceable label.
func TestCalibrationAnalysisRejectsRepeatedRound(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[1].Baseline.RoundId = cohorts[0].Baseline.RoundId
	rebuildCalibrationAnalysisFixture(&cohorts[1])
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("repeated round accepted")
	}
}

// Copying a candidate into another cohort cannot increase the observed sample.
func TestCalibrationAnalysisRejectsRepeatedCandidateIdentity(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[1].Candidates[0].diagnostics.EvaluationId = cohorts[0].Candidates[0].diagnostics.EvaluationId
	rebuildCalibrationAnalysisFixture(&cohorts[1])
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("repeated candidate execution accepted")
	}
}

// Fresh request/round labels cannot turn an old control execution into a new
// measurement, even when its raw statistics and rebuilt baseline are valid.
func TestCalibrationAnalysisRejectsRepeatedControlIdentity(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	settings.BootstrapDraws = 64
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[1].ControlEvaluationIds[0] = cohorts[0].ControlEvaluationIds[0]
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("reused control execution accepted as a fresh cohort")
	}
}

// The same observation cannot serve as both a control and a candidate.
func TestCalibrationAnalysisRejectsControlCandidateIdentityReuse(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	settings.BootstrapDraws = 64
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[0].ControlEvaluationIds[0] = cohorts[0].Candidates[0].diagnostics.EvaluationId
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("same execution accepted on both sides of a comparison")
	}
}

// Every successful control needs its original sidecar identity, not just a
// plausible score value and a count of nine rows.
func TestCalibrationAnalysisRequiresCompleteControlIdentities(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	for _, missing := range []bool{false, true} {
		cohorts := newCalibrationAnalysisFixture(settings)
		if missing {
			cohorts[0].ControlEvaluationIds = cohorts[0].ControlEvaluationIds[:8]
		} else {
			cohorts[0].ControlEvaluationIds[0] = ""
		}
		if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
			t.Errorf("incomplete control identities accepted; missing=%v", missing)
		}
	}
}

// Same-shaped control statistics do not authorize a different baseline blob.
func TestCalibrationAnalysisRejectsChangedBaselineIdentity(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[0].BaselineSha256 = strings.Repeat("f", 64)
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("changed baseline identity accepted")
	}
}

// Scorer output must be reproduced, not trusted because its verdict is green.
func TestCalibrationAnalysisRejectsForgedProductionGate(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	cohorts := newCalibrationAnalysisFixture(settings)
	gate := cohorts[0].Result.Gates["G4_matchmaking"]
	gate.Passed = false
	cohorts[0].Result.Gates["G4_matchmaking"] = gate
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("unreproduced gate accepted")
	}
}

// The actual G2 rejection survives resampling instead of being dropped as noise.
func TestCalibrationAnalysisPreservesProductionGateFailure(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	settings.BootstrapDraws = 64
	cohorts := newCalibrationAnalysisFixture(settings)
	for i := range cohorts {
		for j := range cohorts[i].Candidates {
			cohorts[i].Candidates[j].diagnostics.RequestCount = 200
		}
		rebuildCalibrationAnalysisFixture(&cohorts[i])
	}
	report, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil || report.Accepted || report.Placeability != 0 {
		t.Fatalf("G2 failures became accepted: %+v, %v", report, err)
	}
	if rate, ok := report.GatePassRates["G2_volume"]; !ok || rate != 0 {
		t.Fatalf("failed gate missing from report: %+v", report.GatePassRates)
	}
}

// Between-cohort differences remain in the estimate; the margin is not clamped
// into a supported range to manufacture release acceptance.
func TestCalibrationAnalysisRetainsBetweenCohortNoiseAndUnsupportedMargin(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	settings.BootstrapDraws = 128
	cohorts := newCalibrationAnalysisFixture(settings)
	for i := range cohorts {
		for j := range cohorts[i].Candidates {
			value := float64(100 * (i + 1))
			cohorts[i].Candidates[j].diagnostics.RawScore = value
			cohorts[i].Baseline.Replicates[j].RawScore = value
		}
		rebuildCalibrationAnalysisFixture(&cohorts[i])
	}
	report, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil || report.Accepted || report.RecommendedMargin <= settings.MaximumMargin {
		t.Fatalf("between-cohort noise hidden: %+v, %v", report, err)
	}
}

// Decimal rounding must never weaken the selected floor.
func TestCalibrationAnalysisRoundsMarginUp(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	settings.BootstrapDraws = 64
	settings.MarginFloor = 0.1611
	report, err := analyzeCalibrationCohorts(newCalibrationAnalysisFixture(settings), settings)
	if err != nil || report.RecommendedMargin != 0.162 || report.RecommendedMargin < settings.MarginFloor {
		t.Fatalf("margin rounding weakened the floor: %+v, %v", report, err)
	}
}

// New workload flags do not become additional samples of the old environment.
func TestCalibrationAnalysisRejectsMixedWorkloadContracts(t *testing.T) {
	settings := defaultCalibrationAnalysisSettings()
	cohorts := newCalibrationAnalysisFixture(settings)
	cohorts[1].Baseline.RunFlags["impairment"] = "false"
	if _, err := analyzeCalibrationCohorts(cohorts, settings); err == nil {
		t.Fatal("mixed workload contracts accepted")
	}
}
