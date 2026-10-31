package grafana

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The core signals of the 2026-10-26 incidents, with the SIGNALS.md section 26
// levels: services-tier CPU saturation that queued the database behind
// PgBouncer, and the proxy contract-compensation storm. Neither had a panel,
// so both were found hours late. A WARN level is orange and a PAGE level red.
type incidentSignalPanel struct {
	dashboard string
	id        int
	panelType string
	unit      string
	selectors []string
	levels    []incidentLevel
}

type incidentLevel struct {
	color string
	value float64
}

func warnAt(value float64) incidentLevel { return incidentLevel{color: "orange", value: value} }
func pageAt(value float64) incidentLevel { return incidentLevel{color: "red", value: value} }

func TestIncidentSignalsCarryTheirThresholds(t *testing.T) {
	const pgbouncerWaiting = `sum(avg_over_time(pgbouncer_pool_clients_waiting{env="$env",job="pgbouncer"}[5m]))`
	const oldestWait = `max(max_over_time(pgbouncer_pool_maxwait_seconds{env="$env",job="pgbouncer"}[5m]))`
	const acquireWait = `urnetwork_pg_pool_acquire_duration_seconds_total{env="$env",pool="default",instance!=""}`
	const busy = `mode=~"idle|iowait"`
	const mainThread = `redis_cpu_sys_main_thread_seconds_total{`
	const hello = `redis_commands_total{`
	const proxy = `service="proxy",block=~"$block",host=~"$host",instance!=""`
	const failed = `outcome=~"canceled|panic"`
	const returned = `outcome=~"contract_reply|canceled|panic"`
	const settled = `(time() - max by (host, block) (process_start_time_seconds{env="$env",` + proxy + `}) > 1800)`
	signals := []incidentSignalPanel{
		{"services-overview.json", 17, "stat", "short", []string{pgbouncerWaiting}, []incidentLevel{warnAt(100), pageAt(1000)}},
		{"services-overview.json", 18, "stat", "s", []string{acquireWait, `outcome="acquired"`, "[5m]"}, []incidentLevel{warnAt(0.1)}},
		{"services-overview.json", 19, "stat", "percentunit", []string{busy, `go_goroutines{env="$env",service="connect",instance!=""}`, "1 - sum(sum by (host) (rate(node_cpu_seconds_total{"}, []incidentLevel{warnAt(0.8)}},
		{"services-overview.json", 20, "stat", "percentunit", []string{busy, `go_goroutines{env="$env",service="proxy",instance!=""}`}, []incidentLevel{warnAt(0.85)}},
		{"services-overview.json", 21, "stat", "short", []string{`max_over_time(urnetwork_contract_creation_stage_inflight{env="$env",stage="other",instance!=""}[2m])`, "sum by (service, host, block, instance)"}, []incidentLevel{pageAt(1000)}},
		{"services-overview.json", 22, "stat", "percentunit", []string{failed, returned, "sum by (service)", ">= 20)"}, []incidentLevel{warnAt(0.05), pageAt(0.2)}},
		{"services-overview.json", 23, "stat", "ops", []string{`operation="release-pending",result="error"`, "max(sum by (service)", "[5m]"}, []incidentLevel{warnAt(10)}},
		{"services-overview.json", 24, "stat", "ops", []string{`urnetwork_redis_pool_timeouts_total{env="$env",instance!=""}[5m]`, "sum by (service)"}, []incidentLevel{warnAt(10), pageAt(1000)}},
		{"services-overview.json", 25, "stat", "percentunit", []string{mainThread, "redis_cpu_user_main_thread_seconds_total{", "[5m]"}, []incidentLevel{warnAt(0.85)}},
		{"services-overview.json", 26, "stat", "ops", []string{hello, `cmd="hello"`, "[5m]"}, []incidentLevel{warnAt(500)}},
		{"postgres.json", 17, "stat", "short", []string{pgbouncerWaiting}, []incidentLevel{warnAt(100), pageAt(1000)}},
		{"postgres.json", 18, "stat", "s", []string{oldestWait}, []incidentLevel{warnAt(0.5)}},
		{"postgres.json", 19, "stat", "s", []string{acquireWait, "[5m]"}, []incidentLevel{warnAt(0.1)}},
		{"postgres.json", 21, "timeseries", "short", []string{pgbouncerWaiting}, []incidentLevel{warnAt(100), pageAt(1000)}},
		{"postgres.json", 23, "timeseries", "s", []string{oldestWait}, []incidentLevel{warnAt(0.5)}},
		{"postgres.json", 27, "timeseries", "s", []string{acquireWait}, []incidentLevel{warnAt(0.1)}},
		{"postgres.json", 31, "timeseries", "percentunit", []string{busy, `go_goroutines{env="$env",service="connect",instance!=""}`, "1 - sum(sum by (host)"}, []incidentLevel{warnAt(0.8), warnAt(0.85)}},
		{"redis-cluster.json", 20, "stat", "percentunit", []string{mainThread, "[5m]"}, []incidentLevel{warnAt(0.85)}},
		{"redis-cluster.json", 21, "stat", "ops", []string{hello, `cmd="hello"`, "[5m]"}, []incidentLevel{warnAt(500)}},
		{"redis-cluster.json", 22, "stat", "ops", []string{"urnetwork_redis_pool_timeouts_total", "sum by (service)"}, []incidentLevel{warnAt(10), pageAt(1000)}},
		{"redis-cluster.json", 25, "timeseries", "percentunit", []string{mainThread, `node=~"$node"`, "[5m]"}, []incidentLevel{warnAt(0.85)}},
		{"redis-cluster.json", 26, "timeseries", "ops", []string{hello, `node=~"$node"`, `cmd="hello"`, "[5m]"}, []incidentLevel{warnAt(500)}},
		{"redis-cluster.json", 16, "timeseries", "ops", []string{"urnetwork_redis_pool_timeouts_total"}, []incidentLevel{warnAt(10), pageAt(1000)}},
		{"proxy.json", 42, "stat", "short", []string{"max_over_time(urnetwork_contract_creation_stage_inflight{", proxy + `,stage="other"}[2m])`}, []incidentLevel{pageAt(1000)}},
		{"proxy.json", 43, "stat", "percentunit", []string{proxy + "," + failed, proxy + "," + returned, ">= 20)"}, []incidentLevel{warnAt(0.05), pageAt(0.2)}},
		{"proxy.json", 44, "stat", "ops", []string{proxy + `,operation="release-pending",result="error"`}, []incidentLevel{warnAt(10)}},
		{"proxy.json", 45, "stat", "ops", []string{"urnetwork_redis_pool_timeouts_total{" + `env="$env",` + proxy}, []incidentLevel{warnAt(10), pageAt(1000)}},
		{"proxy.json", 46, "stat", "percentunit", []string{busy, "go_goroutines{"}, []incidentLevel{warnAt(0.85)}},
		{"proxy.json", 47, "stat", "ops", []string{"urnetwork_hosted_contract_admission_refusals_total{", "and on (host, block) " + settled}, []incidentLevel{warnAt(50)}},
		{"proxy.json", 49, "timeseries", "short", []string{proxy + `,stage="other"}[2m])`, "max by (host, block)"}, []incidentLevel{pageAt(1000)}},
		{"proxy.json", 51, "timeseries", "percentunit", []string{proxy + "," + failed, proxy + "," + returned}, []incidentLevel{warnAt(0.05), pageAt(0.2)}},
		{"proxy.json", 53, "timeseries", "ops", []string{"urnetwork_hosted_contract_admission_refusals_total{", "and on (host, block) " + settled}, []incidentLevel{warnAt(50)}},
		{"proxy.json", 55, "timeseries", "ops", []string{`operation="compensation",result=~"dropped|expired"`}, []incidentLevel{warnAt(1)}},
		{"proxy.json", 56, "timeseries", "ops", []string{"urnetwork_redis_pool_timeouts_total{", "urnetwork_redis_pool_misses_total{"}, []incidentLevel{warnAt(10), pageAt(1000)}},
		{"proxy.json", 57, "timeseries", "percentunit", []string{busy, "go_goroutines{"}, []incidentLevel{warnAt(0.85)}},
		{"proxy.json", 61, "timeseries", "percentunit", []string{proxy + `,outcome="contract_reply"}[5m]))[2h:5m] offset 10m)`, "quantile_over_time(0.5, "}, []incidentLevel{{color: "transparent", value: 0.25}}},
	}
	for _, signal := range signals {
		panel := dashboardPanelById(readTestDashboard(t, signal.dashboard), signal.id)
		if panel == nil {
			t.Errorf("%s has no incident signal panel %d", signal.dashboard, signal.id)
			continue
		}
		where := signal.dashboard + " panel " + panel.Title
		if panel.Type != signal.panelType || panel.FieldConfig.Defaults.Unit != signal.unit {
			t.Errorf("%s is a %s in %q, want a %s in %q", where, panel.Type, panel.FieldConfig.Defaults.Unit, signal.panelType, signal.unit)
		}
		expressions := []string{}
		for _, target := range panel.Targets {
			expressions = append(expressions, target.Expr)
		}
		joined := strings.Join(expressions, "\n")
		for _, selector := range signal.selectors {
			if !strings.Contains(joined, selector) {
				t.Errorf("%s does not read %s: %s", where, selector, joined)
			}
		}
		levels := []incidentLevel{}
		for _, step := range panel.FieldConfig.Defaults.Thresholds.Steps {
			if step.Value != nil {
				levels = append(levels, incidentLevel{color: step.Color, value: *step.Value})
			}
		}
		if !slices.Equal(levels, signal.levels) {
			t.Errorf("%s levels are %v, want the SIGNALS.md section 26 levels %v", where, levels, signal.levels)
		}
		if signal.panelType == "timeseries" && !strings.HasPrefix(panel.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode, "line") {
			t.Errorf("%s does not draw its thresholds as lines", where)
		}
		if !strings.Contains(panel.Description, "SIGNALS.md section 26") {
			t.Errorf("%s does not name the source of its threshold", where)
		}
	}
}

