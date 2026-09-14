package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// Builds a complete replicated score at the shared scorer/API boundary.
func validCompetitionScoreForTest() *CompetitionScoreResult {
	rawScore := 80.0
	normalizedScore := 125.0
	baselineVariance := 4.0
	candidateVariance := 3.0
	minimumPercent := 3.0
	requiredPercent := 16.1
	pValue := 0.01
	welchT := 4.0
	degreesOfFreedom := 14.0
	nextEpochMinimumPercent := 2.0
	recommendedPercent := 16.1
	return &CompetitionScoreResult{
		ScoreSchema:      CompetitionScoreSchema,
		RawScore:         &rawScore,
		NormalizedScore:  &normalizedScore,
		Placeable:        true,
		TakeoverEligible: true,
		Gates: map[string]CompetitionGate{
			"G1_success": {Passed: true, Details: map[string]any{}},
		},
		Significance: &CompetitionScoreSignificance{
			Method:                                      "one-sided-welch-t",
			Alpha:                                       0.05,
			ReplicateCount:                              9,
			BaselineMeanRawScore:                        100,
			CandidateMeanRawScore:                       80,
			BaselineSampleVariance:                      &baselineVariance,
			CandidateSampleVariance:                     &candidateVariance,
			ObservedImprovementPercent:                  20,
			TakeoverMarginPercent:                       16.1,
			MinimumSignificantImprovementPercent:        &minimumPercent,
			RequiredImprovementPercent:                  &requiredPercent,
			OneSidedPValue:                              &pValue,
			WelchT:                                      &welchT,
			WelchDegreesOfFreedom:                       &degreesOfFreedom,
			StatisticallySignificant:                    true,
			NextEpochMinimumImprovementPercent:          &nextEpochMinimumPercent,
			RecommendedNextEpochTakeoverMarginPercent:   &recommendedPercent,
			RecommendedNextEpochTakeoverMarginSupported: true,
		},
	}
}

// The epoch-three incident produced a superficially valid score with no
// significance record. Keep that exact contract omission terminal.
func TestCompetitionScoreRejectsMissingSignificance(t *testing.T) {
	score := validCompetitionScoreForTest()
	score.Significance = nil
	if err := ValidateCompetitionScore(score); err == nil ||
		!strings.Contains(err.Error(), "significance metadata") {
		t.Fatalf("missing significance error = %v", err)
	}
}

// Adjacent score fields must not provide alternate ways to publish a result
// whose statistical decision is incomplete or contradictory.
func TestCompetitionScoreRejectsAdjacentContractDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CompetitionScoreResult)
		want   string
	}{
		{
			name: "missing gates",
			mutate: func(score *CompetitionScoreResult) {
				score.Gates = nil
			},
			want: "gates are missing",
		},
		{
			name: "even replicate count",
			mutate: func(score *CompetitionScoreResult) {
				score.Significance.ReplicateCount = 8
			},
			want: "significance metadata",
		},
		{
			name: "contradictory p value",
			mutate: func(score *CompetitionScoreResult) {
				pValue := 0.5
				score.Significance.OneSidedPValue = &pValue
			},
			want: "significance decision",
		},
		{
			name: "ineligible significance",
			mutate: func(score *CompetitionScoreResult) {
				score.Significance.StatisticallySignificant = false
				pValue := 0.5
				score.Significance.OneSidedPValue = &pValue
			},
			want: "takeover eligibility",
		},
	}
	for _, test := range tests {
		score := validCompetitionScoreForTest()
		test.mutate(score)
		if err := ValidateCompetitionScore(score); err == nil ||
			!strings.Contains(err.Error(), test.want) {
			t.Errorf("%s error = %v, want %q", test.name, err, test.want)
		}
	}
}

// The shared transport model must retain the public API field names while its
// Go identifiers stay distinct from the rest of the model package.
func TestCompetitionInfoResultUsesCompetitionSchema(t *testing.T) {
	encoded, err := json.Marshal(CompetitionInfoResult{
		CompetitionId: "sim-latency",
		ScoreSchema:   CompetitionScoreSchema,
		ScorerVersion: CompetitionScorerVersion,
		PatchPolicy: CompetitionPatchPolicy{
			MaxPatchBytes: 1024,
		},
		EvaluationPolicy: CompetitionEvaluationPolicy{
			ScoreTimeoutSeconds: 3 * 60 * 60,
		},
		SeasonPolicy: CompetitionSeasonPolicy{
			EpochCount:       6,
			SubmissionFeeUsd: 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"competition_id",
		"score_schema",
		"scorer_version",
		"patch_policy",
		"evaluation_policy",
		"season_policy",
	} {
		if _, ok := result[field]; !ok {
			t.Errorf("competition info is missing %q", field)
		}
	}
	if len(result) != 9 {
		t.Fatalf("competition info field count = %d, want 9: %s", len(result), encoded)
	}
}

// A nil error remains safe for handlers that expose an optional evaluation
// failure, while concrete errors keep the stable code-prefixed diagnostic.
func TestCompetitionErrorMessage(t *testing.T) {
	var nilError *CompetitionError
	if nilError.Error() != "" {
		t.Fatalf("nil competition error = %q, want empty", nilError.Error())
	}

	evaluationError := &CompetitionError{
		Code:    "evaluation_timeout",
		Message: "evaluation exceeded three hours",
	}
	if got := evaluationError.Error(); got != "evaluation_timeout: evaluation exceeded three hours" {
		t.Fatalf("competition error = %q", got)
	}
}
