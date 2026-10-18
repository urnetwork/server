// Staging markers are explicit wire authority, while omitted false values
// preserve the JSON identity of historical production evidence.
package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/urnetwork/server/model"
)

// The published strict schemas must accept staging without changing legacy
// production payloads into a different signed/content-addressed representation.
func TestCompetitionV2StagingSchemaMatchesModeBoundModels(t *testing.T) {
	spec := loadCompetitionSpec(t)
	for _, fixture := range []struct {
		schema string
		value  any
	}{
		{schema: "CompetitionEvaluationSeal", value: model.CompetitionEvaluationSeal{}},
		{schema: "CompetitionRewardDecision", value: model.CompetitionRewardDecision{}},
		{schema: "CompetitionV2RoundResult", value: model.CompetitionV2RoundResult{}},
		{schema: "CompetitionSourceActivation", value: model.CompetitionSourceActivation{}},
		{schema: "CompetitionSourceReadiness", value: model.CompetitionSourceReadiness{}},
	} {
		schema, ok := spec.schemas[fixture.schema].(map[string]any)
		if !ok {
			t.Fatalf("missing object schema %s", fixture.schema)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("missing properties for %s", fixture.schema)
		}
		marker, ok := properties["staging"].(map[string]any)
		if !ok || marker["type"] != "boolean" || marker["default"] != false {
			t.Fatalf("missing explicit mode in %s: %+v", fixture.schema, marker)
		}
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				if name == "staging" {
					t.Errorf("%s would reject historical omitted-false evidence", fixture.schema)
				}
			}
		}
		value := reflect.New(reflect.TypeOf(fixture.value)).Elem()
		field, ok := value.Type().FieldByName("Staging")
		if !ok || field.Tag.Get("json") != "staging,omitempty" {
			t.Fatalf("%s changed historical JSON mode encoding", fixture.schema)
		}
		for _, staging := range []bool{false, true} {
			value.FieldByName("Staging").SetBool(staging)
			content, err := json.Marshal(value.Interface())
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(content, &body); err != nil {
				t.Fatal(err)
			}
			marker, present := body["staging"]
			if present != staging || staging && marker != true {
				t.Fatalf("%s mode=%t encoded as present=%t value=%v", fixture.schema, staging, present, marker)
			}
		}
	}
}

// A literal $20/weekly-only schema would reject the honest zero-fee policy
// emitted by an explicitly configured short-window staging season.
func TestCompetitionV2StagingSeasonSchemaAllowsFeeFreePolicy(t *testing.T) {
	spec := loadCompetitionSpec(t)
	schema := spec.schemas["CompetitionSeasonPolicy"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	fee := properties["submission_fee_usd"].(map[string]any)
	values, ok := fee["enum"].([]any)
	if !ok || len(values) != 2 || values[0] != 0 || values[1] != 20 {
		t.Fatalf("public fee contract does not distinguish staging: %+v", fee)
	}
	window := properties["submission_window_seconds"].(map[string]any)
	if window["const"] != nil || window["minimum"] != 60 || window["maximum"] != 604800 {
		t.Fatalf("staging window rejected by public schema: %+v", window)
	}
	branches, ok := schema["oneOf"].([]any)
	if !ok || len(branches) != 2 {
		t.Fatal("public policy must retain distinct production and staging alternatives")
	}
	production := branches[0].(map[string]any)["properties"].(map[string]any)
	if production["submission_fee_usd"].(map[string]any)["const"] != 20 || production["submission_window_seconds"].(map[string]any)["const"] != 604800 {
		t.Fatal("staging schema widened production's fee/window contract")
	}
	staging := branches[1].(map[string]any)["properties"].(map[string]any)
	if staging["submission_fee_usd"].(map[string]any)["const"] != 0 || staging["preparation_window_seconds"].(map[string]any)["maximum"] != 57600 {
		t.Fatal("staging schema lacks bounded non-financial timing")
	}
}
