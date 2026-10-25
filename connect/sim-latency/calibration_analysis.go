// File-only release calibration reuses the production reducers. Callers must
// authenticate and replay original artifact bundles before constructing cohorts;
// synthetic bootstrap draws are never labeled as additional measurements.
package main

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
)

// Owns the predeclared analysis policy; tests may select smaller draw counts.
type calibrationAnalysisSettings struct {
	Cohorts                 int
	Replicates              int
	BootstrapDraws          int
	MinimumPlaceability     float64
	MarginFloor             float64
	MaximumMargin           float64
	NoiseMultiplier         float64
	MarginRoundingScale     float64
	SeedA                   uint64
	SeedB                   uint64
	MaximumEvidenceBindings int
	MaximumArtifactBytes    int64
	MaximumCohortBytes      int64
	ExecutionLimitSeconds   int
	MaximumTraversalEntries int
}

// Matches the draft release protocol, not a change to runtime scoring policy.
func defaultCalibrationAnalysisSettings() calibrationAnalysisSettings {
	return calibrationAnalysisSettings{
		Cohorts: 5, Replicates: 9, BootstrapDraws: 100000,
		MinimumPlaceability: 0.94, MarginFloor: 0.161, MaximumMargin: 0.50,
		NoiseMultiplier: 4, MarginRoundingScale: 1000,
		SeedA: 0x20261025, SeedB: 0x9,
		MaximumEvidenceBindings: 4096, MaximumArtifactBytes: 128 * 1024 * 1024,
		MaximumCohortBytes:      1024 * 1024 * 1024,
		ExecutionLimitSeconds:   10800,
		MaximumTraversalEntries: 16384,
	}
}

// Keeps an observed failure in its declared position, rather than replacing it
// with a successful cohort. Successful inputs must already be authenticated.
type calibrationAnalysisCohort struct {
	FailureCode          string
	Baseline             *ScoreBaselineManifest
	BaselineSha256       string
	ControlEvaluationIds []string
	Candidates           []candidateReplicate
	Result               *ScoreResult
}

// Separates validated original records, simulated draws and release acceptance.
type calibrationAnalysisReport struct {
	Kind                   string                         `json:"kind"`
	Cohorts                int                            `json:"cohorts"`
	ObservedReplicates     int                            `json:"validated_observed_replicates"`
	BootstrapDraws         int                            `json:"bootstrap_draws"`
	Placeability           float64                        `json:"bootstrap_placeability"`
	GatePassRates          map[string]float64             `json:"bootstrap_gate_pass_rates"`
	MarginTerms            map[string]float64             `json:"margin_terms"`
	RecommendedMargin      float64                        `json:"recommended_margin"`
	Accepted               bool                           `json:"accepted"`
	Reasons                []string                       `json:"reasons"`
	InputManifestSha256    string                         `json:"input_manifest_sha256,omitempty"`
	EvaluationPolicySha256 string                         `json:"evaluation_policy_sha256,omitempty"`
	ReplayObservations     []calibrationReplayObservation `json:"replay_observations,omitempty"`
}

