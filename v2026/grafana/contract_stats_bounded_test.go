// Dashboard reads must not turn a cap, stale value or historical last-not-null
// point into a current open-set total.
package grafana

import (
	"strings"
	"testing"
)

// All panels require same-process authority; only the labeled second series
// may show a capped lower bound, and current stat panels are instant queries.
func TestProvidersOpenContractCountsRequireFreshExactAuthority(t *testing.T) {
	dashboard := readTestDashboard(t, "providers.json")
	for _, id := range []int{15, 16, 19, 20, 23, 24} {
		panel := dashboardPanelById(dashboard, id)
		if panel == nil || len(panel.Targets) != 2 {
			t.Fatalf("panel %d lacks exact and lower-bound series", id)
		}
		for i, target := range panel.Targets {
			for _, part := range []string{"urnetwork_stats_contract_open_status", "urnetwork_stats_contract_open_observed_at_seconds", "on (env,host,block,instance)", "< 600", ">= -30"} {
				if !strings.Contains(target.Expr, part) {
					t.Errorf("panel %d series %d lacks %s", id, i, part)
				}
			}
			if i == 0 && (!strings.Contains(target.Expr, `status="exact"`) || strings.Contains(target.Expr, "urnetwork_stats_contract_open_lower_bound")) {
				t.Errorf("panel %d exact series accepts capped data", id)
			}
			if i == 1 && (!strings.Contains(target.Expr, `status="capped"`) || !strings.Contains(target.Expr, "urnetwork_stats_contract_open_lower_bound") || !strings.HasPrefix(target.LegendFormat, "at least ")) {
				t.Errorf("panel %d lower-bound series hides its status", id)
			}
			if panel.Type == "stat" && (!target.Instant || target.Range == nil || *target.Range) {
				t.Errorf("panel %d can revive a historical last-not-null value", id)
			}
		}
	}
	share := dashboardPanelById(dashboard, 28)
	if share == nil || len(share.Targets) != 1 {
		t.Fatal("missing exact share panel")
	}
	target := share.Targets[0]
	if strings.Count(target.Expr, `status="exact"`) != 2 || strings.Contains(target.Expr, "lower_bound") || !target.Instant || target.Range == nil || *target.Range {
		t.Fatal("share must use two fresh exact counts at the current instant")
	}
}
