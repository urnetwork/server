// Public coverage metadata preserves historical discovery and sanitized owner
// feedback while preventing a new score version from silently dropping G7.
package controller

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"
)

// Resolves the actual companion repository's schema, not a duplicate fixture.
func coveragePublicSchemaValidator(t testing.TB, name string) func(any) error {
	t.Helper()
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
	schemaBytes, err := json.Marshal(map[string]any{"$ref": "#/$defs/" + name, "$defs": document.Components.Schemas})
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
	return resolved.Validate
}

// Numeric policy values remain operator approval, not an implicit choice by
// this transport test. The version selection nevertheless binds all fields.
func TestCompetitionWorkCoveragePublicPolicyVersions(t *testing.T) {
	validate := coveragePublicSchemaValidator(t, "CompetitionInfoResult")
	for _, fixture := range []struct {
		name, version       string
		g2, coverage, valid bool
	}{
		{name: "historical v1", version: "sim-latency-score/1", valid: true},
		{name: "historical v2", version: "sim-latency-score/2", g2: true, valid: true},
		{name: "coverage v3", version: "sim-latency-score/3", g2: true, coverage: true, valid: true},
		{name: "coverage missing G2", version: "sim-latency-score/3", coverage: true},
		{name: "version stripped", version: "sim-latency-score/2", g2: true, coverage: true},
		{name: "policy stripped", version: "sim-latency-score/3", g2: true},
		{name: "null coverage", version: "sim-latency-score/3", g2: true, coverage: true},
		{name: "unknown coverage", version: "sim-latency-score/3", g2: true, coverage: true},
	} {
		content, err := json.Marshal(validSettings().PublicInfo())
		if err != nil {
			t.Fatal(err)
		}
		var instance map[string]any
		if err := json.Unmarshal(content, &instance); err != nil {
			t.Fatal(err)
		}
		instance["scorer_version"] = fixture.version
		policy := instance["evaluation_policy"].(map[string]any)
		delete(policy, "g2_volume_policy")
		delete(policy, "work_coverage_policy")
		if fixture.g2 {
			policy["g2_volume_policy"] = "minimum-work/1"
		}
		if fixture.coverage {
			policy["work_coverage_policy"] = map[string]any{"kind": "seeded-crawl-coverage/v1", "synthetic_threshold": .037}
		}
		if fixture.name == "null coverage" {
			policy["work_coverage_policy"] = nil
		} else if fixture.name == "unknown coverage" {
			policy["work_coverage_policy"].(map[string]any)["kind"] = "unknown/1"
		}
		if err := validate(instance); (err == nil) != fixture.valid {
			t.Errorf("%s accepted=%t want=%t: %v", fixture.name, err == nil, fixture.valid, err)
		}
	}
}

// The same score schema accepts the explicit private projection without
// treating that redacted projection as full stored eligibility evidence.
func TestCompetitionWorkCoveragePublicAndPrivateScoreShapes(t *testing.T) {
	validate := coveragePublicSchemaValidator(t, "CompetitionScoreResult")
	for _, fixture := range []struct {
		name  string
		valid bool
	}{
		{name: "historical", valid: true}, {name: "full coverage", valid: true}, {name: "private coverage", valid: true},
		{name: "gate omitted"}, {name: "policy omitted"}, {name: "diagnostics omitted"}, {name: "version downgraded"},
		{name: "G2 omitted"}, {name: "null policy"}, {name: "nonboolean gate"}, {name: "invalid nested coverage"},
	} {
		gates := map[string]Gate{}
		for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
			gates[name] = Gate{Passed: true, Details: map[string]any{}}
		}
		score := ScoreResult{ScoreSchema: 1, Placeable: true, Gates: gates, Significance: testScoreSignificance(true)}
		content, err := json.Marshal(score)
		if err != nil {
			t.Fatal(err)
		}
		var instance map[string]any
		if err := json.Unmarshal(content, &instance); err != nil {
			t.Fatal(err)
		}
		if fixture.name != "historical" {
			policy := map[string]any{"kind": "seeded-crawl-coverage/v1", "synthetic_threshold": .037}
			details := map[string]any{"candidate_page_coverage": .99, "candidate_byte_coverage": .99}
			if fixture.name != "private coverage" {
				details["policy"] = policy
			}
			gate := map[string]any{"passed": true, "details": details}
			instance["gates"].(map[string]any)["G7_work_coverage"] = gate
			diagnostics := map[string]any{"scorer_version": "sim-latency-score/3", "g2_volume_policy": "minimum-work/1", "work_coverage_policy": policy}
			instance["diagnostics"] = diagnostics
			switch fixture.name {
			case "gate omitted":
				delete(instance["gates"].(map[string]any), "G7_work_coverage")
			case "policy omitted":
				delete(diagnostics, "work_coverage_policy")
			case "diagnostics omitted":
				delete(instance, "diagnostics")
			case "version downgraded":
				diagnostics["scorer_version"] = "sim-latency-score/2"
			case "G2 omitted":
				delete(diagnostics, "g2_volume_policy")
			case "null policy":
				diagnostics["work_coverage_policy"] = nil
			case "nonboolean gate":
				gate["passed"] = "true"
			case "invalid nested coverage":
				details["candidate"] = map[string]any{"page_coverage": 1.5}
			}
		}
		if err := validate(instance); (err == nil) != fixture.valid {
			t.Errorf("%s accepted=%t want=%t: %v", fixture.name, err == nil, fixture.valid, err)
		}
	}
}

// Source discovery and independent summary denominators share the typed
// transport contract; omitted work remains visible as zero completion.
func TestCompetitionWorkCoveragePublicSourceAndSummaryShapes(t *testing.T) {
	validateSource := coveragePublicSchemaValidator(t, "CompetitionSourceActivation")
	for _, fixture := range []struct{ policy, g2, valid bool }{
		{valid: true}, {policy: true, g2: true, valid: true}, {policy: true},
	} {
		instance := map[string]any{"target_round_id": "00000000-0000-0000-0000-000000000001"}
		if fixture.policy {
			instance["work_coverage_policy"] = map[string]any{"kind": "seeded-crawl-coverage/v1", "synthetic_threshold": .037}
		}
		if fixture.g2 {
			instance["g2_volume_policy"] = "minimum-work/1"
		}
		if err := validateSource(instance); (err == nil) != fixture.valid {
			t.Errorf("source policy=%t G2=%t accepted=%t want=%t: %v", fixture.policy, fixture.g2, err == nil, fixture.valid, err)
		}
	}
	validateSummary := coveragePublicSchemaValidator(t, "CompetitionWorkCoverageSummary")
	for _, fixture := range []struct {
		name  string
		valid bool
	}{
		{name: "zero completions", valid: true}, {name: "missing denominator"}, {name: "unknown plan"}, {name: "fraction over one"}, {name: "fractional pages"},
	} {
		instance := map[string]any{"plan_sha256": strings.Repeat("a", 64), "offered_roots": 10, "expected_pages": 100, "expected_bytes": 1000,
			"completed_pages": 0, "completed_bytes": 0, "page_coverage": 0, "byte_coverage": 0}
		switch fixture.name {
		case "missing denominator":
			delete(instance, "expected_pages")
		case "unknown plan":
			instance["plan_sha256"] = "unknown"
		case "fraction over one":
			instance["byte_coverage"] = 1.01
		case "fractional pages":
			instance["completed_pages"] = .5
		}
		if err := validateSummary(instance); (err == nil) != fixture.valid {
			t.Errorf("%s accepted=%t want=%t: %v", fixture.name, err == nil, fixture.valid, err)
		}
	}
}
