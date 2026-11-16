package grafana

// Since 2026-11-16 the route metrics count a 5xx written after the caller has
// gone as status none (router/metrics.go), so the status panels no longer show
// an abandoned request as a server error. The canceled outcome is where those
// requests stay visible: every service that serves routes through the router
// must chart the request rate by route and outcome, canceled included.

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestRouterServicesChartCanceledOutcomesByRoute(t *testing.T) {
	entries, err := dashboardsFs.ReadDir("dashboards")
	if err != nil {
		t.Fatal(err)
	}
	outcomeFilter := regexp.MustCompile(`outcome=~?"([^"]*)"`)
	charted := map[string]string{}
	for _, entry := range entries {
		dashboard := readTestDashboard(t, entry.Name())
		if slices.Contains(dashboard.Tags, PublicTag) {
			continue
		}
		for _, panel := range dashboard.Panels {
			for _, target := range panel.Targets {
				expression := target.Expr
				if !strings.Contains(expression, "urnetwork_http_requests_total{") {
					continue
				}
				grouping, _, found := strings.Cut(strings.TrimPrefix(expression, "sum by ("), ")")
				if !found || !strings.HasPrefix(expression, "sum by (") {
					continue
				}
				labels := strings.Split(strings.ReplaceAll(grouping, " ", ""), ",")
				if !slices.Contains(labels, "route") || !slices.Contains(labels, "outcome") {
					continue
				}
				// an outcome filter must keep canceled
				if match := outcomeFilter.FindStringSubmatch(expression); match != nil && !strings.Contains(match[1], "canceled") {
					continue
				}
				for _, service := range []string{"api", "alt", "connect", "mcp", "proxy"} {
					if strings.Contains(expression, `service="`+service+`"`) {
						charted[service] = entry.Name() + ": " + panel.Title
					}
				}
			}
		}
	}
	for _, service := range []string{"api", "alt", "connect", "mcp", "proxy"} {
		if charted[service] == "" {
			t.Errorf("no internal dashboard charts %s's urnetwork_http_requests_total by route and outcome with canceled kept", service)
		}
	}
}
