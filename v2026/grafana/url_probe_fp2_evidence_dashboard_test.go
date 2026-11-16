package grafana

import (
	"strings"
	"testing"
)

// The capability evidence the URL ingest stores for capability-aware
// FindProviders2 (FP2-PROTOCOL.md §8.1): the failed later legs each failed
// outcome's mask names, and the post-quantum outcomes of the turns that
// opened an end-to-end session, in one row under the IP family panels.
func TestEgressProbeDashboardChartsCapabilityEvidence(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const rowTitle = "capability evidence: failed later legs and post-quantum outcomes"

	rowY := -1
	for _, panel := range dashboard.Panels {
		if panel.Type == "row" && panel.Title == rowTitle {
			rowY = panel.GridPos.Y
		}
	}
	if rowY < 0 {
		t.Fatalf("missing the row %q", rowTitle)
	}
	for _, c := range []struct {
		title string
		unit  string
		exprs []string
		words []string
	}{
		{title: "URL outcomes failed only on later legs, by leg / min", unit: "short", exprs: []string{
			`sum by (legs) (rate(urnetwork_egress_url_failed_later_legs_total{env="$env",instance!=""}[$__rate_interval])) * 60`,
		}, words: []string{"v4udp", "v6tcp", "v6udp", "v4udp_v6tcp", "v4udp_v6udp", "failed_later_legs", "place"}},
		{title: "post-quantum outcomes of URL turns / min", unit: "short", exprs: []string{
			`sum by (pq) (rate(urnetwork_egress_url_pq_outcomes_total{env="$env",instance!=""}[$__rate_interval])) * 60`,
			`sum by (pq) (rate(urnetwork_url_probe_e2e_outcomes_total{env="$env",service="taskworker",instance!=""}[$__rate_interval])) * 60`,
		}, words: []string{"positive", "negative", "unmeasured", "pq_outcome", "X25519MLKEM768", "eight-hour window", "prober"}},
	} {
		panels := []testPanel{}
		for _, panel := range dashboard.Panels {
			if panel.Title == c.title {
				panels = append(panels, panel)
			}
		}
		if len(panels) != 1 {
			t.Fatalf("%d panels titled %q, want 1", len(panels), c.title)
		}
		panel := panels[0]
		if panel.GridPos.Y != rowY+1 {
			t.Errorf("panel %q sits at y=%d, want %d", c.title, panel.GridPos.Y, rowY+1)
		}
		if panel.FieldConfig.Defaults.Unit != c.unit {
			t.Errorf("panel %q shows %q, want %q", c.title, panel.FieldConfig.Defaults.Unit, c.unit)
		}
		if len(panel.Targets) < len(c.exprs) {
			t.Fatalf("panel %q reads %+v", c.title, panel.Targets)
		}
		for i, expr := range c.exprs {
			if panel.Targets[i].Expr != expr {
				t.Errorf("panel %q target %d reads %s, want %s", c.title, i, panel.Targets[i].Expr, expr)
			}
		}
		for _, word := range c.words {
			if !strings.Contains(panel.Description, word) {
				t.Errorf("panel %q does not say what %q means", c.title, word)
			}
		}
	}
}
