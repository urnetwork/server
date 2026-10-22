// Keeps the lifecycle dashboard on committed events and visible observed deficits.
package grafana

import (
	"encoding/json"
	"strings"
	"testing"
)

// Rate each process's monotonic counter before summing all writing services.
// Connect host/block filters must not silently exclude taskworker closes.
func TestConnectContractLifecycleDashboardRatesAndCoverage(t *testing.T) {
	const selector = `{env="$env",instance!=""}`
	const opened = "sum(rate(urnetwork_contract_opened_total" + selector + "[$__rate_interval]))"
	const closed = "sum(rate(urnetwork_contract_closed_total" + selector + "[$__rate_interval]))"
	const difference = opened + " - " + closed
	assertDiagnosticDashboardPanels(t, "connect.json", []diagnosticDashboardPanel{
		{id: 23, unit: "ops", targets: []testTarget{
			{Expr: opened, LegendFormat: "opened / s"},
			{Expr: closed, LegendFormat: "closed / s"},
		}, descriptionParts: []string{"first committed", "reuse and rollback", "quarantine", "all scraped writing services", "No data, not zero"}},
		{id: 24, unit: "ops", targets: []testTarget{
			{Expr: difference, LegendFormat: "opened - closed / s"},
			{Expr: "clamp_min((" + difference + "), 0)", LegendFormat: "opening excess / s"},
		}, descriptionParts: []string{"red area", "normal contract lifetime", "Sustained positive growth", "growing overdue backlog", "not an absolute or overdue backlog census"}},
		{id: 26, unit: "short", targets: []testTarget{
			{Expr: "count by (service) (urnetwork_contract_opened_total" + selector + ")", LegendFormat: "opened {{service}}"},
			{Expr: "count by (service) (urnetwork_contract_closed_total" + selector + ")", LegendFormat: "closed {{service}}"},
		}, descriptionParts: []string{"zero-valued counters", "not proof of complete fleet coverage", "observed rates", "No data, not zero"}},
	})
	dashboard := readTestDashboard(t, "connect.json")
	change := dashboardPanelById(dashboard, 25)
	expected := "sum(increase(urnetwork_contract_opened_total" + selector + "[$__range])) - sum(increase(urnetwork_contract_closed_total" + selector + "[$__range]))"
	if change == nil || change.Type != "stat" || change.FieldConfig.Defaults.Unit != "short" || len(change.Targets) != 1 ||
		change.Targets[0].Expr != expected || !change.Targets[0].Instant || change.Targets[0].Range == nil || *change.Targets[0].Range {
		t.Fatal("selected-range backlog change lost reset-aware counter units")
	}
	for _, id := range []int{23, 24, 25, 26} {
		panel := dashboardPanelById(dashboard, id)
		for _, target := range panel.Targets {
			for _, forbidden := range []string{" or ", "vector(0)", "$host", "$block", `service="connect"`, "created_total", "force_closed_total"} {
				if strings.Contains(target.Expr, forbidden) {
					t.Fatalf("panel %d hides missing data or excludes lifecycle writers: %s", id, forbidden)
				}
			}
		}
	}
}

// Positive opening excess remains directly visible in red, without a ratio or
// normalization that could conceal an absolute closing deficit.
func TestConnectContractLifecycleDashboardHighlightsPositiveDeficit(t *testing.T) {
	dashboard := readTestDashboard(t, "connect.json")
	panel := dashboardPanelById(dashboard, 24)
	if panel == nil {
		t.Fatal("opening deficit panel missing")
	}
	red, filled := false, false
	for _, override := range panel.FieldConfig.Overrides {
		if override.Matcher.Options != "opening excess / s" {
			continue
		}
		for _, property := range override.Properties {
			if property.Id == "color" {
				var color struct {
					Mode       string `json:"mode"`
					FixedColor string `json:"fixedColor"`
				}
				if err := json.Unmarshal(property.Value, &color); err != nil {
					t.Fatal(err)
				}
				red = color.Mode == "fixed" && color.FixedColor == "red"
			}
			if property.Id == "custom.fillOpacity" {
				var opacity float64
				if err := json.Unmarshal(property.Value, &opacity); err != nil {
					t.Fatal(err)
				}
				filled = opacity > 0
			}
		}
	}
	if !red || !filled {
		t.Fatal("positive opening excess must retain its red filled series")
	}
	for _, other := range dashboard.Panels {
		if other.Id <= 21 && other.GridPos.Y < 15 {
			t.Fatal("existing Connect panels overlap the lifecycle row")
		}
	}
}
