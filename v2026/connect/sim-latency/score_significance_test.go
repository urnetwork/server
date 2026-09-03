package main

import "testing"

func significanceBaseline(values []float64) []ScoreBaselineReplicate {
	replicates := make([]ScoreBaselineReplicate, 0, len(values))
	for _, value := range values {
		replicates = append(replicates, ScoreBaselineReplicate{RawScore: value})
	}
	return replicates
}

func significanceCandidate(values []float64) []candidateReplicate {
	replicates := make([]candidateReplicate, 0, len(values))
	for _, value := range values {
		replicates = append(replicates, candidateReplicate{
			diagnostics: ScoreReplicateDiagnostics{RawScore: value},
		})
	}
	return replicates
}

func TestScoreSignificanceRecordsVarianceAndOneSidedDecision(t *testing.T) {
	baseline := significanceBaseline([]float64{96, 97, 98, 99, 100, 101, 102, 103, 104})
	candidate := significanceCandidate([]float64{76, 77, 78, 79, 80, 81, 82, 83, 84})
	result := scoreSignificance(baseline, candidate, 0.161)

	if result.Method != scoreSignificanceMethod || result.Alpha != 0.05 ||
		result.ReplicateCount != 9 || !result.StatisticallySignificant {
		t.Fatalf("unexpected significance decision: %+v", result)
	}
	if result.BaselineSampleVariance == nil || result.CandidateSampleVariance == nil {
		t.Fatal("sample variance was not recorded")
	}
	almostEqual(t, "baseline sample variance", *result.BaselineSampleVariance, 7.5, 1e-12)
	almostEqual(t, "candidate sample variance", *result.CandidateSampleVariance, 7.5, 1e-12)
	almostEqual(t, "observed improvement", result.ObservedImprovementPercent, 20, 1e-12)
	if result.OneSidedPValue == nil || 1e-8 < *result.OneSidedPValue {
		t.Fatalf("one-sided p-value = %v, want <= 1e-8", result.OneSidedPValue)
	}
	if result.MinimumSignificantImprovementPercent == nil ||
		*result.MinimumSignificantImprovementPercent <= 2 ||
		2.5 <= *result.MinimumSignificantImprovementPercent {
		t.Fatalf("minimum significant improvement = %v", result.MinimumSignificantImprovementPercent)
	}
	if result.RecommendedNextEpochTakeoverMarginPercent == nil ||
		*result.RecommendedNextEpochTakeoverMarginPercent != 16.1 ||
		!result.RecommendedNextEpochTakeoverMarginSupported {
		t.Fatalf("next-epoch recommendation = %+v", result)
	}
}

func TestScoreSignificanceRejectsSmallOrUnmeasurableDraws(t *testing.T) {
	t.Run("small improvement", func(t *testing.T) {
		baseline := significanceBaseline([]float64{96, 97, 98, 99, 100, 101, 102, 103, 104})
		candidate := significanceCandidate([]float64{95, 96, 97, 98, 99, 100, 101, 102, 103})
		result := scoreSignificance(baseline, candidate, 0.161)
		if result.StatisticallySignificant || result.OneSidedPValue == nil ||
			*result.OneSidedPValue <= result.Alpha {
			t.Fatalf("small improvement declared significant: %+v", result)
		}
	})

	t.Run("one replicate", func(t *testing.T) {
		result := scoreSignificance(
			significanceBaseline([]float64{100}),
			significanceCandidate([]float64{50}),
			0.161,
		)
		if result.StatisticallySignificant || result.BaselineSampleVariance != nil ||
			result.CandidateSampleVariance != nil || result.OneSidedPValue != nil ||
			result.RecommendedNextEpochTakeoverMarginPercent != nil {
			t.Fatalf("single replicate claimed significance: %+v", result)
		}
	})
}

func TestScoreSignificanceHandlesZeroAndUnsupportedVariance(t *testing.T) {
	t.Run("zero variance", func(t *testing.T) {
		baseline := significanceBaseline([]float64{100, 100, 100})
		candidate := significanceCandidate([]float64{80, 80, 80})
		result := scoreSignificance(baseline, candidate, 0.161)
		if !result.StatisticallySignificant || result.OneSidedPValue == nil ||
			*result.OneSidedPValue != 0 ||
			result.MinimumSignificantImprovementPercent == nil ||
			*result.MinimumSignificantImprovementPercent != 0 {
			t.Fatalf("zero-variance decision = %+v", result)
		}
	})

	t.Run("next epoch exceeds scorer range", func(t *testing.T) {
		baseline := significanceBaseline([]float64{100, 100, 100, 100, 100, 100, 100, 100, 100})
		candidate := significanceCandidate([]float64{1, 1, 1, 1, 1, 1, 1, 1, 1000})
		result := scoreSignificance(baseline, candidate, 0.161)
		if result.RecommendedNextEpochTakeoverMarginPercent == nil ||
			*result.RecommendedNextEpochTakeoverMarginPercent <= 50 ||
			result.RecommendedNextEpochTakeoverMarginSupported {
			t.Fatalf("unsupported next-epoch margin = %+v", result)
		}
	})
}
