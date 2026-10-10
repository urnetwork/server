// Task lifecycle and execution failure trends lead the dashboard and include every producer.
package grafana

import (
	"strings"
	"testing"
)

// Rate each process before summing, separate graceful drain from failures, and
// keep execution attempts distinct from committed queue owners. Worker filters
// cannot silently discard API submissions or change the breakdown's population.
func TestTaskLifecycleCountersLeadDashboard(t *testing.T) {
	const selector = `{env="$env",instance!=""}`
	assertDiagnosticDashboardPanels(t, "taskworker.json", []diagnosticDashboardPanel{
		{id: 26, unit: "ops", targets: []testTarget{
			{Expr: "sum(rate(urnetwork_task_submitted_total" + selector + "[$__rate_interval]))", LegendFormat: "submitted / s"},
			{Expr: "sum(rate(urnetwork_task_finished_total" + selector + "[$__rate_interval]))", LegendFormat: "finished / s"},
			{Expr: `sum(rate(urnetwork_taskworker_execution_errors_total{env="$env",instance!="",cause!="drained"}[$__rate_interval]))`, LegendFormat: "failed attempts / s"},
			{Expr: `sum(rate(urnetwork_taskworker_execution_errors_total{env="$env",instance!="",cause="drained"}[$__rate_interval]))`, LegendFormat: "graceful drain attempts / s"},
		}, descriptionParts: []string{"all scraped producer services", "RunOnce successors", "Rollbacks are excluded from submitted/finished", "before retry writes or finalization", "not distinct tasks or financial commits", "other cancellations remain failures", "No data, not zero"}},
		{id: 28, unit: "ops", targets: []testTarget{
			{Expr: `sum by (task) (rate(urnetwork_taskworker_execution_errors_total{env="$env",instance!="",cause!="drained"}[$__rate_interval]))`, LegendFormat: "{{task}}"},
		}, descriptionParts: []string{"same failure scope as the adjacent total", "graceful drain (cause=drained) is excluded", "Repeated failures count again", "No data, not zero"}},
		{id: 27, unit: "ops", targets: []testTarget{
			{Expr: "sum(rate(urnetwork_task_balked_total" + selector + "[$__rate_interval]))", LegendFormat: "balked / s"},
		}, descriptionParts: []string{"accepted coalescing", "IfAbsent refusals", "later run", "No data, not zero"}},
	})
	dashboard := readTestDashboard(t, "taskworker.json")
	if len(dashboard.Panels) < 2 || dashboard.Panels[0].Id != 26 || dashboard.Panels[1].Id != 28 {
		t.Fatal("task lifecycle and failure counters are not first in the dashboard")
	}
	for _, panel := range dashboard.Panels {
		if panel.Id != 26 && panel.Id != 28 {
			if panel.GridPos.Y < 8 {
				t.Fatal("existing panel overlaps the task lifecycle and failure row")
			}
		} else if panel.GridPos.Y != 0 {
			t.Fatal("task lifecycle or failure chart is not at the top")
		}
		if panel.Id != 26 && panel.Id != 27 && panel.Id != 28 {
			continue
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
