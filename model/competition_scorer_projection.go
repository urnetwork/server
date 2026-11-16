// Authenticated scorer archives and stored worker scores have distinct shapes.
package model

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Replays the evaluator's successful-score projection before typed decoding.
// The scorer's diagnostic, not a top-level field, owns takeover eligibility;
// absent/null defaults false and non-boolean values fail just as worker decoding
// does. Callers still authenticate original bytes and compare the entire score.
func ProjectCompetitionScorerDocument(content []byte) (*CompetitionScoreResult, error) {
	var document map[string]json.RawMessage
	if json.Unmarshal(content, &document) != nil || document == nil {
		return nil, errors.New("scorer document is malformed")
	}
	if evalError := document["eval_error"]; len(evalError) != 0 && !bytes.Equal(bytes.TrimSpace(evalError), []byte("null")) {
		return nil, errors.New("scorer document contains an evaluation error")
	}
	var diagnostics map[string]json.RawMessage
	if content := document["diagnostics"]; len(content) != 0 && json.Unmarshal(content, &diagnostics) != nil {
		return nil, errors.New("scorer diagnostics are malformed")
	}
	// Select exact keys as evaluator.sh does; unrelated top-level fields cannot
	// affect decoding, even if their names resemble fields of the worker score.
	projection := map[string]json.RawMessage{}
	for _, name := range []string{"score_schema", "raw_score", "normalized_score", "placeable", "gates", "significance", "diagnostics"} {
		projection[name] = document[name]
	}
	eligibility := diagnostics["baseline_takeover_eligible"]
	if len(eligibility) == 0 || bytes.Equal(bytes.TrimSpace(eligibility), []byte("null")) {
		eligibility = json.RawMessage("false")
	}
	projection["takeover_eligible"] = eligibility
	projectedJson, err := json.Marshal(projection)
	if err != nil {
		return nil, errors.New("scorer projection is malformed")
	}
	var score CompetitionScoreResult
	if json.Unmarshal(projectedJson, &score) != nil {
		return nil, errors.New("scorer worker score is malformed")
	}
	return &score, nil
}
