// The key history retention's taskworker panels: the rows it deleted by
// table, its runs by why they ended, and the rows due at the newest probe,
// which the RetentionStalled alert reads with the deletes.
package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The retention's row and panel ids on the taskworker dashboard.
const (
	clientKeyHistoryRetentionRowPanel     = 152
	clientKeyHistoryRetentionDeletedPanel = 153
	clientKeyHistoryRetentionRunsPanel    = 154
	clientKeyHistoryRetentionDuePanel     = 155
)

// Deleted rows read as a rate by the retention's three tables, runs as a rate
// a minute by stop, and the due rows from the taskworker that probed last,
// all under the taskworker filters. The panels explain the row bound, the
// brake, the failing stop, the pass's start over and the alert.
func TestClientKeyHistoryRetentionDashboardPanels(t *testing.T) {
	dashboard := readTestDashboard(t, "taskworker.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("key history retention diagnostics must remain authenticated")
	}
	row := dashboardPanelById(dashboard, clientKeyHistoryRetentionRowPanel)
	deleted := dashboardPanelById(dashboard, clientKeyHistoryRetentionDeletedPanel)
	runs := dashboardPanelById(dashboard, clientKeyHistoryRetentionRunsPanel)
	due := dashboardPanelById(dashboard, clientKeyHistoryRetentionDuePanel)
	if row == nil || deleted == nil || runs == nil || due == nil {
		t.Fatal("the key history retention row or panels are missing")
	}
	if row.Type != "row" || !strings.Contains(row.Title, "model/st_client_key_history_retention.go") {
		t.Errorf("key history retention row = %q %q", row.Type, row.Title)
	}
	for _, panel := range []*testPanel{deleted, runs, due} {
		if panel.GridPos.Y != row.GridPos.Y+row.GridPos.H {
			t.Errorf("panel %d is not under its row", panel.Id)
		}
	}

	if deleted.Type != "timeseries" || len(deleted.Targets) != 1 ||
		deleted.Targets[0].Expr != `sum by (table) (rate(urnetwork_retention_deleted_rows_total{`+contractHorizonTaskworkerFilter+`,table=~"st_client_key_history|st_client_key_head|st_client_key_gone"}[$__rate_interval]))` ||
		deleted.Targets[0].LegendFormat != "{{table}}" {
		t.Errorf("deleted rows panel lost its rate by table: %+v", deleted.Targets)
	}
	for _, part := range []string{"1,000 registrations", "DeleteBatchRowCount", "{client_key_history_retention}control", "maxClientsPerSecond", "whichever is more", "statement bound"} {
		if !strings.Contains(deleted.Description, part) {
			t.Errorf("deleted rows panel does not explain %s", part)
		}
	}

	if runs.Type != "timeseries" || len(runs.Targets) != 1 ||
		runs.Targets[0].Expr != "60 * sum by (stop) (rate(urnetwork_client_key_history_retention_runs_total{"+contractHorizonTaskworkerFilter+"}[$__rate_interval]))" ||
		runs.Targets[0].LegendFormat != "{{stop}}" {
		t.Errorf("runs panel lost its rate by stop: %+v", runs.Targets)
	}
	for _, part := range []string{"due", "budget", "brake_off", "brake_unread", "canceled", "batch_bound", "the run fails", "starts over at the oldest record"} {
		if !strings.Contains(runs.Description, part) {
			t.Errorf("runs panel does not explain %s", part)
		}
	}

	if due.Type != "timeseries" || len(due.Targets) != 2 ||
		due.Targets[0].Expr != "max by (table) (urnetwork_retention_due_rows{"+contractHorizonTaskworkerFilter+"} and on (env, service, block, host, instance, table) topk by (table) (1, urnetwork_retention_due_rows_timestamp_seconds{"+contractHorizonTaskworkerFilter+"}))" ||
		due.Targets[1].Expr != "time() - max by (table) (urnetwork_retention_due_rows_timestamp_seconds{"+contractHorizonTaskworkerFilter+"})" {
		t.Errorf("due panel lost the newest probe's count or its age: %+v", due.Targets)
	}
	for _, part := range []string{"1,000,000", "RetentionStalled", "retention-stall.yml", "6 hours"} {
		if !strings.Contains(due.Description, part) {
			t.Errorf("due panel does not explain %s", part)
		}
	}
}
