package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The core signals of the 2026-10-26 incidents, with the SIGNALS.md section 26
// thresholds: services-tier CPU saturation that queued the database behind
// PgBouncer, and the proxy contract-compensation storm. Neither had a panel,
// so both were found hours late.
type incidentSignalPanel struct {
	dashboard  string
	id         int
	panelType  string
	unit       string
	selectors  []string
	thresholds []float64
}

func TestIncidentSignalsCarryTheirThresholds(t *testing.T) {
	const pgbouncerWaiting = `sum(avg_over_time(pgbouncer_pool_clients_waiting{env="$env",job="pgbouncer"}[5m]))`
	const acquireWait = `urnetwork_pg_pool_acquire_duration_seconds_total{env="$env",pool="default",instance!=""}`
	const mainThread = `redis_cpu_sys_main_thread_seconds_total{`
	const hello = `redis_commands_total{`
	const proxy = `service="proxy",block=~"$block",host=~"$host",instance!=""`
	signals := []incidentSignalPanel{
		{"services-overview.json", 17, "stat", "short", []string{pgbouncerWaiting}, []float64{100, 1000}},
		{"services-overview.json", 18, "stat", "s", []string{acquireWait, `outcome="acquired"`, "[5m]"}, []float64{0.1}},
		{"services-overview.json", 19, "stat", "percentunit", []string{`node_cpu_seconds_total{env="$env",job="node",mode="idle"}[5m]`, `go_goroutines{env="$env",service="connect",instance!=""}`}, []float64{0.85}},
		{"services-overview.json", 20, "stat", "percentunit", []string{`node_cpu_seconds_total{env="$env",job="node",mode="idle"}[5m]`, `go_goroutines{env="$env",service="proxy",instance!=""}`}, []float64{0.85}},
		{"services-overview.json", 21, "stat", "short", []string{`urnetwork_contract_creation_stage_inflight{env="$env",stage="other",instance!=""}`, "sum by (service, host, block, instance)"}, []float64{1000}},
		{"services-overview.json", 22, "stat", "percentunit", []string{`outcome="canceled"`, "sum by (service)", "> 1)"}, []float64{0.2}},
		{"services-overview.json", 23, "stat", "ops", []string{`operation="release-pending",result="error"`, "[5m]"}, []float64{10}},
		{"services-overview.json", 24, "stat", "ops", []string{`urnetwork_redis_pool_timeouts_total{env="$env",instance!=""}[5m]`, "sum by (service)"}, []float64{10}},
		{"services-overview.json", 25, "stat", "percentunit", []string{mainThread, "redis_cpu_user_main_thread_seconds_total{", "[5m]"}, []float64{0.85}},
		{"services-overview.json", 26, "stat", "ops", []string{hello, `cmd="hello"`, "[5m]"}, []float64{500}},
		{"postgres.json", 17, "stat", "short", []string{pgbouncerWaiting}, []float64{100, 1000}},
		{"postgres.json", 18, "stat", "s", []string{"pgbouncer_pool_maxwait_seconds"}, []float64{3}},
		{"postgres.json", 19, "stat", "s", []string{acquireWait, "[5m]"}, []float64{0.1}},
		{"postgres.json", 21, "timeseries", "short", []string{pgbouncerWaiting}, []float64{100, 1000}},
		{"postgres.json", 23, "timeseries", "s", []string{"pgbouncer_pool_maxwait_seconds"}, []float64{3}},
		{"postgres.json", 27, "timeseries", "s", []string{acquireWait}, []float64{0.1}},
		{"redis-cluster.json", 20, "stat", "percentunit", []string{mainThread, "[5m]"}, []float64{0.85}},
		{"redis-cluster.json", 21, "stat", "ops", []string{hello, `cmd="hello"`, "[5m]"}, []float64{500}},
		{"redis-cluster.json", 22, "stat", "ops", []string{"urnetwork_redis_pool_timeouts_total", "sum by (service)"}, []float64{10}},
		{"redis-cluster.json", 25, "timeseries", "percentunit", []string{mainThread, `node=~"$node"`, "[5m]"}, []float64{0.85}},
		{"redis-cluster.json", 26, "timeseries", "ops", []string{hello, `node=~"$node"`, `cmd="hello"`, "[5m]"}, []float64{500}},
		{"redis-cluster.json", 16, "timeseries", "ops", []string{"urnetwork_redis_pool_timeouts_total"}, []float64{10}},
		{"proxy.json", 42, "stat", "short", []string{"urnetwork_contract_creation_stage_inflight{", proxy + `,stage="other"`}, []float64{1000}},
		{"proxy.json", 43, "stat", "percentunit", []string{proxy + `,outcome="canceled"`}, []float64{0.2}},
		{"proxy.json", 44, "stat", "ops", []string{proxy + `,operation="release-pending",result="error"`}, []float64{10}},
		{"proxy.json", 45, "stat", "ops", []string{"urnetwork_redis_pool_timeouts_total{" + `env="$env",` + proxy}, []float64{10}},
		{"proxy.json", 46, "stat", "percentunit", []string{"node_cpu_seconds_total{", "go_goroutines{"}, []float64{0.85}},
		{"proxy.json", 49, "timeseries", "short", []string{proxy + `,stage="other"`, "max by (host, block)"}, []float64{1000}},
		{"proxy.json", 51, "timeseries", "percentunit", []string{proxy + `,outcome="canceled"`}, []float64{0.2}},
		{"proxy.json", 56, "timeseries", "ops", []string{"urnetwork_redis_pool_timeouts_total{", "urnetwork_redis_pool_misses_total{"}, []float64{10}},
		{"proxy.json", 57, "timeseries", "percentunit", []string{"node_cpu_seconds_total{", "go_goroutines{"}, []float64{0.85}},
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
		values := []float64{}
		for _, step := range panel.FieldConfig.Defaults.Thresholds.Steps[min(1, len(panel.FieldConfig.Defaults.Thresholds.Steps)):] {
			if step.Value != nil {
				values = append(values, *step.Value)
			}
		}
		if !slices.Equal(values, signal.thresholds) {
			t.Errorf("%s thresholds are %v, want the SIGNALS.md section 26 levels %v", where, values, signal.thresholds)
		}
		if signal.panelType == "timeseries" && panel.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode != "line" {
			t.Errorf("%s does not draw its thresholds as lines", where)
		}
		if !strings.Contains(panel.Description, "SIGNALS.md section 26") && !strings.Contains(panel.Description, "PgBouncerPoolQueueWaitHigh") {
			t.Errorf("%s does not name the source of its threshold", where)
		}
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
	if compensation == nil || len(compensation.Targets) != 1 ||
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
}
