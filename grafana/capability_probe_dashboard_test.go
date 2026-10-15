// the capability probe's series (server/capprobe) sit with the egress
// probes: every one is charted exactly once, over the env's taskworkers, in
// its unit, on a panel that says it is the capability probe's; durations
// read as a histogram's sum over its count, beside its p95
package grafana

import (
	"strings"
	"testing"
)

func TestEgressProbeDashboardChartsTheCapabilityProbe(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const selector = `{env="$env",service="taskworker",instance!=""}`
	for _, c := range []struct {
		expr string
		unit string
	}{
		{expr: "sum by (result) (rate(urnetwork_capability_probe_passes_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: "sum by (outcome) (rate(urnetwork_capability_probe_providers_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: "sum by (capability, reading) (rate(urnetwork_capability_probe_readings_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: "sum by (stage) (rate(urnetwork_capability_probe_selection_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: "sum(rate(urnetwork_capability_probe_measure_seconds_sum" + selector + "[$__rate_interval])) / clamp_min(sum(rate(urnetwork_capability_probe_measure_seconds_count" + selector + "[$__rate_interval])), 1e-9)", unit: "s"},
		{expr: "histogram_quantile(0.95, sum by (le) (rate(urnetwork_capability_probe_measure_seconds_bucket" + selector + "[$__rate_interval])))", unit: "s"},
		{expr: "sum by (host, block, instance) (urnetwork_capability_probe_in_flight" + selector + ")", unit: "short"},
		{expr: "sum(rate(urnetwork_capability_probe_close_errors_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: "max(urnetwork_capability_probe_last_sweep_seconds" + selector + ")", unit: "s"},
		{expr: "time() - max(urnetwork_capability_probe_last_pass_timestamp_seconds" + selector + ")", unit: "s"},
		{expr: "sum(increase(urnetwork_capability_probe_sweeps_total" + selector + "[1d]))", unit: "short"},
		{expr: "sum by (result) (rate(urnetwork_capability_probe_downgrades_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: "sum(rate(urnetwork_capability_probe_breaker_trips_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: "sum by (step) (rate(urnetwork_capability_probe_readiness_failures_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
		{expr: "sum(rate(urnetwork_capability_probe_shard_cleanup_errors_total" + selector + "[$__rate_interval])) * 60", unit: "short"},
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
				if !strings.Contains(strings.ToLower(panel.Title), "capability") || !strings.Contains(strings.ToLower(panel.Description), "capability") {
					t.Errorf("%q does not say it charts the capability probe", panel.Title)
				}
			}
		}
		if found != 1 {
			t.Errorf("egress probes dashboard charts %s %d times, want once", c.expr, found)
		}
	}
}