// Rejects malformed input instead of letting NaN pass comparisons or sorting.
func validateCalibrationAnalysisCohort(cohort calibrationAnalysisCohort, settings calibrationAnalysisSettings) error {
	if cohort.Baseline == nil || cohort.Result == nil ||
		len(cohort.Baseline.Replicates) != settings.Replicates || len(cohort.Candidates) != settings.Replicates ||
		len(cohort.ControlEvaluationIds) != settings.Replicates {
		return errors.New("calibration requires a complete exact-replicate cohort")
	}
	for _, evaluationId := range cohort.ControlEvaluationIds {
		if evaluationId == "" {
			return errors.New("calibration control is missing its execution identity")
		}
	}
	baseline := cohort.Baseline
	result := cohort.Result
	if baseline.ScoreSchema != apexScoreSchema || baseline.Kind != scoreBaselineKind ||
		baseline.ScorerVersion != officialScorerVersion || baseline.RoundId == "" ||
		!validSha256(baseline.ConfigSha256) || !validSha256(cohort.BaselineSha256) ||
		baseline.TakeoverMargin != settings.MarginFloor || result.ScoreSchema != apexScoreSchema ||
		result.Diagnostics.RoundId != baseline.RoundId || result.Diagnostics.ReplicateCount != settings.Replicates ||
		result.Diagnostics.BaselineSha256 != cohort.BaselineSha256 || len(result.Diagnostics.Replicates) != settings.Replicates {
		return errors.New("calibration cohort contract or round identity mismatch")
	}
	for _, replicate := range baseline.Replicates {
		for _, value := range []float64{replicate.RawScore, replicate.SuccessRate, replicate.FindProvidersLoadP95Ms,
			replicate.FindProvidersPoolP05, replicate.FindProvidersSampleSpanFraction} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return errors.New("calibration control contains invalid numeric data")
			}
		}
		if replicate.RawScore <= 0 || replicate.SuccessRate > 1 || replicate.RequestCount <= 0 ||
			replicate.ReceivedBytes <= 0 || replicate.FindProvidersSampleSpanFraction > 1 {
			return errors.New("calibration control diagnostics are incomplete")
		}
	}
	for i, replicate := range cohort.Candidates {
		diagnostic := replicate.diagnostics
		for _, value := range []float64{diagnostic.RawScore, diagnostic.SuccessRate, diagnostic.AccountingCoverage,
			diagnostic.FindProvidersLoadP95Ms, diagnostic.FindProvidersPoolP05, diagnostic.FindProvidersSampleSpanFraction} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return errors.New("calibration candidate contains invalid numeric data")
			}
		}
		if diagnostic.EvaluationId == "" || diagnostic.RawScore <= 0 || diagnostic.SuccessRate > 1 ||
			diagnostic.RequestCount <= 0 || diagnostic.ReceivedBytes <= 0 || diagnostic.FindProvidersSampleSpanFraction > 1 ||
			diagnostic.FindProvidersSamples < 0 || diagnostic.FindProvidersEmptyPools < 0 ||
			!reflect.DeepEqual(result.Diagnostics.Replicates[i], diagnostic) {
			return errors.New("calibration candidate diagnostics are incomplete")
		}
	}
	base := aggregateBaseline(baseline.Replicates)
	candidate := aggregateCandidate(cohort.Candidates)
	gates := buildScoreGates(base, candidate, cohort.Candidates)
	if result.RawScore != candidate.RawScore || result.Placeable != allScoreGatesPass(gates) ||
		!reflect.DeepEqual(result.Diagnostics.Baseline, &base.ScoreAggregateDiagnostics) ||
		!reflect.DeepEqual(result.Diagnostics.Candidate, &candidate) ||
		!reflect.DeepEqual(result.Significance, scoreSignificance(baseline.Replicates, cohort.Candidates, baseline.TakeoverMargin)) {
		return errors.New("calibration cohort does not reproduce production score")
	}
	for name, gate := range gates {
		retained, ok := result.Gates[name]
		if !ok || retained.Passed != gate.Passed {
			return errors.New("calibration cohort does not reproduce production gates")
		}
	}
	return nil
}

