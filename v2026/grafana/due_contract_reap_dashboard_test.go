// The due-contract delete pass's taskworker panels: deletions and the pass's
// stops, scoped to the taskworker, beside the contract horizon reaper's.
package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The delete pass's row and panel ids on the taskworker dashboard.
const (
	dueContractReapRowPanel     = 84
	dueContractReapDeletedPanel = 85
	dueContractReapStopsPanel   = 86
)

// Deletions read as a rate and stops as the trailing hour's increase by stop
// reason, both under the taskworker filters. The stops panel explains every
// reason the pass can stop for, and the row follows the reaper's panels.
func TestDueContractReapDashboardPanels(t *testing.T) {
	dashboard := readTestDashboard(t, "taskworker.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("delete pass diagnostics must remain authenticated")
	}
	row := dashboardPanelById(dashboard, dueContractReapRowPanel)
	deleted := dashboardPanelById(dashboard, dueContractReapDeletedPanel)
	stops := dashboardPanelById(dashboard, dueContractReapStopsPanel)
	if row == nil || deleted == nil || stops == nil {
		t.Fatal("the delete pass row or panels are missing")
	}
	if row.Type != "row" || !strings.Contains(row.Title, "model/subscription_due_contract_reaper.go") {
		t.Errorf("delete pass row = %q %q", row.Type, row.Title)
	}
	reaperRuns := dashboardPanelById(dashboard, contractHorizonRunsPanel)
	if reaperRuns == nil || row.GridPos.Y != reaperRuns.GridPos.Y+reaperRuns.GridPos.H {
		t.Error("the delete pass row does not follow the contract horizon reaper's panels")
	}

	if deleted.Type != "timeseries" || len(deleted.Targets) != 1 ||
		deleted.Targets[0].Expr != "sum(rate(urnetwork_due_contract_reap_deleted_contracts_total{"+contractHorizonTaskworkerFilter+"}[$__rate_interval]))" {
		t.Errorf("deleted contracts panel lost its rate: %+v", deleted.Targets)
	}
	if stops.Type != "timeseries" || len(stops.Targets) != 1 ||
		stops.Targets[0].Expr != "sum by (stop) (increase(urnetwork_due_contract_reap_stops_total{"+contractHorizonTaskworkerFilter+"}[1h]))" ||
		stops.Targets[0].LegendFormat != "{{stop}}" {
		t.Errorf("stops panel lost its trailing-hour increase by reason: %+v", stops.Targets)
	}
	for _, stop := range []string{"drained", "budget", "busy", "lock_timeout", "statement_timeout"} {
		if !strings.Contains(stops.Description, stop) {
			t.Errorf("stops panel does not explain %s", stop)
		}
	}
}
