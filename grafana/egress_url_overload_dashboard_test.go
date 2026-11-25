package grafana

import (
	"strings"
	"testing"
)

// The overload windows (server model/provider_egress_url_overload.go,
// taskworker provider_egress_url_overload_work.go): the discounted outcome
// counter comes from whichever taskworker ran the judge, so its panel sums
// every taskworker of the env per outcome; the run timestamp is the newest over
// the taskworkers, so a stopped judge shows as a time that keeps growing.
func TestEgressProbeDashboardChartsTheOverloadWindows(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const rowTitle = "overload windows: URL outcomes measured while the platform was overloaded, recorded but not scored"

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
		expr  string
		words []string
	}{
		{"URL outcomes discounted as an overload / min", "short",
			`sum by (outcome) (rate(urnetwork_egress_url_discounted_outcomes_total{env="$env",service="taskworker",instance!=""}[$__rate_interval])) * 60`,
			[]string{"whole fleet", "worst country", "worst network", "recorded", "rolling quota", "Quality", "one country", "once"}},
		{"overload judge: since the last run", "s",
			`time() - max(urnetwork_egress_url_overload_judge_last_run_timestamp_seconds{env="$env",service="taskworker"} > 0)`,
			[]string{"once a minute", "discounts nothing", "judge_provider_egress_url_overload"}},
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
		if len(panel.Targets) != 1 || panel.Targets[0].Expr != c.expr {
			t.Errorf("panel %q reads %+v, want %s", c.title, panel.Targets, c.expr)
		}
		for _, word := range c.words {
			if !strings.Contains(panel.Description, word) {
				t.Errorf("panel %q does not say %q", c.title, word)
			}
		}
	}
}
