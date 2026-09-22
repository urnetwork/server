package grafana

import (
	"regexp"
	"strings"
	"testing"
)

// The extenders dashboard has to survive two things the other dashboards
// already learned the hard way, and both are invisible until production:
//
//   - every stats series is per process and keyed by instance, so a bare
//     selector double counts during a redeploy while the draining and starting
//     processes both push;
//   - the extender population is open, so a query that groups by an unbounded
//     label would grow without limit in mimir.
func TestExtendersDashboardCoversGossipDnsAndPopularity(t *testing.T) {
	dashboard := readTestDashboard(t, "extenders.json")

	if dashboard.Uid != "urnetwork-extenders" {
		t.Errorf("uid = %q", dashboard.Uid)
	}
	if len(dashboard.Templating.List) == 0 {
		t.Error("the dashboard has no env template variable")
	}

	expressions := dashboardExpressions(dashboard)
	if len(expressions) == 0 {
		t.Fatal("the dashboard has no queries")
	}

	// each question the dashboard exists to answer, and the metric that
	// answers it
	for question, metric := range map[string]string{
		"population":            "urnetwork_stats_online_extenders",
		"reachability":          "urnetwork_stats_online_extenders_by_ip_family",
		"addresses in dns":      "urnetwork_taskworker_extender_dns_addresses",
		"record sets in dns":    "urnetwork_taskworker_extender_dns_sets",
		"signed records in dns": "urnetwork_taskworker_extender_dns_txt_records",
		"gossip backlog":        "urnetwork_stats_extender_gossip_pending",
		"gossip staleness":      "urnetwork_stats_extender_gossip_oldest_pending_seconds",
		"released to gossip":    "urnetwork_stats_extender_gossip_released_24h",
		"popularity":            "urnetwork_stats_extender_contracts_total",
		"provider pings":        "urnetwork_stats_extender_provider_pings_total",
		"providers measuring":   "urnetwork_stats_extender_ping_providers_24h",
		"extenders pinged":      "urnetwork_stats_extender_pinged_extenders_24h",
	} {
		found := false
		for _, expression := range expressions {
			if strings.Contains(expression, metric) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no panel answers %q (expected %s)", question, metric)
		}
	}

	// every query must aggregate across the fleet
	bare := regexp.MustCompile(`(^|[^a-z_(])urnetwork_[a-z0-9_]+\{`)
	aggregated := regexp.MustCompile(`\b(max|sum|count|topk|avg)\s*(by\s*\([^)]*\)\s*)?\(`)
	for _, expression := range expressions {
		if !aggregated.MatchString(expression) {
			t.Errorf("query does not aggregate across instances: %q", expression)
			continue
		}
		// a selector that is not inside an aggregation at all
		if bare.MatchString(expression) && !strings.Contains(expression, "(") {
			t.Errorf("query selects a bare series: %q", expression)
		}
	}

	// every query is scoped to the env variable, or it mixes environments
	for _, expression := range expressions {
		if !strings.Contains(expression, `env="$env"`) {
			t.Errorf("query is not scoped to $env: %q", expression)
		}
	}
}

// The contracts counter and the provider pings counter are the only metrics
// that name extenders. Every panel reading either must cap with topk, so a
// dashboard never renders one line per extender in an open population, and
// must read it through increase(): both are counters fed from closed hour
// buckets, and a raw value is the process-lifetime total, which a restart
// resets.
func TestExtendersDashboardLeaderboardIsBounded(t *testing.T) {
	dashboard := readTestDashboard(t, "extenders.json")

	perExtenderCounters := []string{
		"urnetwork_stats_extender_contracts_total",
		"urnetwork_stats_extender_provider_pings_total",
	}
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			if !strings.Contains(target.Expr, "extender_id") {
				continue
			}
			// the only sources of an extender_id series are the per-extender
			// counters, and every use of one must be capped with topk so a
			// panel never renders one line per extender in the fleet
			fromCounter := false
			for _, counter := range perExtenderCounters {
				if strings.Contains(target.Expr, counter) {
					fromCounter = true
				}
			}
			if !fromCounter {
				t.Errorf("panel %q groups by extender_id from an unexpected source: %q",
					panel.Title, target.Expr)
			}
			if !strings.Contains(target.Expr, "topk(") {
				t.Errorf("panel %q shows extender_id series without a topk cap: %q",
					panel.Title, target.Expr)
			}
			// a counter is read through increase(), never as a raw value
			if !strings.Contains(target.Expr, "increase(") {
				t.Errorf("panel %q reads a per-extender counter without increase(): %q",
					panel.Title, target.Expr)
			}
			if panel.Description == "" {
				t.Errorf("panel %q names extenders without explaining the bound", panel.Title)
			}
		}
	}
}

// Every panel carries a description. These dashboards are read during an
// incident by someone who did not write them, and a bare number with no note
// about what "normal" looks like is where the wrong conclusion comes from.
func TestExtendersDashboardPanelsAreDocumented(t *testing.T) {
	dashboard := readTestDashboard(t, "extenders.json")

	for _, panel := range dashboard.Panels {
		if panel.Type == "row" {
			continue
		}
		if strings.TrimSpace(panel.Description) == "" {
			t.Errorf("panel %q has no description", panel.Title)
		}
	}
}