// SIGNALS.md section 25.1's failure share leaves protocol_reject out of both
// sides: the api's rejects run 2,000-7,000/s against 190-360 replies/s, so a
// share of every outcome reads healthy through a storm.
func TestIncidentFailureShareLeavesProtocolRejectsOut(t *testing.T) {
	selector := regexp.MustCompile(`urnetwork_contract_creation_completed_total\{[^}]*\}`)
	for _, c := range []struct {
		dashboard string
		id        int
	}{{"services-overview.json", 22}, {"proxy.json", 43}, {"proxy.json", 51}} {
		panel := dashboardPanelById(readTestDashboard(t, c.dashboard), c.id)
		if panel == nil || len(panel.Targets) != 1 {
			t.Errorf("%s failure share panel %d is missing", c.dashboard, c.id)
			continue
		}
		selectors := selector.FindAllString(panel.Targets[0].Expr, -1)
		if len(selectors) != 2 || !strings.Contains(selectors[0], `outcome=~"canceled|panic"`) ||
			!strings.Contains(selectors[1], `outcome=~"contract_reply|canceled|panic"`) {
			t.Errorf("%s panel %q is not (canceled + panic) / (contract_reply + canceled + panic): %s", c.dashboard, panel.Title, panel.Targets[0].Expr)
		}
		if !strings.Contains(panel.Description, "25.1") || !strings.Contains(panel.Description, "protocol_reject") {
			t.Errorf("%s panel %q does not explain its definition", c.dashboard, panel.Title)
		}
	}
}

