// Task lifecycle trends remain the first panels and include every producer.
package grafana

import (
	"strings"
	"testing"
)

// Rate each process before summing and keep RunOnce coalescing separate from
// new queue owners. Worker filters cannot silently discard API submissions.
func TestTaskLifecycleCountersLeadDashboard(t *testing.T) {
	const selector = `{env="$env",instance!=""}`
	assertDiagnosticDashboardPanels(t, "taskworker.json", []diagnosticDashboardPanel{
		{id: 26, unit: "ops", targets: []testTarget{
			{Expr: "sum(rate(urnetwork_task_submitted_total" + selector + "[$__rate_interval]))", LegendFormat: "submitted / s"},
			{Expr: "sum(rate(urnetwork_task_finished_total" + selector + "[$__rate_interval]))", LegendFormat: "finished / s"},
		}, descriptionParts: []string{"all scraped producer services", "RunOnce successors", "Rollbacks", "No data, not zero"}},
		{id: 27, unit: "ops", targets: []testTarget{
			{Expr: "sum(rate(urnetwork_task_balked_total" + selector + "[$__rate_interval]))", LegendFormat: "balked / s"},
		}, descriptionParts: []string{"accepted coalescing", "IfAbsent refusals", "later run", "No data, not zero"}},
	})
	dashboard := readTestDashboard(t, "taskworker.json")
	if len(dashboard.Panels) < 2 || dashboard.Panels[0].Id != 26 || dashboard.Panels[1].Id != 27 {
		t.Fatal("task lifecycle counters are not first in the dashboard")
	}
	for _, panel := range dashboard.Panels {
		if panel.Id != 26 && panel.Id != 27 {
			if panel.GridPos.Y < 8 {
				t.Fatal("existing panel overlaps the task lifecycle row")
			}
			continue
		}
		if panel.GridPos.Y != 0 {
			t.Fatal("task lifecycle chart is not at the top")
		}
		for _, target := range panel.Targets {
			for _, forbidden := range []string{" or ", "vector(0)", "$host", "$block", "service=", "queue_depth"} {
				if strings.Contains(target.Expr, forbidden) {
					t.Fatalf("task lifecycle query hides a producer or missing evidence: %s", forbidden)
				}
			}
		}
	}
}
