// The proxy test dashboard's lanes and proxy host sections (tests repo,
// tests/proxy-test; P90SPEED.md §1 and §8): the runner's count windows of the
// last 100 setups per lane, per lane and country and per proxy host and block,
// with ready timeouts counted as their wait, and each lane's runs.
package grafana

import (
	"strings"
	"testing"
)

func TestProxyTestDashboardLanesAndProxyHosts(t *testing.T) {
	dashboard := readTestDashboard(t, proxyTestDashboardName)
	expect := func(id int, kind string, unit string, queries map[string]string) {
		t.Helper()
		panel := dashboardPanelById(dashboard, id)
		if panel == nil {
			t.Fatalf("proxy test panel %d is missing", id)
		}
		if panel.Type != kind || panel.FieldConfig.Defaults.Unit != unit {
			t.Errorf("panel %d (%s) is a %s in %q, want a %s in %q", id, panel.Title, panel.Type, panel.FieldConfig.Defaults.Unit, kind, unit)
		}
		if len(panel.Targets) != len(queries) {
			t.Errorf("panel %d (%s) has %d queries, want %d", id, panel.Title, len(panel.Targets), len(queries))
		}
		for refId, want := range queries {
			if got := proxyTestTarget(t, dashboard, id, refId).Expr; got != want {
				t.Errorf("panel %d query %s = %s, want %s", id, refId, got, want)
			}
		}
	}

	// each lane's p90 shows only once its window holds 50 setups, beside the
	// window's size, its timeouts and how far back it reaches
	for _, lane := range []struct {
		name                         string
		p90, samples, timeouts, ages int
	}{
		{"round_robin", proxyTestRoundRobinP90Panel, proxyTestRoundRobinSamplesPanel, proxyTestRoundRobinTimeoutsPanel, proxyTestRoundRobinAgePanel},
		{"us_probe", proxyTestUsProbeP90Panel, proxyTestUsProbeSamplesPanel, proxyTestUsProbeTimeoutsPanel, proxyTestUsProbeAgePanel},
	} {
		expect(lane.p90, "stat", "s", map[string]string{"A": `max(urnetwork_proxy_test_all_setup_window_seconds{env="main",lane="` + lane.name + `",stat="p90"}) and on() (max(urnetwork_proxy_test_all_setup_window_samples{env="main",lane="` + lane.name + `"}) >= 50)`})
		expect(lane.samples, "stat", "none", map[string]string{"A": `max(urnetwork_proxy_test_all_setup_window_samples{env="main",lane="` + lane.name + `"})`})
		expect(lane.timeouts, "stat", "none", map[string]string{"A": `max(urnetwork_proxy_test_all_setup_window_timeouts{env="main",lane="` + lane.name + `"})`})
		expect(lane.ages, "stat", "s", map[string]string{"A": `max(urnetwork_proxy_test_all_setup_window_age_seconds{env="main",lane="` + lane.name + `"})`})
	}
	expect(proxyTestLanePercentilesPanel, "timeseries", "s", map[string]string{"A": `max by (lane, stat) (urnetwork_proxy_test_all_setup_window_seconds{env="main",stat=~"p50|p90"}) and on (lane) (max by (lane) (urnetwork_proxy_test_all_setup_window_samples{env="main"}) >= 50)`})
	expect(proxyTestLaneTimeoutSharePanel, "timeseries", "percentunit", map[string]string{"A": `max by (lane) (urnetwork_proxy_test_all_setup_window_timeouts{env="main"}) / (max by (lane) (urnetwork_proxy_test_all_setup_window_samples{env="main"}) > 0)`})

	// the tables: one row per lane and country, and per proxy host and block
	for _, table := range []struct {
		id      int
		metric  string
		by      string
		country string
	}{
		{proxyTestLaneCountryTablePanel, "urnetwork_proxy_test_setup_window", "lane, country", `,country=~"$country"`},
		{proxyTestProxyHostTablePanel, "urnetwork_proxy_test_proxy_setup_window", "proxy_host, proxy_block", ""},
	} {
		expect(table.id, "table", "", map[string]string{
			"A": `max by (` + table.by + `) (` + table.metric + `_samples{env="main"` + table.country + `})`,
			"B": `max by (` + table.by + `) (` + table.metric + `_timeouts{env="main"` + table.country + `})`,
			"C": `max by (` + table.by + `) (` + table.metric + `_seconds{env="main"` + table.country + `,stat="p50"})`,
			"D": `max by (` + table.by + `) (` + table.metric + `_seconds{env="main"` + table.country + `,stat="p90"})`,
			"E": `max by (` + table.by + `) (` + table.metric + `_seconds{env="main"` + table.country + `,stat="max"})`,
			"F": `max by (` + table.by + `) (` + table.metric + `_age_seconds{env="main"` + table.country + `})`,
		})
		for _, target := range dashboardPanelById(dashboard, table.id).Targets {
			if target.Format != "table" || !target.Instant {
				t.Errorf("table %d query %s is not an instant table query", table.id, target.RefId)
			}
		}
	}
	expect(proxyTestProxyHostP90Panel, "timeseries", "s", map[string]string{"A": `max by (proxy_host, proxy_block) (urnetwork_proxy_test_proxy_setup_window_seconds{env="main",stat="p90"})`})
	expect(proxyTestProxyHostSetupsPanel, "timeseries", "short", map[string]string{"A": `sum by (proxy_host, result) (max by (lane, proxy_host, proxy_block, result) (increase(urnetwork_proxy_test_setups_total{env="main"}[1h])))`})
	expect(proxyTestLaneRunsPanel, "timeseries", "short", map[string]string{
		"A": `max by (lane, outcome) (increase(urnetwork_proxy_test_lane_runs_total{env="main"}[1h]))`,
		"B": `max by (lane) (increase(urnetwork_proxy_test_lane_exit_country_mismatch_total{env="main"}[1h]))`,
	})

	// the descriptions say how a timeout counts, so a reader does not take
	// the p90 for completed setups only
	for _, id := range []int{proxyTestRoundRobinP90Panel, proxyTestUsProbeP90Panel, proxyTestLanePercentilesPanel, proxyTestLaneCountryTablePanel, proxyTestProxyHostTablePanel} {
		if description := dashboardPanelById(dashboard, id).Description; !strings.Contains(description, "60 s") {
			t.Errorf("panel %d does not say a timeout counts as its wait of at least 60 s: %s", id, description)
		}
	}
}
