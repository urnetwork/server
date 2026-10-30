// The published schema enforces the same optional legacy and explicit future
// policy/version pairs as the actual discovery producer and frozen snapshots.
package controller

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Real schema compilation, not a second hand-written contract approximation.
func TestCompetitionG2PublicPolicyVersionConformance(t *testing.T) {
	content, err := os.ReadFile("../../sn/api/competition.yml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	schemaBytes, err := json.Marshal(map[string]any{"$ref": "#/$defs/CompetitionInfoResult", "$defs": document.Components.Schemas})
	if err != nil {
		t.Fatal(err)
	}
	schemaBytes = bytes.ReplaceAll(schemaBytes, []byte("#/components/schemas/"), []byte("#/$defs/"))
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name                 string
		policy               any
		version              string
		policyPresent, valid bool
	}{
		{name: "legacy omission", version: model.CompetitionScorerVersion, valid: true},
		{name: "minimum work", policy: string(model.CompetitionG2VolumePolicyMinimumWork), version: model.CompetitionMinimumWorkScorerVersion, policyPresent: true, valid: true},
		{name: "new version without policy", version: model.CompetitionMinimumWorkScorerVersion},
		{name: "new policy old version", policy: "minimum-work/1", version: model.CompetitionScorerVersion, policyPresent: true},
		{name: "unknown policy", policy: "unknown/1", version: model.CompetitionMinimumWorkScorerVersion, policyPresent: true},
		{name: "explicit empty policy", policy: "", version: model.CompetitionScorerVersion, policyPresent: true},
		{name: "null policy", version: model.CompetitionMinimumWorkScorerVersion, policyPresent: true},
	} {
		info := validSettings().PublicInfo()
		info.ScorerVersion = c.version
		encoded, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		var instance map[string]any
		if err := json.Unmarshal(encoded, &instance); err != nil {
			t.Fatal(err)
		}
		if c.policyPresent {
			instance["evaluation_policy"].(map[string]any)["g2_volume_policy"] = c.policy
		}
		if err := resolved.Validate(instance); (err == nil) != c.valid {
			t.Errorf("%s: schema accepted=%t expected=%t: %v", c.name, err == nil, c.valid, err)
		}
	}
}
