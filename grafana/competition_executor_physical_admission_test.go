// Physical admission is not inferred from an idle database row. Dashboard
// values must retain finite unknown/occupied/available meanings and freshness.
package grafana

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompetitionExecutorPhysicalAdmissionSeparatesUnknownAndOccupied(t *testing.T) {
	panel := dashboardPanelById(readTestDashboard(t, "competition-automation.json"), 15)
	if panel == nil || len(panel.Targets) != 1 {
		t.Fatal("physical admission panel is absent")
	}
	query := panel.Targets[0].Expr
	for _, required := range []string{"executor_physical_available", "executor_physical_observed_at_seconds", "time()", "<= bool 60", "> bool 60", "and on (executor_host)", "executor_noop_qualified"} {
		if !strings.Contains(query, required) {
			t.Fatal("physical observation lost freshness or current-host scope", required)
		}
	}
	steps := panel.FieldConfig.Defaults.Thresholds.Steps
	if len(steps) != 3 || steps[0].Value != nil || steps[0].Color != "red" || steps[1].Value == nil || *steps[1].Value != 0 || steps[1].Color != "orange" ||
		steps[2].Value == nil || *steps[2].Value != 1 || steps[2].Color != "green" {
		t.Fatal("unknown or occupied physical custody was rendered healthy")
	}
	content, err := dashboardsFs.ReadFile("dashboards/competition-automation.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Panels []struct {
			Id          int `json:"id"`
			FieldConfig struct {
				Defaults struct {
					Mappings []struct {
						Type    string `json:"type"`
						Options map[string]struct {
							Color string `json:"color"`
						} `json:"options"`
					} `json:"mappings"`
				} `json:"defaults"`
			} `json:"fieldConfig"`
		} `json:"panels"`
	}
	if json.Unmarshal(content, &raw) != nil {
		t.Fatal("physical value mappings are unavailable")
	}
	for _, candidate := range raw.Panels {
		if candidate.Id != 15 {
			continue
		}
		for _, mapping := range candidate.FieldConfig.Defaults.Mappings {
			if mapping.Type == "value" && mapping.Options["-1"].Color == "red" && mapping.Options["0"].Color == "orange" && mapping.Options["1"].Color == "green" {
				return
			}
		}
	}
	t.Fatal("physical admission mappings omitted an explicit custody state")
}
