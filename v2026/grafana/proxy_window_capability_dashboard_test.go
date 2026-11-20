package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The proxy dashboard charts the capability-aware placement of the hosted
// devices (server FP2-PROTOCOL.md §8.2): the live window clients by window
// and labels, the flows by the window of their first exit, ip version and
// protocol, and the flows no window held a compatible exit for. The gauge is
// summed live; the counters are rated, scoped to live proxy workers.
func TestProxyDashboardChartsTheWindowCapabilities(t *testing.T) {
	dashboard := readTestDashboard(t, "proxy.json")
	expressions := dashboardExpressions(dashboard)
	const selector = `{env="$env",service="proxy",block=~"$block",host=~"$host",instance!=""}`
	for _, expression := range []string{
		`sum by (window, ip, protocol) (urnetwork_proxy_window_clients` + selector + `)`,
		`sum by (window, ip_version, protocol) (rate(urnetwork_proxy_window_flows_total` + selector + `[$__rate_interval]))`,
		`sum by (ip_version, protocol) (rate(urnetwork_proxy_window_no_compatible_total` + selector + `[$__rate_interval]))`,
	} {
		if !slices.Contains(expressions, expression) {
			t.Errorf("proxy dashboard is missing %q", expression)
		}
	}
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			if strings.Contains(target.Expr, "urnetwork_proxy_window_clients") || strings.Contains(target.Expr, "urnetwork_proxy_window_flows_total") || strings.Contains(target.Expr, "urnetwork_proxy_window_no_compatible_total") {
				if panel.Type != "timeseries" || !strings.Contains(panel.Description, "FP2-PROTOCOL.md") {
					t.Errorf("panel %q is not a described time series", panel.Title)
				}
			}
		}
	}
}
