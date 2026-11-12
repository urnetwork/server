package grafana

import (
	"strings"
	"testing"
)

// The URL probe's two legs (qualityprobe/egresshealth url_h3.go): the HTTPS
// leg over TCP and, for a URL that declares h3, the HTTP/3 leg over UDP, each
// counted apart by the taskworker that ran the turn, so the share of
// providers that carry TCP and not UDP is read directly; beside them, the
// turns the HTTP/3 common-mode guard held back, and by country the HTTP/3
// legs a country's udp_probes: false turned off.
func TestEgressProbeDashboardChartsUrlLegsByTransport(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const selector = `{env="$env",service="taskworker",instance!=""`
	const metric = "urnetwork_url_probe_transport_outcomes_total"
	const rowTitle = "URL legs by transport: HTTPS over TCP, HTTP/3 over UDP"

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
		{"URL legs by transport and outcome / min", "short", []string{
			"sum by (transport, outcome) (rate(" + metric + selector + "}[$__rate_interval])) * 60",
			"sum(rate(urnetwork_url_probe_h3_common_mode_held_total" + selector + "}[$__rate_interval])) * 60",
			`sum by (verdict) (rate(urnetwork_egress_url_h3_verdicts_total{env="$env",instance!=""}[$__rate_interval])) * 60`,
			// the HTTP/3 legs a country's udp_probes: false turned off, by country
			"sum by (country) (rate(urnetwork_url_probe_udp_country_disabled_total" + selector + "}[$__rate_interval])) * 60",
		}, []string{"tcp", "udp", "h3", "answered", "silent", "not_measured", "skipped", "verdict", "provider", "place", "held"}},
		{"silent HTTP/3 legs after a passed HTTPS leg (1h)", "percentunit", []string{
			"sum(increase(" + metric + selector + `,transport="udp",outcome="silent"}[1h])) / sum(increase(` + metric + selector + `,transport="udp",outcome=~"success|answered|silent"}[1h]))`,
		}, []string{"TCP but not UDP", "place judgement", "unmeasured", "skipped"}},
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
		if len(panel.Targets) != len(c.exprs) {
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
