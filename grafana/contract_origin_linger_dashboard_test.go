// The lingering origins' side table panels on the taskworker dashboard
// (model/contract_origin_linger.go): the companion origin reads by service
// and statements, the sweep's deletes and runs, and its lag.
package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The side table's row and panel ids on the taskworker dashboard.
const (
	contractOriginLingerRowPanel   = 131
	contractOriginLingerReadsPanel = 132
	contractOriginLingerSweepPanel = 133
	contractOriginLingerLagPanel   = 134
)

// The reads panel reads every serving process, not only the taskworker, by
// service and path; the sweep panels read the taskworker's deletes of its
// table, its runs by stop and its lag. Each panel explains what it reads.
func TestContractOriginLingerDashboardPanels(t *testing.T) {
	dashboard := readTestDashboard(t, "taskworker.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("side table diagnostics must remain authenticated")
	}
	row := dashboardPanelById(dashboard, contractOriginLingerRowPanel)
	reads := dashboardPanelById(dashboard, contractOriginLingerReadsPanel)
	sweep := dashboardPanelById(dashboard, contractOriginLingerSweepPanel)
	lag := dashboardPanelById(dashboard, contractOriginLingerLagPanel)
	if row == nil || reads == nil || sweep == nil || lag == nil {
		t.Fatal("the side table row or panels are missing")
	}
	if row.Type != "row" || !strings.Contains(row.Title, "model/contract_origin_linger.go") {
		t.Errorf("side table row = %q %q", row.Type, row.Title)
	}
	for _, panel := range []*testPanel{reads, sweep, lag} {
		if panel.GridPos.Y != row.GridPos.Y+row.GridPos.H {
			t.Errorf("panel %d is not directly under its row", panel.Id)
		}
	}

	if reads.Type != "timeseries" || len(reads.Targets) != 1 ||
		reads.Targets[0].Expr != `sum by (service, path) (rate(urnetwork_contract_origin_linger_reads_total{env="$env",instance!=""}[$__rate_interval]))` ||
		reads.Targets[0].LegendFormat != "{{service}} {{path}}" {
		t.Errorf("reads panel lost its rate by service and path: %+v", reads.Targets)
	}
	for _, part := range []string{"side_table", "legacy", "75 minutes", "transfer_contract_open_source_id"} {
		if !strings.Contains(reads.Description, part) {
			t.Errorf("reads panel does not explain %s", part)
		}
	}

	if sweep.Type != "timeseries" || len(sweep.Targets) != 2 ||
		sweep.Targets[0].Expr != `sum(rate(urnetwork_retention_deleted_rows_total{`+contractHorizonTaskworkerFilter+`,table="contract_origin_linger"}[$__rate_interval]))` ||
		sweep.Targets[1].Expr != `60 * sum by (stop) (rate(urnetwork_contract_origin_linger_sweep_runs_total{`+contractHorizonTaskworkerFilter+`}[$__rate_interval]))` {
		t.Errorf("sweep panel lost its deletes or its runs by stop: %+v", sweep.Targets)
	}
	for _, stop := range []string{"caught_up", "batches", "budget", "canceled", "error"} {
		if !strings.Contains(sweep.Description, stop) {
			t.Errorf("sweep panel does not explain %s", stop)
		}
	}

	if lag.Type != "timeseries" || len(lag.Targets) != 1 ||
		lag.Targets[0].Expr != `max(urnetwork_contract_origin_linger_sweep_lag_seconds{`+contractHorizonTaskworkerFilter+`})` ||
		lag.FieldConfig.Defaults.Unit != "s" {
		t.Errorf("lag panel lost its maximum in seconds: %+v", lag.Targets)
	}
}
