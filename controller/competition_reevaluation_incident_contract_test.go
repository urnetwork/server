// The actual public schema and native receipt validator must agree about
// legacy omission and the two explicit operator exceptions.
package controller

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Compile the published OpenAPI component, not a hand-written replacement of
// its rules. All identities come from the existing synthetic receipt fixture.
func TestCompetitionReevaluationIncidentPublicContractPreservesTypedFailure(t *testing.T) {
	content, err := readCompetitionTestSnContract()
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
	schemaBytes, err := json.Marshal(map[string]any{"$ref": "#/$defs/CompetitionJobReevaluationCycle", "$defs": document.Components.Schemas})
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
		name, class, kind, code string
		valid                   bool
	}{
		{name: "legacy-archive", kind: "infrastructure", code: "artifact_archive_failed", valid: true},
		{name: "legacy-budget", kind: "infrastructure", code: "evaluation_time_budget_exhausted", valid: true},
		{name: "heartbeat", class: model.CompetitionJobReevaluationHeartbeatBudget, kind: "infrastructure", code: "evaluation_time_budget_exhausted", valid: true},
		{name: "backing-service", class: model.CompetitionJobReevaluationBackingService, kind: "submission", code: "run_unstable", valid: true},
		{name: "operator-interruption", class: model.CompetitionJobReevaluationOperatorInterruption, kind: "infrastructure", code: "evaluation_time_budget_exhausted", valid: true},
		{name: "untyped-submission", kind: "submission", code: "run_unstable"},
		{name: "backing-service-wrong-kind", class: model.CompetitionJobReevaluationBackingService, kind: "infrastructure", code: "artifact_archive_failed"},
		{name: "heartbeat-wrong-code", class: model.CompetitionJobReevaluationHeartbeatBudget, kind: "infrastructure", code: "artifact_archive_failed"},
		{name: "operator-interruption-wrong-code", class: model.CompetitionJobReevaluationOperatorInterruption, kind: "infrastructure", code: "artifact_archive_failed"},
		{name: "unknown-class", class: "synthetic_unknown", kind: "infrastructure", code: "artifact_archive_failed"},
		{name: "unpublished-class-marker", class: model.CompetitionJobReevaluationArchiveInventory, kind: "infrastructure", code: "artifact_archive_failed"},
	} {
		cycle := newCompetitionReevaluationCycleTestFixture(t)
		cycle.IncidentClass = c.class
		cycle.PreviousFailure = model.CompetitionEvaluationFailure{Kind: c.kind, Code: c.code, Retriable: false}
		cycle.ReceiptSha256, err = model.CompetitionJobReevaluationCycleDigest(cycle)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(cycle)
		if err != nil {
			t.Fatal(err)
		}
		var instance map[string]any
		if err := json.Unmarshal(encoded, &instance); err != nil {
			t.Fatal(err)
		}
		schemaErr := resolved.Validate(instance)
		nativeErr := model.ValidateCompetitionJobReevaluationCycle(&cycle)
		if (schemaErr == nil) != c.valid || (nativeErr == nil) != c.valid {
			t.Errorf("%s schema=%v native=%v expected-valid=%t", c.name, schemaErr, nativeErr, c.valid)
		}
	}
}
