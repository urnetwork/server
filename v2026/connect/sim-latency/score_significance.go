package main

import "math"

const scoreSignificanceAlpha = 0.05

const scoreSignificanceMethod = "one-sided-welch-t"

// ScoreSignificance records the complete run-level variance and one-sided
// significance calculation for one candidate evaluation. Percent fields use
// 100 for one hundred percent; raw-score variance is sample variance (n-1).
type ScoreSignificance struct {
	Method                                      string   `json:"method"`
	Alpha                                       float64  `json:"alpha"`
	ReplicateCount                              int      `json:"replicate_count"`
	BaselineMeanRawScore                        float64  `json:"baseline_mean_raw_score"`
	CandidateMeanRawScore                       float64  `json:"candidate_mean_raw_score"`
	BaselineSampleVariance                      *float64 `json:"baseline_sample_variance"`
	CandidateSampleVariance                     *float64 `json:"candidate_sample_variance"`
	ObservedImprovementPercent                  float64  `json:"observed_improvement_percent"`
	TakeoverMarginPercent                       float64  `json:"takeover_margin_percent"`
	MinimumSignificantImprovementPercent        *float64 `json:"minimum_significant_improvement_percent"`
	RequiredImprovementPercent                  *float64 `json:"required_improvement_percent"`
	OneSidedPValue                              *float64 `json:"one_sided_p_value"`
	WelchT                                      *float64 `json:"welch_t,omitempty"`
	WelchDegreesOfFreedom                       *float64 `json:"welch_degrees_of_freedom,omitempty"`
	StatisticallySignificant                    bool     `json:"statistically_significant"`
	NextEpochMinimumImprovementPercent          *float64 `json:"next_epoch_minimum_improvement_percent"`
	RecommendedNextEpochTakeoverMarginPercent   *float64 `json:"recommended_next_epoch_takeover_margin_percent"`
	RecommendedNextEpochTakeoverMarginSupported bool     `json:"recommended_next_epoch_takeover_margin_supported"`
}

// scoreSignificance compares baseline and candidate run-level raw scores. The
// current evaluation uses a one-sided Welch test because the two sides may
// have different variance. The next-epoch recommendation treats the winning
// candidate variance as the new incumbent noise floor at the same replicate
// count and never weakens the already calibrated takeover margin.
func scoreSignificance(
	baselineReplicates []ScoreBaselineReplicate,
	candidateReplicates []candidateReplicate,
	takeoverMargin float64,
) *ScoreSignificance {
	baselineValues := make([]float64, 0, len(baselineReplicates))
	for _, replicate := range baselineReplicates {
		baselineValues = append(baselineValues, replicate.RawScore)
	}
	candidateValues := make([]float64, 0, len(candidateReplicates))
	for _, replicate := range candidateReplicates {
		candidateValues = append(candidateValues, replicate.diagnostics.RawScore)
	}

	baselineMean, baselineSd := meanStd(baselineValues)
	candidateMean, candidateSd := meanStd(candidateValues)
	result := &ScoreSignificance{
		Method:                     scoreSignificanceMethod,
		Alpha:                      scoreSignificanceAlpha,
		ReplicateCount:             len(candidateValues),
		BaselineMeanRawScore:       baselineMean,
		CandidateMeanRawScore:      candidateMean,
		ObservedImprovementPercent: (baselineMean - candidateMean) / baselineMean * 100,
		TakeoverMarginPercent:      takeoverMargin * 100,
		StatisticallySignificant:   false,
		RecommendedNextEpochTakeoverMarginSupported: false,
	}
	if len(baselineValues) < 2 || len(candidateValues) < 2 {
		return result
	}

	baselineVariance := baselineSd * baselineSd
	candidateVariance := candidateSd * candidateSd
	result.BaselineSampleVariance = &baselineVariance
	result.CandidateSampleVariance = &candidateVariance

	standardError := math.Sqrt(
		baselineVariance/float64(len(baselineValues)) +
			candidateVariance/float64(len(candidateValues)),
	)
	if 0 < standardError {
		t, degreesOfFreedom, ok := welch(baselineValues, candidateValues)
		if ok {
			pValue := studentTSf(t, degreesOfFreedom)
			critical := studentTCrit(scoreSignificanceAlpha, degreesOfFreedom)
			minimumPercent := critical * standardError / baselineMean * 100
			requiredPercent := math.Max(result.TakeoverMarginPercent, minimumPercent)
			result.OneSidedPValue = &pValue
			result.WelchT = &t
			result.WelchDegreesOfFreedom = &degreesOfFreedom
			result.MinimumSignificantImprovementPercent = &minimumPercent
			result.RequiredImprovementPercent = &requiredPercent
			result.StatisticallySignificant = 0 < result.ObservedImprovementPercent &&
				pValue <= scoreSignificanceAlpha
		}
	} else {
		pValue := 0.5
		if candidateMean < baselineMean {
			pValue = 0
		} else if baselineMean < candidateMean {
			pValue = 1
		}
		minimumPercent := 0.0
		requiredPercent := result.TakeoverMarginPercent
		result.OneSidedPValue = &pValue
		result.MinimumSignificantImprovementPercent = &minimumPercent
		result.RequiredImprovementPercent = &requiredPercent
		result.StatisticallySignificant = pValue <= scoreSignificanceAlpha
	}

	nextEpochMinimumPercent := 0.0
	if 0 < candidateSd {
		critical := studentTCrit(scoreSignificanceAlpha, float64(len(candidateValues)-1))
		nextEpochMinimumPercent = critical * candidateSd *
			math.Sqrt(2/float64(len(candidateValues))) / candidateMean * 100
	}
	recommendedPercent := math.Max(result.TakeoverMarginPercent, nextEpochMinimumPercent)
	result.NextEpochMinimumImprovementPercent = &nextEpochMinimumPercent
	result.RecommendedNextEpochTakeoverMarginPercent = &recommendedPercent
	result.RecommendedNextEpochTakeoverMarginSupported = 0 < recommendedPercent && recommendedPercent <= 50
	return result
}
