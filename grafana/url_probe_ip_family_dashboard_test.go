package grafana

import (
	"strings"
	"testing"
)

// The URL probe's legs by IP family (qualityprobe/egresshealth url_family.go):
// every load's legs through the URL's A record, and a dual-stack provider's
// legs through its AAAA record, each counted by transport and outcome by the
// taskworker that ran the turn, so the share of providers that carry IPv4 and
// not IPv6 is read directly; beside them, the ingest's place verdicts of
// failed IPv6 legs and the turns the IPv6 common-mode guard held back.
func TestEgressProbeDashboardChartsUrlLegsByIpFamily(t *testing.T) {
	dashboard := readTestDashboard(t, "egress-probes.json")
	const selector = `{env="$env",service="taskworker",instance!=""`
	const metric = "urnetwork_url_probe_transport_outcomes_total"
	const rowTitle = "URL legs by IP family: IPv4 through A records, IPv6 through AAAA"

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
		{"URL legs by IP family, transport and outcome / min", "short", []string{
			"sum by (family, transport, outcome) (rate(" + metric + selector + "}[$__rate_interval])) * 60",
			"sum by (transport) (rate(urnetwork_url_probe_ipv6_common_mode_held_total" + selector + "}[$__rate_interval])) * 60",
			`sum by (transport, verdict) (rate(urnetwork_egress_url_ipv6_verdicts_total{env="$env",instance!=""}[$__rate_interval])) * 60`,
		}, []string{"ipv4", "ipv6", "AAAA", "not_measured", "no_url", "country_disabled", "skipped", "verdict", "provider", "place", "held"}},
		{"failed IPv6 legs after a passed IPv4 leg (1h)", "percentunit", []string{
			"sum(increase(" + metric + selector + `,transport="tcp",family="ipv6",outcome="error"}[1h])) / sum(increase(` + metric + selector + `,transport="tcp",family="ipv6",outcome=~"success|error"}[1h]))`,
			"sum(increase(" + metric + selector + `,transport="udp",family="ipv6",outcome="silent"}[1h])) / sum(increase(` + metric + selector + `,transport="udp",family="ipv6",outcome=~"success|answered|silent"}[1h]))`,
		}, []string{"IPv4 but not over IPv6", "place judgement", "not_measured", "no_url"}},
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