// The rules hold for different times, and the descriptions say so: the
// PgBouncer WARN needs 10 minutes, a single services edge 30 minutes, and the
// cap counts a process only once it has run 30 minutes.
func TestIncidentSignalHoldsAreDescribed(t *testing.T) {
	for _, c := range []struct {
		dashboard string
		id        int
		parts     []string
	}{
		{"postgres.json", 17, []string{"10 minutes above 100", "0.5 s for 10 minutes", "5 minutes above 1,000"}},
		{"services-overview.json", 17, []string{"10 minutes above 100", "0.5 s for 10 minutes"}},
		{"postgres.json", 21, []string{"WARN above 100 for 10 minutes"}},
		{"postgres.json", 18, []string{"0.5 s for 10 minutes"}},
		{"postgres.json", 23, []string{"0.5 s for 10 minutes"}},
		{"services-overview.json", 19, []string{"80% for 10 minutes", "85% for 30 minutes"}},
		{"postgres.json", 31, []string{"80% for 10 minutes", "85% for 30 minutes"}},
		{"proxy.json", 47, []string{"run 30 minutes", "12-21 minutes"}},
		{"proxy.json", 53, []string{"30 minutes or more", "12-21 minutes"}},
	} {
		panel := dashboardPanelById(readTestDashboard(t, c.dashboard), c.id)
		if panel == nil {
			t.Errorf("%s panel %d is missing", c.dashboard, c.id)
			continue
		}
		for _, part := range c.parts {
			if !strings.Contains(panel.Description, part) {
				t.Errorf("%s panel %q does not say %q", c.dashboard, panel.Title, part)
			}
		}
	}
	// the busiest edge carries the single-host level beside the four together
	edges := dashboardPanelById(readTestDashboard(t, "services-overview.json"), 19)
	if edges == nil || len(edges.Targets) != 2 || edges.Targets[0].LegendFormat != "4 edges" || edges.Targets[1].LegendFormat != "busiest edge" {
		t.Fatal("services-edge CPU does not show the four edges together beside the busiest one")
	}
	busiest := []incidentLevel{}
	for _, override := range edges.FieldConfig.Overrides {
		for _, property := range override.Properties {
			if override.Matcher.Options == "busiest edge" && property.Id == "thresholds" {
				var thresholds struct {
					Steps []struct {
						Color string   `json:"color"`
						Value *float64 `json:"value"`
					} `json:"steps"`
				}
				if err := json.Unmarshal(property.Value, &thresholds); err != nil {
					t.Fatal(err)
				}
				for _, step := range thresholds.Steps {
					if step.Value != nil {
						busiest = append(busiest, incidentLevel{color: step.Color, value: *step.Value})
					}
				}
			}
		}
	}
	if !slices.Equal(busiest, []incidentLevel{warnAt(0.85)}) {
		t.Errorf("the busiest edge's levels are %v, want orange at 85%%", busiest)
	}
}

