// Public contract checks read the sibling schema directly, independent of module replacements.
package controller

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The selector, incident and public projection tests exercise these exact wire values.
func TestCompetitionV2OpenApiEnumConformance(t *testing.T) {
	content, err := os.ReadFile("../../sn/api/competition.yml")
	if err != nil {
		t.Fatal(err)
	}
	type propertySchema struct {
		Enum []string `yaml:"enum"`
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]propertySchema `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile("../connect/sim-latency/README.md")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		Schema   string
		Property string
		Values   string
	}{
		{Schema: "CompetitionRewardDecision", Property: "action", Values: "elect replace no_winner invalidated"},
		{Schema: "CompetitionRewardDecision", Property: "reason_code", Values: "first_eligible no_submissions no_gate_passing_candidate no_significant_improvement no_new_reward_entitlement all_eligible_disqualified EVALUATION_INTEGRITY_INCIDENT"},
		{Schema: "CompetitionV2RoundResult", Property: "integrity_review", Values: "not_reviewed approved disqualified not_applicable"},
		{Schema: "CompetitionV2RoundResult", Property: "source_approval", Values: "pending approved rejected deferred not_applicable"},
		{Schema: "CompetitionV2RoundResult", Property: "settlement_status", Values: "pending received applied confirmed rejected"},
	}
	for _, c := range cases {
		want := strings.Fields(c.Values)
		for _, value := range want {
			if !strings.Contains(string(readme), "`"+value+"`") {
				t.Errorf("README omits %s.%s value %q", c.Schema, c.Property, value)
			}
		}
		sort.Strings(want)
		got := document.Components.Schemas[c.Schema].Properties[c.Property].Enum
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s.%s enum = %q, want %q", c.Schema, c.Property, got, want)
		}
	}
}
