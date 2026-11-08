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
// outcomes over the same hour. Beside them, the country sites' graduation
// (taskworker egress_country_site_refresh.go): the state gauge is set by the
// taskworker that ran the latest daily refresh, so it is read from that one
// process, as the pool gauges are; the change counter is summed across the
// taskworkers over a day.
func TestEgressProbeDashboardChartsUrlOutcomesByCatalogSource(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const metric = "urnetwork_egress_url_outcomes_total"
	const selector = metric + `{env="$env",instance!=""`
	const rowTitle = "URL outcomes by catalog source, and country sites graduating on paired evidence"
	const latestRefreshSelection = `and on(env,service,block,host,instance) topk(1, max by (env,service,block,host,instance) (urnetwork_egress_site_refresh_last_run_timestamp_seconds{env="$env"}))`

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
		line  int
		words []string
	}{
		{"URL outcomes by catalog source / min", "short",
			"sum by (source, scoring, outcome) (rate(" + selector + "}[$__rate_interval])) * 60",
			rowY + 1, []string{"general", "country", "probation", "scored"}},
		{"URL success share by catalog source (1h)", "percentunit",
			"sum by (source, scoring) (increase(" + selector + `,outcome="success"}[1h])) / sum by (source, scoring) (increase(` + selector + "}[1h]))",
			rowY + 1, []string{"general", "country", "probation", "scored"}},
		{"country sites by state (latest refresh)", "short",
			`max by (state) (urnetwork_egress_country_sites{env="$env"} ` + latestRefreshSelection + `)`,
			rowY + 9, []string{"probation", "scored", "demoted", "network", "Online"}},
		{"country site graduations and demotions / day", "short",
			`sum by (change, reason) (increase(urnetwork_egress_country_site_changes_total{env="$env"}[1d]))`,
			rowY + 9, []string{"paired", "worse", "collapsed", "tls", "prober fault"}},
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
		if panel.GridPos.Y != c.line {
			t.Errorf("panel %q sits at y=%d, want %d", c.title, panel.GridPos.Y, c.line)
		}
		if panel.FieldConfig.Defaults.Unit != c.unit {
			t.Errorf("panel %q shows %q, want %q", c.title, panel.FieldConfig.Defaults.Unit, c.unit)
		}
		if len(panel.Targets) != 1 || panel.Targets[0].Expr != c.expr {
			t.Errorf("panel %q reads %+v, want %s", c.title, panel.Targets, c.expr)
		}
		for _, word := range c.words {
			if !strings.Contains(panel.Description, word) {
				t.Errorf("panel %q does not say what %q means", c.title, word)
			}
		}
	}
}