// The overview's incident row leads the dashboard and reads the whole env: a
// backlog or storm must show whatever service, block or host is picked.
func TestServicesOverviewIncidentRowIsFleetWideAndFirst(t *testing.T) {
	dashboard := readTestDashboard(t, "services-overview.json")
	row := dashboardPanelById(dashboard, 16)
	if row == nil || row.Type != "row" || row.GridPos.Y != 0 || !strings.Contains(row.Title, "incident signals") {
		t.Fatal("the incident signals row does not lead the services overview")
	}
	for id := 17; id <= 26; id++ {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil || panel.Type != "stat" || 9 <= panel.GridPos.Y {
			t.Errorf("incident stat %d is missing or below the incident row", id)
			continue
		}
		for _, target := range panel.Targets {
			for _, picker := range []string{"$service", "$block", "$host"} {
				if strings.Contains(target.Expr, picker) {
					t.Errorf("incident stat %q follows the %s picker: %s", panel.Title, picker, target.Expr)
				}
			}
		}
	}
	for _, target := range []string{"/d/urnetwork-postgres", "/d/urnetwork-redis-cluster", "/d/urnetwork-proxy"} {
		if !slices.ContainsFunc(dashboard.Links, func(link testDashboardLink) bool { return link.Url == target && link.KeepTime }) {
			t.Errorf("services overview does not link %s with the time range", target)
		}
	}
}

// The reservation panel highlights the results that leave tokens reserved or
// mean Redis was unreachable, and the compensation panel names the deferred
// owner's outcomes, which read no data until a build carrying them is deployed.
// Beside them, on their own right axis, it shows how full the busiest process's
// owner is: the balances it holds and the release calls it has in flight.
func TestProxyStormRowHighlightsFailedReservations(t *testing.T) {
	dashboard := readTestDashboard(t, "proxy.json")
	reservations := dashboardPanelById(dashboard, 54)
	if reservations == nil || len(reservations.Targets) != 1 ||
		!strings.Contains(reservations.Targets[0].Expr, "sum by (operation, result) (rate(urnetwork_redis_contract_reservation_total{") {
		t.Fatal("proxy reservation results panel is missing")
	}
	highlighted := map[string]string{}
	for _, override := range reservations.FieldConfig.Overrides {
		for _, property := range override.Properties {
			if property.Id == "color" {
				highlighted[override.Matcher.Options] = string(property.Value)
			}
		}
	}
	if !strings.Contains(highlighted[".* (error|dropped|expired)"], `"red"`) || !strings.Contains(highlighted[".* pending"], `"orange"`) {
		t.Errorf("reservation results do not highlight error, dropped, expired and pending: %v", highlighted)
	}
	compensation := dashboardPanelById(dashboard, 55)
	if compensation == nil || len(compensation.Targets) != 4 ||
		!strings.Contains(compensation.Targets[0].Expr, `operation="compensation"`) ||
		compensation.Targets[0].LegendFormat != "{{result}}" {
		t.Fatal("proxy compensation results panel is missing")
	}
	named := []string{}
	for _, override := range compensation.FieldConfig.Overrides {
		named = append(named, override.Matcher.Options)
	}
	for _, result := range []string{"completed", "deferred", "pending", "dropped", "expired"} {
		if !slices.Contains(named, result) || !strings.Contains(compensation.Description, result) {
			t.Errorf("compensation panel does not name result %s", result)
		}
	}
	if !strings.Contains(compensation.Description, "handoff") {
		t.Error("compensation panel does not explain the give-up handoff")
	}
	for index, gauge := range []struct{ metric, bound string }{
		{"urnetwork_redis_contract_compensation_balances_held", "1,024"},
		{"urnetwork_redis_contract_compensation_calls_inflight", "4 workers"},
	} {
		target := compensation.Targets[2+index]
		// a per-process bound, so the busiest process, not the fleet's sum
		if !strings.HasPrefix(target.Expr, "max("+gauge.metric+"{") || !strings.Contains(target.Expr, `service="proxy"`) ||
			!strings.Contains(target.Expr, `env="$env"`) || !strings.Contains(target.Expr, `instance!=""`) {
			t.Errorf("compensation panel does not read the busiest process's %s: %s", gauge.metric, target.Expr)
			continue
		}
		if !strings.Contains(compensation.Description, gauge.bound) {
			t.Errorf("compensation panel does not give the %s bound %s", gauge.metric, gauge.bound)
		}
		properties := map[string]string{}
		for _, override := range compensation.FieldConfig.Overrides {
			if override.Matcher.Options == target.LegendFormat {
				for _, property := range override.Properties {
					properties[property.Id] = string(property.Value)
				}
			}
		}
		if properties["custom.axisPlacement"] != `"right"` || properties["unit"] != `"short"` || !strings.Contains(properties["custom.scaleDistribution"], `"linear"`) {
			t.Errorf("%s is not a count on its own linear right axis: %v", gauge.metric, properties)
		}
	}
}

