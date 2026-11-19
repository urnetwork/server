// Synthetic scorer documents exercise the frozen evaluator's projection only.
package model

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// An authentic scorer omits top-level eligibility and carries all score facts
// in the fields selected by the evaluator, including the complete diagnostics.
func TestCompetitionScorerDocumentProjectionUsesDiagnosticEligibility(t *testing.T) {
	content := []byte(`{
		"score_schema": 1, "raw_score": 71.25, "normalized_score": 128.75,
		"placeable": true, "gates": {"G1_success": {"passed": true, "details": {"candidate": 0.99}}},
		"significance": {"method": "synthetic-method", "alpha": 0.05, "replicate_count": 9},
		"diagnostics": {"baseline_takeover_eligible": true, "baseline": {"raw_score": 100}, "replicates": [71, 72]},
		"eval_error": null, "synthetic_extra": {"ignored": true}
	}`)
	before := bytes.Clone(content)
	raw, normalized := 71.25, 128.75
	expected := &CompetitionScoreResult{
		ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized, Placeable: true, TakeoverEligible: true,
		Gates:        map[string]CompetitionGate{"G1_success": {Passed: true, Details: map[string]any{"candidate": 0.99}}},
		Significance: &CompetitionScoreSignificance{Method: "synthetic-method", Alpha: .05, ReplicateCount: 9},
		Diagnostics: map[string]any{"baseline_takeover_eligible": true, "baseline": map[string]any{"raw_score": 100.0},
			"replicates": []any{71.0, 72.0}},
	}
	actual, err := ProjectCompetitionScorerDocument(content)
	if err != nil || !reflect.DeepEqual(actual, expected) || !bytes.Equal(content, before) {
		t.Fatalf("scorer projection changed score facts or archived bytes: %+v, %v", actual, err)
	}
	var unprojected CompetitionScoreResult
	if json.Unmarshal(content, &unprojected) != nil || unprojected.TakeoverEligible == expected.TakeoverEligible {
		t.Fatal("fixture does not reproduce omitted top-level eligibility")
	}
}

// The frozen jq projection ignores unrelated fields and uses exact top-level
// keys; a conflicting or malformed top-level eligibility is not authoritative.
func TestCompetitionScorerDocumentProjectionSelectsExactFields(t *testing.T) {
	content := []byte(`{"score_schema":1,"raw_score":71,"normalized_score":129,"placeable":true,
		"gates":{},"significance":null,"diagnostics":{"baseline_takeover_eligible":true},
		"takeover_eligible":"synthetic-ignored","Raw_Score":999,"Score_Schema":99,"Placeable":false}`)
	score, err := ProjectCompetitionScorerDocument(content)
	if err != nil || score.ScoreSchema != 1 || score.RawScore == nil || *score.RawScore != 71 || !score.Placeable || !score.TakeoverEligible {
		t.Fatalf("unselected top-level fields changed the worker projection: %+v, %v", score, err)
	}
}

// Missing, empty, null and false diagnostics all preserve the frozen false
// default; true is derived even if the document contains the opposite flag.
func TestCompetitionScorerDocumentProjectionDefaultsEligibility(t *testing.T) {
	for _, sample := range []struct {
		diagnostics string
		eligible    bool
	}{
		{diagnostics: ""},
		{diagnostics: `,"diagnostics":null`},
		{diagnostics: `,"diagnostics":{}`},
		{diagnostics: `,"diagnostics":{"baseline_takeover_eligible":null}`},
		{diagnostics: `,"diagnostics":{"baseline_takeover_eligible":false}`},
		{diagnostics: `,"diagnostics":{"baseline_takeover_eligible":true}`, eligible: true},
	} {
		for _, topLevel := range []string{"true", "false"} {
			score, err := ProjectCompetitionScorerDocument([]byte(`{"score_schema":1,"takeover_eligible":` + topLevel + sample.diagnostics + `}`))
			if err != nil || score.TakeoverEligible != sample.eligible {
				t.Fatalf("eligibility default differs for %q with top-level %s: %+v, %v", sample.diagnostics, topLevel, score, err)
			}
		}
	}
}

// Non-boolean diagnostic values cannot silently become false: jq retains
// their type and the worker's boolean decode must reject the result.
func TestCompetitionScorerDocumentProjectionRejectsInvalidEligibility(t *testing.T) {
	for _, value := range []string{`"true"`, `"false"`, `""`, `0`, `1`, `[]`, `{}`} {
		if score, err := ProjectCompetitionScorerDocument([]byte(`{"diagnostics":{"baseline_takeover_eligible":` + value + `}}`)); err == nil || score != nil {
			t.Fatalf("invalid diagnostic eligibility %s was accepted: %+v", value, score)
		}
	}
	for _, value := range []string{`false`, `0`, `[]`, `"synthetic-invalid"`} {
		if score, err := ProjectCompetitionScorerDocument([]byte(`{"diagnostics":` + value + `}`)); err == nil || score != nil {
			t.Fatalf("invalid diagnostics %s were accepted: %+v", value, score)
		}
	}
}

// A reported evaluation error or malformed selected field is never repaired
// into successful review evidence, even when other score fields appear usable.
func TestCompetitionScorerDocumentProjectionRejectsErrorsAndMalformedDocuments(t *testing.T) {
	for _, content := range []string{"", `null`, `[]`, `false`, `{`, `{} {}`,
		`{"raw_score":"synthetic-invalid"}`, `{"gates":[]}`, `{"significance":false}`,
		`{"eval_error":{"kind":"infrastructure","code":"synthetic-error"}}`,
		`{"eval_error":false}`, `{"eval_error":0}`, `{"eval_error":""}`} {
		if score, err := ProjectCompetitionScorerDocument([]byte(content)); err == nil || score != nil {
			t.Fatalf("invalid scorer document %q was accepted: %+v", content, score)
		}
	}
}
