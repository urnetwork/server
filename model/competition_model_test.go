package model

import (
	"encoding/json"
	"testing"
)

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
