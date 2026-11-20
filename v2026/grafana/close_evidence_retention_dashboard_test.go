// The close evidence row retention's taskworker panels: the rows it deleted,
// the retention each taskworker last applied and why its runs stopped, scoped
// to the taskworker, after the due-contract delete pass.
package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The row retention's row and panel ids on the taskworker dashboard.
const (
	closeEvidenceRetentionRowPanel       = 87
	closeEvidenceRetentionDeletedPanel   = 88
	closeEvidenceRetentionRetentionPanel = 89
	closeEvidenceRetentionStopsPanel     = 90
)

// Deleted rows read as a rate, the applied retention as each taskworker's last
// value in days, and stops as the trailing hour's increase by reason, all
// under the taskworker filters. The panels explain the brake, how the
// retention is bounded and every stop, and the row follows the due-contract
// delete pass.
func TestCloseEvidenceRetentionDashboardPanels(t *testing.T) {
	dashboard := readTestDashboard(t, "taskworker.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("close evidence retention diagnostics must remain authenticated")
	}
	row := dashboardPanelById(dashboard, closeEvidenceRetentionRowPanel)
	deleted := dashboardPanelById(dashboard, closeEvidenceRetentionDeletedPanel)
	retention := dashboardPanelById(dashboard, closeEvidenceRetentionRetentionPanel)
	stops := dashboardPanelById(dashboard, closeEvidenceRetentionStopsPanel)
	if row == nil || deleted == nil || retention == nil || stops == nil {
		t.Fatal("the close evidence retention row or panels are missing")
	}
	if row.Type != "row" || !strings.Contains(row.Title, "db_contract_close_evidence_retention.go") {
		t.Errorf("close evidence retention row = %q %q", row.Type, row.Title)
	}
	dueContractStops := dashboardPanelById(dashboard, dueContractReapStopsPanel)
	if dueContractStops == nil || row.GridPos.Y != dueContractStops.GridPos.Y+dueContractStops.GridPos.H {
		t.Error("the close evidence retention row does not follow the due-contract delete pass")
	}
	for _, panel := range []*testPanel{deleted, retention, stops} {
		if panel.GridPos.Y != row.GridPos.Y+row.GridPos.H {
			t.Errorf("panel %d is not under its row", panel.Id)
		}
	}

	if deleted.Type != "timeseries" || len(deleted.Targets) != 1 ||
		deleted.Targets[0].Expr != "sum(rate(urnetwork_close_evidence_retention_removed_rows_total{"+contractHorizonTaskworkerFilter+"}[$__rate_interval]))" {
		t.Errorf("deleted rows panel lost its rate: %+v", deleted.Targets)
	}
	for _, part := range []string{"{close_evidence_retention}control", "maxRowsPerSecond", "1,000", "5,000", "bringyourctl db close-evidence-retention", "ctid", "never reads the brake"} {
		if !strings.Contains(deleted.Description, part) {
			t.Errorf("deleted rows panel does not explain %s", part)
		}
	}

	if retention.Type != "timeseries" || len(retention.Targets) != 1 ||
		retention.Targets[0].Expr != "max by (instance) (urnetwork_close_evidence_retention_seconds{"+contractHorizonTaskworkerFilter+"}) / 86400" ||
		retention.Targets[0].LegendFormat != "{{instance}}" {
		t.Errorf("retention panel lost its days by taskworker: %+v", retention.Targets)
	}
	for _, part := range []string{"horizonDays", "35 days", "91", "dry run", "permanently"} {
		if !strings.Contains(retention.Description, part) {
			t.Errorf("retention panel does not explain %s", part)
		}
	}

	if stops.Type != "timeseries" || len(stops.Targets) != 1 ||
		stops.Targets[0].Expr != "sum by (stop) (increase(urnetwork_close_evidence_retention_stops_total{"+contractHorizonTaskworkerFilter+"}[1h]))" ||
		stops.Targets[0].LegendFormat != "{{stop}}" {
		t.Errorf("stops panel lost its trailing-hour increase by reason: %+v", stops.Targets)
	}
	for _, stop := range []string{"done", "budget", "brake_off", "brake_unread", "lock_timeout", "statement_timeout", "canceled", "error"} {
		if !strings.Contains(stops.Description, stop) {
			t.Errorf("stops panel does not explain %s", stop)
		}
	}
}
