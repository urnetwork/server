// Keeps the connect dashboard's placement row (connect/MOVE.md §5.9) on the
// mover's metrics with their units, scopes and caveats.
package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The placement panel by host: providers, residents, CPU and moves in and out,
// with the load model, the mode, dry run, the budget, adoption and the ticks.
// Gauges stay gauges, counters are per-process rates per minute, and every
// panel says what no-data means.
func TestConnectPlacementDashboardShowsTheMover(t *testing.T) {
	const selector = `{env="$env",service="connect",block=~"$block",host=~"$host",instance!=""}`
	// the mover's kinds: same-host drain arrivals are not placement
	const arrivalSelector = `{env="$env",service="connect",block=~"$block",host=~"$host",instance!="",kind=~"advisory|compulsory"}`
	perMinute := func(by string, metric string) string {
		return "sum by (" + by + ") (rate(" + metric + selector + "[$__rate_interval])) * 60"
	}
	assertDiagnosticDashboardPanels(t, "connect.json", []diagnosticDashboardPanel{
		{
			id: 62, unit: "short",
			targets: []testTarget{
				{Expr: "max by (host) (urnetwork_connect_move_host_normalized_load" + selector + ")", LegendFormat: "{{host}}"},
				{Expr: "max(urnetwork_connect_move_fleet_mean_load" + selector + ")", LegendFormat: "fleet mean"},
			},
			descriptionParts: []string{"cores times the inventory move_weight", "{connect_load}", "not being evacuated", "non-draining", "(1 + tolerance)", "No-data", "max over min"},
		},
		{
			id: 63, unit: "short",
			targets: []testTarget{
				{Expr: "max by (host) (urnetwork_connect_move_host_excess" + selector + ")", LegendFormat: "{{host}}"},
			},
			descriptionParts: []string{"upper edge", "0 within the band", "evacuated", "MoveMaxPerTick", "budget", "No-data"},
		},
		{
			id: 64, unit: "short",
			targets: []testTarget{
				{Expr: "sum by (host) (urnetwork_connect_provider_connections" + selector + ")", LegendFormat: "providers {{host}}"},
				{Expr: "sum by (host) (urnetwork_connect_resident_clients" + selector + ")", LegendFormat: "residents {{host}}"},
			},
			descriptionParts: []string{"mover's unit", "share of capacity", "Draining processes"},
		},
		{
			id: 65, unit: "percentunit",
			targets: []testTarget{
				{Expr: "max by (host) (urnetwork_connect_move_host_cpu_busy" + selector + ")", LegendFormat: "{{host}}"},
			},
			descriptionParts: []string{"/proc/stat", "MoveTargetMaxCpu", "MoveEmergencyCpu", "No-data"},
		},
		{
			id: 66, unit: "opm",
			targets: []testTarget{
				{Expr: perMinute("source_host", "urnetwork_connect_moves_sent_total"), LegendFormat: "out {{source_host}}"},
				{Expr: perMinute("target_host", "urnetwork_connect_moves_sent_total"), LegendFormat: "sent to {{target_host}}"},
				{Expr: "sum by (target_host) (rate(urnetwork_connect_moves_arrived_total" + arrivalSelector + "[$__rate_interval])) * 60", LegendFormat: "arrived {{target_host}}"},
			},
			descriptionParts: []string{"handoffs by intent", "advisory and compulsory", "drain arrivals are left out", "the source for sends, the target for arrivals", "declines, failures", "success ratio"},
		},
		{
			id: 67, unit: "opm",
			targets: []testTarget{
				{Expr: perMinute("kind, trigger", "urnetwork_connect_moves_sent_total"), LegendFormat: "{{kind}} {{trigger}}"},
			},
			descriptionParts: []string{"advisory or compulsory", "balance", "arrival", "evacuation", "emergency", "Drain moves", "maxPerMinute"},
		},
		{
			id: 68, unit: "short",
			targets: []testTarget{
				{Expr: "max by (host, block) (urnetwork_connect_mover_mode" + selector + ")", LegendFormat: "{{host}} {{block}}"},
			},
			descriptionParts: []string{"{connect_move}control", "0 off, 1 dry run, 2 on", "MoveDefaultMode", "EnableMove", "cannot read the control"},
		},
		{
			id: 69, unit: "opm",
			targets: []testTarget{
				{Expr: perMinute("source_host, target_host", "urnetwork_connect_moves_would_send_total"), LegendFormat: "would send {{source_host}} to {{target_host}}"},
				{Expr: perMinute("cap", "urnetwork_connect_move_budget_refused_total"), LegendFormat: "refused by the {{cap}} cap"},
			},
			descriptionParts: []string{"dry-run", "did not send", "fleet", "source", "target", "arrival cap"},
		},
		{
			id: 70, unit: "short",
			targets: []testTarget{
				{Expr: "sum by (host) (urnetwork_connect_move_capable_residents" + selector + ")", LegendFormat: "{{host}}"},
				{Expr: "sum(urnetwork_connect_move_capable_residents" + selector + ") / sum(urnetwork_connect_resident_clients" + selector + ")", LegendFormat: "share of all residents"},
			},
			descriptionParts: []string{"resident move version 1", "rollout adoption", "every mode", "no-data"},
		},
		{
			id: 71, unit: "opm",
			targets: []testTarget{
				{Expr: perMinute("outcome", "urnetwork_connect_mover_ticks_total"), LegendFormat: "tick {{outcome}}"},
				{Expr: perMinute("kind, trigger", "urnetwork_connect_move_send_errors_total"), LegendFormat: "send error {{kind}} {{trigger}}"},
				{Expr: perMinute("outcome", "urnetwork_connect_move_load_publishes_total"), LegendFormat: "load publish {{outcome}}"},
			},
			descriptionParts: []string{"store_error", "failed closed", "db_paused", "MoveDbWaitPauseThreshold", "budget_refused", "neither a source nor a target"},
		},
	})

	dashboard := readTestDashboard(t, "connect.json")
	row := dashboardPanelById(dashboard, 61)
	if row == nil || row.Type != "row" || !strings.Contains(row.Title, "placement") {
		t.Fatal("the placement row is missing")
	}
	var placement strings.Builder
	for _, panel := range dashboard.Panels {
		if panel.Id < 62 || 71 < panel.Id {
			continue
		}
		if panel.GridPos.Y <= row.GridPos.Y {
			t.Errorf("panel %d is above the placement row", panel.Id)
		}
		for _, target := range panel.Targets {
			placement.WriteString(target.Expr)
			placement.WriteByte('\n')
		}
	}

	// every metric the mover registers is on the placement row
	moverMetrics := []string{
		"urnetwork_connect_moves_sent_total",
		"urnetwork_connect_moves_would_send_total",
		"urnetwork_connect_move_budget_refused_total",
		"urnetwork_connect_move_send_errors_total",
		"urnetwork_connect_mover_ticks_total",
		"urnetwork_connect_mover_mode",
		"urnetwork_connect_move_capable_residents",
		"urnetwork_connect_move_host_normalized_load",
		"urnetwork_connect_move_fleet_mean_load",
		"urnetwork_connect_move_host_excess",
		"urnetwork_connect_move_host_cpu_busy",
		"urnetwork_connect_move_load_publishes_total",
	}
	registered := registeredApplicationMetrics(t)
	for _, metric := range moverMetrics {
		if !slices.Contains(registered, metric) {
			t.Errorf("%s is not registered by the mover", metric)
		}
		if !strings.Contains(placement.String(), metric) {
			t.Errorf("%s is not on the placement row", metric)
		}
	}

	// gauges are never rated and counters always are
	for _, expression := range strings.Split(strings.TrimSpace(placement.String()), "\n") {
		counter := strings.Contains(expression, "_total")
		if counter != strings.Contains(expression, "rate(") {
			t.Errorf("placement query mixes counter and gauge treatment: %s", expression)
		}
		for _, forbidden := range []string{"vector(0)", " or ", "client_id", "move_id"} {
			if strings.Contains(expression, forbidden) {
				t.Errorf("placement query contains %q: %s", forbidden, expression)
			}
		}
	}
}
