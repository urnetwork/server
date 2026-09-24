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
		"pings per extender":    "urnetwork_stats_extender_pings_total",
		"pings by outcome":      "urnetwork_stats_extender_pings_24h",
		"who is measuring":      "urnetwork_stats_extender_ping_sources_24h",
		"extenders pinged":      "urnetwork_stats_extender_pinged_extenders_24h",
		"refusing targets":      "urnetwork_stats_extender_ping_rejections_total",
		"derived locations":     "urnetwork_stats_derived_locations",
		"derived crossings":     "urnetwork_stats_derived_location_crossings",
		"excluded sources":      "urnetwork_stats_derive_excluded_sources",
		"derivation residual":   "urnetwork_stats_derive_residual_km",
		"derivation staleness":  "urnetwork_stats_derive_last_run_seconds",
		"solve time and budget": "urnetwork_stats_derive_solve_seconds",
		"solve heap and budget": "urnetwork_stats_derive_solve_bytes",
		"reading the day":       "urnetwork_stats_derive_ingest_seconds",
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

// The contracts, pings and ping rejections counters are the only metrics
// that name extenders. Every panel reading one must cap with topk, so a
// dashboard never renders one line per extender in an open population, and
// must read it through increase(): all are counters fed from closed hour
// buckets, and a raw value is the process-lifetime total, which a restart
// resets.
func TestExtendersDashboardLeaderboardIsBounded(t *testing.T) {
	dashboard := readTestDashboard(t, "extenders.json")

	perExtenderCounters := []string{
		"urnetwork_stats_extender_contracts_total",
		"urnetwork_stats_extender_pings_total",
		"urnetwork_stats_extender_ping_rejections_total",
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

// The ping row answers what GEOMAP §2.7 asks of it: pings by kind and outcome
// over time, split direct from relayed; extender to extender pings per hour;
// who is measuring; and the targets refusing the most, as a rate over the
// range, with the co-signature named as the reason the panel exists.
func TestExtendersDashboardPingRow(t *testing.T) {
	dashboard := readTestDashboard(t, "extenders.json")

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

	// the verdict window, split by kind, verdict and relay, over time
	outcomes := panelWith(t, func(expr string) bool {
		return strings.HasPrefix(expr, "max by (pinger_kind, outcome, relayed) (urnetwork_stats_extender_pings_24h")
	})
	if outcomes.Type != "timeseries" {
		t.Errorf("pings by kind and outcome is a %s, want a timeseries", outcomes.Type)
	}
	// a relayed ping is described where it is shown: it counts, and it is
	// never a solver term
	for _, want := range []string{"relayed", "NLayer", "never a solver term"} {
		if !strings.Contains(outcomes.Description, want) {
			t.Errorf("pings by kind and outcome description lacks %q", want)
		}
	}

	// extender to extender pings per hour, from the per-kind counter
	panelWith(t, func(expr string) bool {
		return strings.Contains(expr, "urnetwork_stats_extender_pings_total") &&
			strings.Contains(expr, `pinger_kind="extender"`) &&
			strings.Contains(expr, "[1h]")
	})

	// providers and extenders measuring, each on its own
	for _, pingerKind := range []string{"provider", "extender"} {
		panelWith(t, func(expr string) bool {
			return strings.Contains(expr, "urnetwork_stats_extender_ping_sources_24h") &&
				strings.Contains(expr, `pinger_kind="`+pingerKind+`"`)
		})
	}

	// the refusing targets: rejections over pings per target, capped, as a
	// rate, and explained as the evidence the co-signature exists for
	refusing := panelWith(t, func(expr string) bool {
		return strings.Contains(expr, "urnetwork_stats_extender_ping_rejections_total") &&
			strings.Contains(expr, "urnetwork_stats_extender_pings_total") &&
			strings.Contains(expr, "/")
	})
	if refusing.Type != "table" {
		t.Errorf("targets refusing the most is a %s, want a table", refusing.Type)
	}
	if refusing.FieldConfig.Defaults.Unit != "percentunit" {
		t.Errorf("targets refusing the most shows %q, want a percentunit rate", refusing.FieldConfig.Defaults.Unit)
	}
	for _, want := range []string{"co-signature", "§2.7"} {
		if !strings.Contains(refusing.Description, want) {
			t.Errorf("targets refusing the most description lacks %q", want)
		}
	}
	for _, target := range refusing.Targets {
		if !strings.Contains(target.Expr, "topk(") || !strings.Contains(target.Expr, "[$__range]") {
			t.Errorf("targets refusing the most is not a capped rate over the range: %q", target.Expr)
		}
	}

	// the retired provider-only names are gone with the rows they counted
	for _, expression := range dashboardExpressions(dashboard) {
		for _, retired := range []string{
			"urnetwork_stats_extender_provider_pings_total",
			"urnetwork_stats_extender_provider_pings_24h",
			"urnetwork_stats_extender_ping_providers_24h",
		} {
			if strings.Contains(expression, retired) {
				t.Errorf("query reads the retired %s: %q", retired, expression)
			}
		}
	}
}

// The derived-location row answers what GEOMAP §5.3, §5.4, §5.5 and §5.7 ask
// of the derive phase before anything reads it: how many nodes are derived,
// how many of them crossed a region or country line, which sources the last
// derivation excluded, how well it fit against genesis, whether it is still
// running, and how close the next run is projected to the solve's budget.
func TestExtendersDashboardDerivedLocationRow(t *testing.T) {
	dashboard := readTestDashboard(t, "extenders.json")

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

	rowFound := false
	for _, panel := range dashboard.Panels {
		if panel.Type == "row" && panel.Title == "derived locations" {
			rowFound = true
		}
	}
	if !rowFound {
		t.Fatal("no derived locations row")
	}

	// the published count, both kinds, over time
	derived := panelWith(t, func(expr string) bool {
		return strings.HasPrefix(expr, "sum(max by (node_kind) (urnetwork_stats_derived_locations")
	})
	for _, want := range []string{"§5.4", "genesis", "§5.7"} {
		if !strings.Contains(derived.Description, want) {
			t.Errorf("derived locations description lacks %q", want)
		}
	}
	panelWith(t, func(expr string) bool {
		return strings.HasPrefix(expr, "max by (node_kind) (urnetwork_stats_derived_locations")
	})

	// each kind of crossing on its own, and both over time
	for _, kind := range []string{"region", "country"} {
		crossing := panelWith(t, func(expr string) bool {
			return strings.Contains(expr, "urnetwork_stats_derived_location_crossings") &&
				strings.Contains(expr, `kind="`+kind+`"`)
		})
		if !strings.Contains(crossing.Description, "§5.2") {
			t.Errorf("crossed %s does not say the solve priced the crossing (§5.2)", kind)
		}
	}
	panelWith(t, func(expr string) bool {
		return strings.HasPrefix(expr, "max by (kind) (urnetwork_stats_derived_location_crossings")
	})

	// the excluded sources, explained as reputation
	excluded := panelWith(t, func(expr string) bool {
		return strings.Contains(expr, "urnetwork_stats_derive_excluded_sources")
	})
	if !strings.Contains(excluded.Description, "§5.5") {
		t.Error("excluded sources does not cite the reputation of §5.5")
	}

	// the residual at the derived positions against genesis, in km
	residual := panelWith(t, func(expr string) bool {
		return strings.HasPrefix(expr, "max by (at) (urnetwork_stats_derive_residual_km")
	})
	if residual.Type != "timeseries" || residual.FieldConfig.Defaults.Unit != "lengthkm" {
		t.Errorf("ping residual is a %s in %q, want a timeseries in km", residual.Type, residual.FieldConfig.Defaults.Unit)
	}
	if !strings.Contains(residual.Description, "genesis") {
		t.Error("ping residual does not compare against genesis")
	}

	// the time since the last derivation, which turns amber after one late
	// run and red after two missed; a derivation is due every 8 hours
	staleness := panelWith(t, func(expr string) bool {
		return strings.Contains(expr, "urnetwork_stats_derive_last_run_seconds")
	})
	for _, target := range staleness.Targets {
		if !strings.HasPrefix(target.Expr, "time() - max(urnetwork_stats_derive_last_run_seconds") {
			t.Errorf("the time since the last derivation is not measured from now: %q", target.Expr)
		}
	}
	if staleness.FieldConfig.Defaults.Unit != "s" {
		t.Errorf("the time since the last derivation shows %q, want seconds", staleness.FieldConfig.Defaults.Unit)
	}
	const deriveIntervalSeconds = 8 * 60 * 60
	steps := staleness.FieldConfig.Defaults.Thresholds.Steps
	if len(steps) < 2 || steps[1].Value == nil || !(deriveIntervalSeconds < *steps[1].Value) {
		t.Errorf("the first staleness threshold is not past the 8 hour cadence: %+v", steps)
	}
	if !strings.Contains(staleness.Description, "§5.7") {
		t.Error("the time since the last derivation does not cite the cadence of §5.7")
	}

	// the capacity of the derive job: the planner's projection as a share of
	// each budget, amber at the margin SIGNALS.md §2.19c finds it at, and the
	// measured, projected and budget lines over time
	for _, metric := range []string{"urnetwork_stats_derive_solve_seconds", "urnetwork_stats_derive_solve_bytes"} {
		share := panelWith(t, func(expr string) bool {
			return strings.Contains(expr, metric+`{env="$env",at="projected"}) / max(`+metric+`{env="$env",at="budget"})`)
		})
		if share.FieldConfig.Defaults.Unit != "percentunit" {
			t.Errorf("the projected share of %s shows %q, want a share", metric, share.FieldConfig.Defaults.Unit)
		}
		shareSteps := share.FieldConfig.Defaults.Thresholds.Steps
		if len(shareSteps) < 3 || shareSteps[1].Value == nil || *shareSteps[1].Value != 0.8 || shareSteps[2].Value == nil || *shareSteps[2].Value != 1 {
			t.Errorf("the projected share of %s is not amber at the capacity margin and red at the budget: %+v", metric, shareSteps)
		}
		for _, want := range []string{"§5.3", "§2.19c"} {
			if !strings.Contains(share.Description, want) {
				t.Errorf("the projected share of %s does not cite %s", metric, want)
			}
		}
		over := panelWith(t, func(expr string) bool {
			return strings.HasPrefix(expr, "max by (at) ("+metric)
		})
		if over.Type != "timeseries" {
			t.Errorf("%s over time is a %s", metric, over.Type)
		}
	}
	panelWith(t, func(expr string) bool {
		return strings.HasPrefix(expr, "max(urnetwork_stats_derive_ingest_seconds")
	})
}
