// the egress site pool panels (the geomap design, §11.3 and §11.4): the
// prober's dark verdicts and batch guards, and the destination pool the daily
// refresh keeps representative. the providers dashboard reads only the gauges:
// the fleet shares, process-local snapshots, from one fresh taskworker, and the
// pool gauges from the taskworker that ran the latest refresh. the event
// counters, each event counted once by the taskworker where it happened, live
// on the egress probes dashboard, summed across the taskworkers
package grafana

import (
	"strings"
	"testing"
)

// every panel is on its dashboard exactly once, under its row, in its unit,
// with one query that reads its metric scoped to the env. the gauges are read
// from the one process whose values are current, the event counters are summed
// across the taskworkers over a window wider than the staggered scrape
// interval, and no event counter is read on the providers dashboard
func TestEgressSitePoolGaugesAndEventCountersSitOnTheirDashboards(t *testing.T) {
	const providers = "providers.json"
	const egressProbes = "egress-probes.json"
	const gaugeRowTitle = "egress site pool and dark verdicts"
	const counterRowTitle = "egress site pool: prober and refresh events"
	// the selection of egress-probes.json panels 5-14: the freshest taskworker
	// snapshot no older than 15 minutes, its timestamp read per process
	const freshSnapshotSelection = `and on(env,service,block,host,instance) topk(1, (max by (env, service, block, host, instance) (urnetwork_egress_probe_fleet_snapshot_timestamp_seconds{env="$env",service="taskworker"}) > time() - 900) and (max by (env, service, block, host, instance) (urnetwork_egress_probe_fleet_snapshot_timestamp_seconds{env="$env",service="taskworker"}) <= time() + 30))`
	// the taskworker that ran the latest pool refresh, the only one whose pool
	// gauges describe the pool as it is
	const latestRefreshSelection = `and on(env,service,block,host,instance) topk(1, max by (env,service,block,host,instance) (urnetwork_egress_site_refresh_last_run_timestamp_seconds{env="$env"}))`

	dashboards := map[string]testDashboard{
		providers:    readTestDashboard(t, providers),
		egressProbes: readTestDashboard(t, egressProbes),
	}
	providersQueries := strings.Join(dashboardExpressions(dashboards[providers]), "\n")

	// the row a panel sits in is the nearest row header above it
	rowTitle := func(dashboard testDashboard, panel testPanel) string {
		title := ""
		rowY := -1
		for _, row := range dashboard.Panels {
			if row.Type == "row" && rowY < row.GridPos.Y && row.GridPos.Y < panel.GridPos.Y {
				title = row.Title
				rowY = row.GridPos.Y
			}
		}
		return title
	}

	cases := []struct {
		dashboard string
		row       string
		title     string
		metric    string
		unit      string
		// the process selection that must follow the metric's own selector
		selection string
		// a per-process event counter, summed across the taskworkers by this label
		summedBy string
	}{
		{dashboard: providers, row: gaugeRowTitle, title: "fleet dark share", metric: "urnetwork_egress_probe_fleet_dark_share", unit: "percentunit", selection: freshSnapshotSelection},
		{dashboard: providers, row: gaugeRowTitle, title: "fleet failure share by class", metric: "urnetwork_egress_probe_fleet_failure_share", unit: "percentunit", selection: freshSnapshotSelection},
		{dashboard: providers, row: gaugeRowTitle, title: "batch guard share", metric: "urnetwork_egress_probe_batch_share", unit: "percentunit"},
		{dashboard: providers, row: gaugeRowTitle, title: "site failure share (worst 15)", metric: "urnetwork_egress_site_failure_share", unit: "percentunit", selection: latestRefreshSelection},
		{dashboard: providers, row: gaugeRowTitle, title: "site pool by class", metric: "urnetwork_egress_site_pool_size", unit: "short", selection: latestRefreshSelection},
		{dashboard: providers, row: gaugeRowTitle, title: "site pool refresh, last run", metric: "urnetwork_egress_site_refresh_last_run_timestamp_seconds", unit: "s"},
		{dashboard: egressProbes, row: counterRowTitle, title: "batch guard trips", metric: "urnetwork_egress_probe_batch_guard_trips_total", unit: "short", summedBy: "schedule"},
		{dashboard: egressProbes, row: counterRowTitle, title: "not measured providers", metric: "urnetwork_egress_probe_pass_not_measured_total", unit: "short", summedBy: "schedule"},
		{dashboard: egressProbes, row: counterRowTitle, title: "destination pool fetches", metric: "urnetwork_egress_probe_pool_fetches_total", unit: "short", summedBy: "result"},
		{dashboard: egressProbes, row: counterRowTitle, title: "site pool refresh runs", metric: "urnetwork_egress_site_refresh_runs_total", unit: "short", summedBy: "result"},
		{dashboard: egressProbes, row: counterRowTitle, title: "site pool changes", metric: "urnetwork_egress_site_changes_total", unit: "short", summedBy: "change"},
	}
	for _, c := range cases {
		dashboard := dashboards[c.dashboard]
		panels := []testPanel{}
		for _, panel := range dashboard.Panels {
			if panel.Title == c.title {
				panels = append(panels, panel)
			}
		}
		if len(panels) != 1 {
			t.Errorf("%s has %d panels titled %q, want 1", c.dashboard, len(panels), c.title)
			continue
		}
		panel := panels[0]
		if row := rowTitle(dashboard, panel); row != c.row {
			t.Errorf("%s panel %q sits in the row %q, want %q", c.dashboard, c.title, row, c.row)
		}
		if panel.FieldConfig.Defaults.Unit != c.unit {
			t.Errorf("%s panel %q shows %q, want %q", c.dashboard, c.title, panel.FieldConfig.Defaults.Unit, c.unit)
		}
		if len(panel.Targets) != 1 {
			t.Errorf("%s panel %q has %d queries, want 1", c.dashboard, c.title, len(panel.Targets))
			continue
		}

		expression := panel.Targets[0].Expr
		offsets := metricOccurrences(expression, c.metric)
		if len(offsets) == 0 {
			t.Errorf("%s panel %q does not read %s: %s", c.dashboard, c.title, c.metric, expression)
		}
		for _, offset := range offsets {
			if !strings.HasPrefix(expression[offset+len(c.metric):], `{env="$env"`) {
				t.Errorf("%s panel %q reads %s without the env selector: %s", c.dashboard, c.title, c.metric, expression)
			}
		}
		if c.selection != "" && !strings.Contains(expression, c.metric+`{env="$env"} `+c.selection) {
			t.Errorf("%s panel %q does not read %s from its one current process: %s", c.dashboard, c.title, c.metric, expression)
		}
		if c.summedBy != "" {
			read := "sum by (" + c.summedBy + ") (increase(" + c.metric + `{env="$env"}[5m]))`
			if expression != read {
				t.Errorf("%s panel %q reads the per-process %s as %s, want %s", c.dashboard, c.title, c.metric, expression, read)
			}
			// the providers dashboard reads only replicated gauges, with max
			if 0 < len(metricOccurrences(providersQueries, c.metric)) {
				t.Errorf("the per-process counter %s is read on %s, which reads only gauges", c.metric, providers)
			}
		}
	}
}

// every panel of the dashboards the site pool panels were added to carries an
// id of its own, so panel links and edits stay stable as rows are appended
func TestEgressSitePoolDashboardPanelIdsAreUnique(t *testing.T) {
	for _, name := range []string{"providers.json", "egress-probes.json"} {
		dashboard := readTestDashboard(t, name)
		panelIdTitles := map[int]string{}
		for _, panel := range dashboard.Panels {
			if panel.Id == 0 {
				t.Errorf("%s panel %q has no id", name, panel.Title)
				continue
			}
			if title, ok := panelIdTitles[panel.Id]; ok {
				t.Errorf("%s panel id %d is shared by %q and %q", name, panel.Id, title, panel.Title)
			}
			panelIdTitles[panel.Id] = panel.Title
		}
	}
}
