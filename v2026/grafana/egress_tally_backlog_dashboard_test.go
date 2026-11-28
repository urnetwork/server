// the tally backlog gauges sit with the egress probes. each taskworker
// publishes only the shards it drains, so depths are read as the maximum
// across the taskworkers, and the owner read time as an age and a count of
// shards no taskworker has read, which keep a stuck owner visible
package grafana

import (
	"encoding/json"
	"strings"
	"testing"
)

// every backlog gauge is charted once in its unit, scoped to the env's
// taskworkers, including the records, bytes and observed time a stuck or
// saturated owner is diagnosed by
func TestEgressProbeDashboardChartsEveryTallyBacklogGauge(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const selector = `{env="$env",service="taskworker",instance!=""}`
	// the unit a target renders in: its refId's override, else the panel's
	unit := func(panel testPanel, refId string) string {
		for _, override := range panel.FieldConfig.Overrides {
			if override.Matcher.Options != refId {
				continue
			}
			for _, property := range override.Properties {
				var value string
				if property.Id == "unit" && json.Unmarshal(property.Value, &value) == nil {
					return value
				}
			}
		}
		return panel.FieldConfig.Defaults.Unit
	}
	for _, c := range []struct {
		expr string
		unit string
	}{
		{expr: "max by (shard) (urnetwork_provider_egress_tally_queue_fill_ratio" + selector + ")", unit: "percentunit"},
		{expr: "max by (shard) (urnetwork_provider_egress_tally_queue_oldest_pending_seconds" + selector + ")", unit: "s"},
		{expr: "max by (shard) (urnetwork_provider_egress_tally_queue_records" + selector + ")", unit: "short"},
		{expr: "max by (shard) (urnetwork_provider_egress_tally_queue_bytes" + selector + ")", unit: "bytes"},
		{expr: "time() - max by (shard) (urnetwork_provider_egress_tally_queue_observed_timestamp_seconds" + selector + " > 0)", unit: "s"},
		{expr: "count(max by (shard) (urnetwork_provider_egress_tally_queue_observed_timestamp_seconds" + selector + ") == 0) or vector(0)", unit: "short"},
	} {
		found := 0
		for _, panel := range dashboard.Panels {
			for _, target := range panel.Targets {
				if target.Expr != c.expr {
					continue
				}
				found += 1
				if got := unit(panel, target.RefId); got != c.unit {
					t.Errorf("%q renders %s in %q, want %s", panel.Title, c.expr, got, c.unit)
				}
				if !strings.Contains(panel.Description, "taskworker") {
					t.Errorf("%q does not say which processes publish %s", panel.Title, c.expr)
				}
			}
		}
		if found != 1 {
			t.Errorf("egress probes dashboard charts %s %d times, want once", c.expr, found)
		}
	}
}
