// The scoring-policy enum and optional wire fields preserve every historical
// version-one contract while naming the prospective implementation exactly.
package model

import (
	"bytes"
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// A version-one default must never be reinterpreted as the new policy.
func TestCompetitionG2PolicyVersionsRejectUnknown(t *testing.T) {
	for _, c := range []struct {
		policy  CompetitionG2VolumePolicy
		version string
	}{
		{policy: CompetitionG2VolumePolicyLegacy, version: "sim-latency-score/1"},
		{policy: CompetitionG2VolumePolicyMinimumWork, version: "sim-latency-score/2"},
	} {
		version, err := CompetitionScorerVersionForG2Policy(c.policy)
		if err != nil || version != c.version {
			t.Errorf("%q: version=%q error=%v", c.policy, version, err)
		}
	}
	for _, policy := range []CompetitionG2VolumePolicy{"minimum-work", "minimum-work/2", "legacy", " ", "unknown/1"} {
		if _, err := CompetitionScorerVersionForG2Policy(policy); err == nil {
			t.Errorf("unknown policy %q accepted", policy)
		}
	}
	if CompetitionScorerVersion != "sim-latency-score/1" || CompetitionScoreSchema != 1 {
		t.Fatal("historical global version/schema changed")
	}
}

// Neither JSON nor YAML writes an empty new field into old policy snapshots.
func TestCompetitionG2PolicyLegacyCodecOmission(t *testing.T) {
	legacy := CompetitionEvaluationPolicy{HardwareId: "synthetic-hardware"}
	jsonBytes, err := json.Marshal(legacy)
	if err != nil || bytes.Contains(jsonBytes, []byte("g2_volume_policy")) {
		t.Fatal("legacy JSON acquired a policy field", err)
	}
	yamlBytes, err := yaml.Marshal(legacy)
	if err != nil || bytes.Contains(yamlBytes, []byte("g2_volume_policy")) {
		t.Fatal("legacy YAML acquired a policy field", err)
	}
	var replay CompetitionEvaluationPolicy
	if err := json.Unmarshal(jsonBytes, &replay); err != nil {
		t.Fatal(err)
	}
	replayed, err := json.Marshal(replay)
	if err != nil || !bytes.Equal(replayed, jsonBytes) || replay.G2VolumePolicy != CompetitionG2VolumePolicyLegacy {
		t.Fatal("legacy JSON did not round-trip byte-identically", err)
	}
	legacy.G2VolumePolicy = CompetitionG2VolumePolicyMinimumWork
	newBytes, err := json.Marshal(legacy)
	if err != nil || !bytes.Contains(newBytes, []byte(`"g2_volume_policy":"minimum-work/1"`)) {
		t.Fatal("new policy is not explicitly serialized", err)
	}
	for _, value := range []string{`null`, `""`, `"unknown/1"`, `1`, `true`} {
		var policy CompetitionEvaluationPolicy
		if json.Unmarshal([]byte(`{"g2_volume_policy":`+value+`}`), &policy) == nil {
			t.Errorf("explicit invalid policy %s decoded", value)
		}
	}
}

// Historical activation hashes remain unchanged when no instrument was selected.
func TestCompetitionG2PolicyLegacyActivationOmission(t *testing.T) {
	content, err := json.Marshal(CompetitionSourceActivation{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"g2_volume_policy", "trusted_instrument", "simulator_sha256", "scorer_sha256"} {
		if bytes.Contains(content, []byte(`"`+field+`"`)) {
			t.Errorf("historical activation acquired %s", field)
		}
	}
}

// Policy-only relabeling and incomplete new provenance are invalid at the
// shared scorer, worker, persistence and API boundary.
func TestCompetitionScoreG2PolicyProvenance(t *testing.T) {
	for _, c := range []struct {
		name        string
		diagnostics map[string]any
		valid       bool
	}{
		{name: "historical omission", valid: true},
		{name: "minimum work", diagnostics: map[string]any{"g2_volume_policy": "minimum-work/1", "scorer_version": "sim-latency-score/2"}, valid: true},
		{name: "unknown", diagnostics: map[string]any{"g2_volume_policy": "unknown/1", "scorer_version": "sim-latency-score/2"}},
		{name: "wrong scorer", diagnostics: map[string]any{"g2_volume_policy": "minimum-work/1", "scorer_version": "sim-latency-score/1"}},
		{name: "missing scorer", diagnostics: map[string]any{"g2_volume_policy": "minimum-work/1"}},
		{name: "missing policy", diagnostics: map[string]any{"scorer_version": "sim-latency-score/2"}},
		{name: "explicit empty", diagnostics: map[string]any{"g2_volume_policy": "", "scorer_version": "sim-latency-score/1"}},
		{name: "null policy", diagnostics: map[string]any{"g2_volume_policy": nil, "scorer_version": "sim-latency-score/2"}},
	} {
		score := validCompetitionScoreForTest()
		score.Diagnostics = c.diagnostics
		if err := ValidateCompetitionScore(score); (err == nil) != c.valid {
			t.Errorf("%s: validity=%t error=%v", c.name, c.valid, err)
		}
	}
}
