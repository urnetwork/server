// Explicit task-owned carrier limits are execution settings, not extra
// authority to infer live occupancy or successful provider checks.
package monitor

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The parser recognizes the optional pair, while legacy omission remains
// valid and partial/negative limits retain fail-closed configuration behavior.
func TestEgressCoverageTransportBudgetDesiredConfig(t *testing.T) {
	for _, test := range []struct {
		fields string
		valid  bool
	}{
		{valid: true},
		{fields: "  transport_budget_byte_count: 67108864\n  transport_budget_count: 100\n", valid: true},
		{fields: "  transport_budget_byte_count: 0\n  transport_budget_count: 0\n", valid: true},
		{fields: "  transport_budget_byte_count: 67108864\n"},
		{fields: "  transport_budget_count: 100\n"},
		{fields: "  transport_budget_byte_count: -1\n  transport_budget_count: 100\n"},
		{fields: "  transport_budget_byte_count: 67108864\n  transport_budget_count: -1\n"},
	} {
		raw := strings.Replace(syntheticEgressDesiredConfig, "blackhole:\n", "blackhole:\n"+test.fields, 1)
		desired := syntheticDesiredEgressConfig(t, raw)
		if (desired.invalidReason == "") != test.valid {
			t.Errorf("optional carrier limit pair valid=%t reason=%s", test.valid, desired.invalidReason)
		}
	}
}

// Construct new fields through JSON so the unchanged pre-feature reducer
// compiles but fails when it silently loses a configured carrier limit.
func TestEgressCoverageTransportBudgetComparesBothProbeOwners(t *testing.T) {
	desired := syntheticDesiredEgressConfig(t, syntheticEgressDesiredConfig)
	for _, owner := range []string{"full", "blackhole"} {
		for _, field := range []string{"transport_budget_byte_count", "transport_budget_count"} {
			durable := desired.settings
			batch := &durable.full
			if owner == "blackhole" {
				batch = &durable.blackhole
			}
			encoded, err := json.Marshal(*batch)
			if err != nil {
				t.Fatal(err)
			}
			var values map[string]any
			if err := json.Unmarshal(encoded, &values); err != nil {
				t.Fatal(err)
			}
			values[field] = 1
			encoded, err = json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, batch); err != nil {
				t.Fatal(err)
			}
			changes := egressCoverageConfigChanges(desired.settings, durable)
			want := owner + "." + field
			if len(changes) != 1 || changes[0] != want {
				t.Errorf("carrier ownership drift=%v, want only %s", changes, want)
			}
			if !strings.Contains(egressCoverageSafeSettings("durable", durable), "durable_"+owner+"_"+field+"=1") {
				t.Errorf("safe execution evidence omitted %s", want)
			}
		}
	}
	if changes := egressCoverageConfigChanges(desired.settings, desired.settings); len(changes) != 0 {
		t.Fatal("unchanged legacy defaults fabricated configuration drift")
	}
}

// The catalog must distinguish configured limits from live capacity and must
// not reinterpret an unset optional pair as a shared Taskworker budget.
func TestMonitorDocumentationEgressTransportBudgetOwnership(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(data)
	start := strings.Index(catalog, "### 2.19 ")
	end := strings.Index(catalog, "### 2.19a ")
	if start < 0 || end <= start {
		t.Fatal("catalog lost the egress coverage section boundary")
	}
	section := strings.Join(strings.Fields(catalog[start:end]), " ")
	for _, required := range []string{
		"transport_budget_byte_count", "transport_budget_count",
		"absent/zero copy default limit values",
		"never a shared process root",
		"separate full/blackhole task owners",
		"Default limits are not multiplied by probe concurrency",
		"Configuration equality does not prove live admission",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("catalog lost carrier ownership boundary %q", required)
		}
	}
}
