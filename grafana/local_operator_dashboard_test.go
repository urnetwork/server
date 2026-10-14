// the in-process operator's per-method call series sit with the egress probes.
// only taskworkers with operator_transport: local publish them, so every panel
// says so, reads the env's taskworkers, and renders each series once in its
// unit; durations and waits read as a summary's sum over its count
package grafana

import (
	"strings"
	"testing"
)

func TestEgressProbeDashboardChartsTheInProcessOperatorCalls(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const selector = `{env="$env",service="taskworker",instance!=""}`
	meanOf := func(family string) string {
		return "sum by (method) (rate(" + family + "_sum" + selector + "[$__rate_interval])) / clamp_min(sum by (method) (rate(" + family + "_count" + selector + "[$__rate_interval])), 1e-9)"
	}
	for _, c := range []struct {
		expr string
		unit string
	}{
		{expr: "sum by (method) (rate(urnetwork_local_operator_call_duration_seconds_count" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: meanOf("urnetwork_local_operator_call_duration_seconds"), unit: "s"},
		{expr: "sum by (host, block, instance) (urnetwork_local_operator_calls_inflight" + selector + ")", unit: "short"},
		{expr: "sum by (host, block, instance) (urnetwork_local_operator_calls_waiting" + selector + ")", unit: "short"},
		{expr: "sum by (method, kind) (rate(urnetwork_local_operator_call_errors_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: meanOf("urnetwork_local_operator_call_slot_wait_seconds"), unit: "s"},
	} {
		found := 0
		for _, panel := range dashboard.Panels {
			for _, target := range panel.Targets {
				if target.Expr != c.expr {
					continue
				}
				found += 1
				if panel.FieldConfig.Defaults.Unit != c.unit {
					t.Errorf("%q renders %s in %q, want %s", panel.Title, c.expr, panel.FieldConfig.Defaults.Unit, c.unit)
				}
				if !strings.Contains(panel.Description, "operator_transport: local") {
					t.Errorf("%q does not say only opted-in taskworkers publish %s", panel.Title, c.expr)
				}
			}
		}
		if found != 1 {
			t.Errorf("egress probes dashboard charts %s %d times, want once", c.expr, found)
		}
	}
}