// Uses within-cohort independent R9 resampling, preserving between-cohort
// differences. It never substitutes a successful subset for declared failures.
func analyzeCalibrationCohorts(cohorts []calibrationAnalysisCohort, settings calibrationAnalysisSettings) (*calibrationAnalysisReport, error) {
	if settings.Cohorts <= 0 || settings.Replicates < 3 || settings.Replicates%2 == 0 || settings.BootstrapDraws < 2 ||
		len(cohorts) != settings.Cohorts {
		return nil, errors.New("invalid calibration sample cardinality")
	}
	for _, value := range []float64{settings.MinimumPlaceability, settings.MarginFloor, settings.MaximumMargin,
		settings.NoiseMultiplier, settings.MarginRoundingScale} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
			return nil, errors.New("invalid calibration policy number")
		}
	}
	if settings.MinimumPlaceability > 1 || settings.MarginFloor > settings.MaximumMargin || settings.MaximumMargin > 0.50 ||
		settings.MarginRoundingScale < 1 || math.Trunc(settings.MarginRoundingScale) != settings.MarginRoundingScale {
		return nil, errors.New("unsupported calibration policy")
	}
	report := &calibrationAnalysisReport{
		Kind: "sim-latency-calibration-analysis", Cohorts: len(cohorts),
		GatePassRates: map[string]float64{}, MarginTerms: map[string]float64{"preparation_floor": settings.MarginFloor},
		Reasons: []string{},
	}
	for i, cohort := range cohorts {
		if cohort.FailureCode != "" {
			report.Reasons = append(report.Reasons, "declared_cohort_failed")
			continue
		}
		if err := validateCalibrationAnalysisCohort(cohort, settings); err != nil {
			return nil, fmt.Errorf("cohort %d: %w", i+1, err)
		}
		report.ObservedReplicates += len(cohort.Baseline.Replicates) + len(cohort.Candidates)
	}
	if len(report.Reasons) != 0 {
		return report, nil
	}
	roundIds := map[string]bool{}
	evaluationIds := map[string]bool{}
	for i, cohort := range cohorts {
		baseline := cohort.Baseline
		if roundIds[baseline.RoundId] {
			return nil, errors.New("calibration repeats a cohort round")
		}
		roundIds[baseline.RoundId] = true
		for _, evaluationId := range cohort.ControlEvaluationIds {
			if evaluationIds[evaluationId] {
				return nil, errors.New("calibration reuses a control execution")
			}
			evaluationIds[evaluationId] = true
		}
		first := cohorts[0].Baseline
		if baseline.ConfigSha256 != first.ConfigSha256 || baseline.RequestTimeoutMs != first.RequestTimeoutMs ||
			!reflect.DeepEqual(baseline.RunFlags, first.RunFlags) {
			return nil, errors.New("calibration mixes workload contracts")
		}
		for _, replicate := range cohort.Candidates {
			if evaluationIds[replicate.diagnostics.EvaluationId] {
				return nil, errors.New("calibration reuses a candidate execution")
			}
			evaluationIds[replicate.diagnostics.EvaluationId] = true
		}
		if !cohort.Result.Placeable || cohort.Result.EvalError != nil {
			report.Reasons = append(report.Reasons, "observed_noop_not_placeable")
		}
		significance := cohort.Result.Significance
		if !significance.RecommendedNextEpochTakeoverMarginSupported || significance.RecommendedNextEpochTakeoverMarginPercent == nil ||
			significance.RequiredImprovementPercent == nil {
			return nil, errors.New("calibration lacks a supported measured margin")
		}
		report.MarginTerms[fmt.Sprintf("cohort_%d_recommendation", i+1)] = *significance.RecommendedNextEpochTakeoverMarginPercent / 100
		report.MarginTerms[fmt.Sprintf("cohort_%d_required", i+1)] = *significance.RequiredImprovementPercent / 100
		if i == 0 {
			for name := range cohort.Result.Gates {
				report.GatePassRates[name] = 0
			}
		}
	}
	random := rand.New(rand.NewPCG(settings.SeedA, settings.SeedB))
	controlScores := make([]float64, 0, settings.BootstrapDraws)
	noopScores := make([]float64, 0, settings.BootstrapDraws)
	noopImprovements := make([]float64, 0, settings.BootstrapDraws)
	controls := make([]ScoreBaselineReplicate, settings.Replicates)
	candidates := make([]candidateReplicate, settings.Replicates)
	for draw := 0; draw < settings.BootstrapDraws; draw++ {
		cohort := cohorts[random.IntN(len(cohorts))]
		for i := range controls {
			controls[i] = cohort.Baseline.Replicates[random.IntN(settings.Replicates)]
			candidates[i] = cohort.Candidates[random.IntN(settings.Replicates)]
		}
		baseline := aggregateBaseline(controls)
		candidate := aggregateCandidate(candidates)
		gates := buildScoreGates(baseline, candidate, candidates)
		for name, gate := range gates {
			if gate.Passed {
				report.GatePassRates[name]++
			}
		}
		if allScoreGatesPass(gates) {
			report.Placeability++
		}
		controlScores = append(controlScores, baseline.RawScore)
		noopScores = append(noopScores, candidate.RawScore)
		noopImprovements = append(noopImprovements, math.Max(0, 1-candidate.RawScore/baseline.RawScore))
	}
	report.BootstrapDraws = settings.BootstrapDraws
	report.Placeability /= float64(settings.BootstrapDraws)
	for name := range report.GatePassRates {
		report.GatePassRates[name] /= float64(settings.BootstrapDraws)
	}
	controlMean, controlSd := meanStd(controlScores)
	noopMean, noopSd := meanStd(noopScores)
	report.MarginTerms["control_noise"] = settings.NoiseMultiplier * controlSd / controlMean
	report.MarginTerms["noop_noise"] = settings.NoiseMultiplier * noopSd / noopMean
	report.MarginTerms["noop_improvement_p95"] = quantile(noopImprovements, 0.95)
	for _, margin := range report.MarginTerms {
		report.RecommendedMargin = math.Max(report.RecommendedMargin, margin)
	}
	unroundedMargin := report.RecommendedMargin
	report.RecommendedMargin = math.Ceil(unroundedMargin*settings.MarginRoundingScale) / settings.MarginRoundingScale
	if report.RecommendedMargin < unroundedMargin {
		report.RecommendedMargin += 1 / settings.MarginRoundingScale
	}
	if report.RecommendedMargin > settings.MaximumMargin {
		report.Reasons = append(report.Reasons, "required_margin_unsupported")
	}
	if report.Placeability < settings.MinimumPlaceability {
		report.Reasons = append(report.Reasons, "bootstrap_placeability_below_target")
	}
	for _, cohort := range cohorts {
		significance := scoreSignificance(cohort.Baseline.Replicates, cohort.Candidates, report.RecommendedMargin)
		baseline := aggregateBaseline(cohort.Baseline.Replicates)
		candidate := aggregateCandidate(cohort.Candidates)
		if cohort.Result.Placeable && candidate.RawScore <= baseline.RawScore*(1-report.RecommendedMargin) &&
			significance.StatisticallySignificant && significance.RecommendedNextEpochTakeoverMarginSupported {
			report.Reasons = append(report.Reasons, "observed_noop_takeover_under_selected_margin")
		}
	}
	report.Accepted = len(report.Reasons) == 0
	return report, nil
}
