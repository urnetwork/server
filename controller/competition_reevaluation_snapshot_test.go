// Full job and policy snapshots contain case-sensitive data maps, unlike
// typed evidence protocols. Their digest must preserve that data exactly.
package controller

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// Exercise the actual score model used by the scored-row admission control;
// its gate names must reach authorization instead of failing snapshot hashing.
func TestCompetitionReevaluationSnapshotDigestPreservesScoredGateData(t *testing.T) {
	score := competitionRankingScore(90, 110)
	score.Gates["G2"] = Gate{Passed: true, Details: map[string]any{"ProviderA": 1, "providerA": 2}}
	score.Diagnostics = map[string]any{"LatencyMs": 3, "latencyMs": 4}
	snapshot := map[string]any{"state": "succeeded", "score_json": score, "attempt_count": 2}
	content, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		t.Fatal(err)
	}
	expected, err := model.CompetitionAutomationDigest(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := CompetitionJobReevaluationSnapshotDigest(content); err != nil || digest != expected {
		t.Fatalf("scored snapshot lost its exact gate/data identity: digest=%q expected=%q error=%v", digest, expected, err)
	}
}

// Data keys that differ only by case are distinct jsonb keys, not duplicate
// protocol fields. Neither case folding nor dropped values may alias a row.
func TestCompetitionReevaluationSnapshotDigestKeepsCaseDistinctDataKeys(t *testing.T) {
	first, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(`{"score_json":{"gates":{"G2":{"passed":true}},"diagnostics":{"Provider":1,"provider":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(`{"score_json":{"diagnostics":{"provider":2,"Provider":1},"gates":{"G2":{"passed":true}}}}`))
	if err != nil || reordered != first {
		t.Fatalf("ordering changed case-sensitive data identity: first=%q reordered=%q error=%v", first, reordered, err)
	}
	for _, content := range []string{
		`{"score_json":{"gates":{"g2":{"passed":true}},"diagnostics":{"Provider":1,"provider":2}}}`,
		`{"score_json":{"gates":{"G2":{"passed":true}},"diagnostics":{"Provider":2,"provider":1}}}`,
		`{"score_json":{"gates":{"G2":{"passed":true}},"diagnostics":{"provider":2}}}`,
	} {
		if digest, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(content)); err != nil || digest == first {
			t.Fatalf("different case-sensitive data aliased a snapshot: digest=%q first=%q error=%v", digest, first, err)
		}
	}
}

// Token validation must use json.Number too: jsonb numbers are not bounded by
// float64 range or precision, and rounding cannot erase snapshot differences.
func TestCompetitionReevaluationSnapshotDigestPreservesJsonNumbers(t *testing.T) {
	content := json.RawMessage(`{"diagnostics":{"provider_samples":1e400,"exact_count":9007199254740993}}`)
	expected, err := model.CompetitionAutomationDigest(map[string]any{"diagnostics": map[string]any{
		"provider_samples": json.Number("1e400"), "exact_count": json.Number("9007199254740993"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := CompetitionJobReevaluationSnapshotDigest(content)
	if err != nil || digest != expected {
		t.Fatalf("generic number data was rejected or rounded: digest=%q expected=%q error=%v", digest, expected, err)
	}
	changed, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(`{"diagnostics":{"provider_samples":1e400,"exact_count":9007199254740992}}`))
	if err != nil || changed == digest {
		t.Fatalf("distinct exact integers aliased a snapshot: first=%q changed=%q error=%v", digest, changed, err)
	}
}

// The data decoder keeps the former structural safety boundary, including
// exact duplicate keys whose spelling differs only by JSON escape notation.
func TestCompetitionReevaluationSnapshotDigestRejectsMalformedDataObjects(t *testing.T) {
	for _, content := range []string{
		`null`, `[]`, `true`, `3`, `"synthetic"`, ``,
		`{"G2":1,"G2":2}`, `{"G2":null,"G2":2}`,
		`{"data":{"G2":1,"\u00472":2}}`, `{"data":[{"G2":1,"G2":2}]}`,
		`{"G2":1}{"G2":2}`, `{"G2":1} false`, `{"G2":1} invalid`,
		`{"G2":1`, `{"G2":[1,]}`, `{"G2":01}`, "{\"data\":\"\xff\"}",
		`{"nested":` + strings.Repeat("[", 64) + `0` + strings.Repeat("]", 64) + `}`,
	} {
		if digest, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(content)); err != ErrConflict || digest != "" {
			t.Fatalf("malformed snapshot was authenticated: content=%q digest=%q error=%v", content, digest, err)
		}
	}
	bounded := `{"nested":` + strings.Repeat("[", 63) + `0` + strings.Repeat("]", 63) + `}`
	if digest, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(bounded)); err != nil || digest == "" {
		t.Fatalf("snapshot at the existing nesting boundary was refused: digest=%q error=%v", digest, err)
	}
}

// Generic snapshot data cannot weaken the separate typed evidence decoder's
// exact lowercase field and unknown-field rules.
func TestCompetitionReevaluationSnapshotDigestKeepsProtocolDecoderStrict(t *testing.T) {
	for _, content := range []string{`{"Schema":1}`, `{"schema":1,"Schema":2}`, `{"schema":1,"unknown":2}`} {
		var protocol struct {
			Schema int `json:"schema"`
		}
		if err := decodeFailureEvidenceJson([]byte(content), &protocol); err == nil {
			t.Fatalf("typed protocol accepted a mixed-case or unknown field: %s", content)
		}
		if digest, err := CompetitionJobReevaluationSnapshotDigest(json.RawMessage(content)); err != nil || digest == "" {
			t.Fatalf("generic data inherited typed protocol field rules: content=%q digest=%q error=%v", content, digest, err)
		}
	}
}