// The cap's slots in use sit directly beside its refusals, each process
// against the capacity it runs with, so the headroom reads at a glance and a
// configured cap shows as its own line.
func TestProxyStormRowShowsCapSlotsAgainstCapacityBesideRefusals(t *testing.T) {
	dashboard := readTestDashboard(t, "proxy.json")
	slots := dashboardPanelById(dashboard, 62)
	refusals := dashboardPanelById(dashboard, 53)
	if slots == nil || refusals == nil {
		t.Fatal("the proxy storm row lacks the cap slots or the cap refusals panel")
	}
	if slots.Type != "timeseries" || slots.FieldConfig.Defaults.Unit != "short" {
		t.Errorf("cap slots panel is a %s in %q, want a timeseries in short", slots.Type, slots.FieldConfig.Defaults.Unit)
	}
	if slots.GridPos.Y != refusals.GridPos.Y || slots.GridPos.H != refusals.GridPos.H || slots.GridPos.X+slots.GridPos.W != refusals.GridPos.X {
		t.Errorf("cap slots panel %+v does not sit directly beside the cap refusals %+v", slots.GridPos, refusals.GridPos)
	}
	if len(slots.Targets) != 2 {
		t.Fatalf("cap slots panel has %d queries, want slots in use and capacity", len(slots.Targets))
	}
	inUse, capacity := slots.Targets[0], slots.Targets[1]
	if !strings.Contains(inUse.Expr, "max by (host, block) (max_over_time(urnetwork_hosted_contract_admission_in_use{") || !strings.Contains(inUse.Expr, "}[2m]))") {
		t.Errorf("cap slots in use are not each block's busiest process over two minutes: %s", inUse.Expr)
	}
	if !strings.Contains(capacity.Expr, "max by (host, block) (urnetwork_hosted_contract_admission_capacity{") {
		t.Errorf("cap capacity is not read per block on each host: %s", capacity.Expr)
	}
	if !strings.HasSuffix(inUse.LegendFormat, " in use") || !strings.HasSuffix(capacity.LegendFormat, " capacity") {
		t.Errorf("cap slots legends %q and %q do not tell use from capacity", inUse.LegendFormat, capacity.LegendFormat)
	}
	// the capacity lines are dashed references, kept out of the legend so the
	// legend ranks the processes by their slots in use
	dashed, unlisted := false, false
	for _, override := range slots.FieldConfig.Overrides {
		for _, property := range override.Properties {
			if override.Matcher.Options != ".* capacity" {
				continue
			}
			if property.Id == "custom.lineStyle" && strings.Contains(string(property.Value), `"dash"`) {
				dashed = true
			}
			if property.Id == "custom.hideFrom" && strings.Contains(string(property.Value), `"legend": true`) {
				unlisted = true
			}
		}
	}
	if !dashed || !unlisted {
		t.Errorf("the capacity lines are dashed %t and kept out of the legend %t, want both", dashed, unlisted)
	}
	for _, part := range []string{"hosted_contract_concurrency", "256 by default", "headroom"} {
		if !strings.Contains(slots.Description, part) {
			t.Errorf("cap slots panel does not say %q", part)
		}
	}
	for _, id := range []int{47, 49, 53} {
		if panel := dashboardPanelById(dashboard, id); panel == nil || !strings.Contains(panel.Description, "proxy.yml hosted_contract_concurrency") {
			t.Errorf("proxy storm panel %d does not name the configured cap", id)
		}
	}
}
