package grafana

import (
	"strings"
	"testing"
)

// The uptime probe row of the extenders dashboard answers what the
// false-revocation fix (taskworker/work/extender_probe_metrics.go) exists to
// show: how many revocations there were, how many of them the extender
// recovered from, the share that makes, the failures the prober's guard held,
// how long a restored extender was out of the directories, and whether the
// prober's own control target answers.
func TestExtendersDashboardShowsTheFalseRevocationRate(t *testing.T) {
	dashboard := readTestDashboard(t, "extenders.json")

	rowFound := false
	for _, panel := range dashboard.Panels {
		if panel.Type == "row" && panel.Title == "uptime probe" {
			rowFound = true
		}
	}
	if !rowFound {
		t.Fatal("no uptime probe row")
	}

	panelWith := func(t *testing.T, match func(expr string) bool) testPanel {
		t.Helper()
		for _, panel := range dashboard.Panels {
			for _, target := range panel.Targets {
				if match(target.Expr) {
					return panel
				}
			}
		}
		t.Fatal("no panel matches")
		return testPanel{}
	}

	// restored extenders over revocations, over the same day, as a share
	rate := panelWith(t, func(expr string) bool {
		return strings.Contains(expr, `urnetwork_taskworker_extender_probe_restores_total{env="$env",kind="extender"}[24h]`) &&
			strings.Contains(expr, `/ sum(increase(urnetwork_taskworker_extender_probe_revocations_total{env="$env"}[24h]))`)
	})
	if rate.FieldConfig.Defaults.Unit != "percentunit" {
		t.Errorf("the false revocation rate shows %q, want a share", rate.FieldConfig.Defaults.Unit)
	}
	for _, want := range []string{"window", "own activation"} {
		if !strings.Contains(rate.Description, want) {
			t.Errorf("the false revocation rate does not say what it leaves out (%q)", want)
		}
	}

	// the held failures, the verdicts and the control, each by family
	panelWith(t, func(expr string) bool {
		return strings.Contains(expr, `urnetwork_taskworker_extender_probe_results_total{env="$env",result="held"}`)
	})
	for _, metric := range []string{
		"urnetwork_taskworker_extender_probe_passes_total",
		"urnetwork_taskworker_extender_probe_results_total",
		"urnetwork_taskworker_extender_probe_control_total",
		"urnetwork_taskworker_extender_probe_deactivations_total",
	} {
		panel := panelWith(t, func(expr string) bool {
			return strings.Contains(expr, metric) && strings.Contains(expr, "ip_version") && strings.Contains(expr, "increase(")
		})
		if panel.Type != "timeseries" {
			t.Errorf("%s is a %s, want a timeseries", metric, panel.Type)
		}
	}

	// how long a restored extender was revoked, as quantiles of the histogram
	revoked := panelWith(t, func(expr string) bool {
		return strings.HasPrefix(expr, "histogram_quantile(0.95, sum by (le) (increase(urnetwork_taskworker_extender_probe_restore_revoked_seconds_bucket")
	})
	if revoked.FieldConfig.Defaults.Unit != "s" {
		t.Errorf("the time revoked before a restore shows %q, want seconds", revoked.FieldConfig.Defaults.Unit)
	}

	// every counter is read through increase(), never as a raw process total
	for _, expression := range dashboardExpressions(dashboard) {
		if strings.Contains(expression, "urnetwork_taskworker_extender_probe_") && !strings.Contains(expression, "increase(") {
			t.Errorf("an uptime probe counter is read without increase(): %q", expression)
		}
	}
}
