package grafana

import (
	"strings"
	"testing"
)

// The URL outcomes by catalog source (D3, controller
// provider_egress_url_source.go): the counter comes from whichever process ran
// the operator ingest, api and alt or a taskworker with the in-process
// operator, so both panels read every process of the env, per source and
// scoring, and the share sets each source's successes against its own
// outcomes over the same hour.
func TestEgressProbeDashboardChartsUrlOutcomesByCatalogSource(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const metric = "urnetwork_egress_url_outcomes_total"
	const selector = metric + `{env="$env",instance!=""`
	const rowTitle = "URL outcomes by catalog source: country lists and sites on probation, recorded unscored"

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
	}{
		{"URL outcomes by catalog source / min", "short",
			"sum by (source, scoring, outcome) (rate(" + selector + "}[$__rate_interval])) * 60"},
		{"URL success share by catalog source (1h)", "percentunit",
			"sum by (source, scoring) (increase(" + selector + `,outcome="success"}[1h])) / sum by (source, scoring) (increase(` + selector + "}[1h]))"},
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
			t.Errorf("panel %q sits at y=%d, want the row's first line %d", c.title, panel.GridPos.Y, rowY+1)
		}
		if panel.FieldConfig.Defaults.Unit != c.unit {
			t.Errorf("panel %q shows %q, want %q", c.title, panel.FieldConfig.Defaults.Unit, c.unit)
		}
		if len(panel.Targets) != 1 || panel.Targets[0].Expr != c.expr {
			t.Errorf("panel %q reads %+v, want %s", c.title, panel.Targets, c.expr)
		}
		for _, word := range []string{"general", "country", "probation", "scored"} {
			if !strings.Contains(panel.Description, word) {
				t.Errorf("panel %q does not say what %q means", c.title, word)
			}
		}
	}
}
