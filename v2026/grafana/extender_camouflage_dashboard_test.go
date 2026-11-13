package grafana

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The camouflage report's dashboard (controller/extender_camouflage_report_metrics.go;
// connect/EXTENDER-SPLICE-ROLLOUT.md). It answers the numeric gates of a splice
// rollout, so each is pinned to a panel, it stays internal, since country by outcome
// says where the fleet is, and every query reads a counter through increase(), so a
// restart does not read as a jump.
func TestExtenderCamouflageDashboardAnswersTheRolloutGates(t *testing.T) {
	dashboard := readTestDashboard(t, "extender-camouflage.json")
	if dashboard.Uid != "urnetwork-extender-camouflage" {
		t.Errorf("uid = %q", dashboard.Uid)
	}
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Errorf("the dashboard is public")
	}
	expressions := dashboardExpressions(dashboard)
	for question, metric := range map[string]string{
		"how reports ended":                    "urnetwork_extender_camouflage_reports_total",
		"what clients' dials came to":          "urnetwork_extender_camouflage_client_outcomes_total",
		"clients by version":                   "urnetwork_extender_camouflage_client_version_outcomes_total",
		"how extenders classified connections": "urnetwork_extender_camouflage_extender_outcomes_total",
		"extenders by version":                 "urnetwork_extender_camouflage_extender_version_outcomes_total",
		"coverage verdicts":                    "urnetwork_extender_camouflage_status_reports_total",
		"verdicts by version":                  "urnetwork_extender_camouflage_status_version_reports_total",
		// gate 7: the fleet's load on one borrowed site
		"splices per borrowed site": "urnetwork_extender_camouflage_target_splices_total",
		"bytes per borrowed site":   "urnetwork_extender_camouflage_target_relayed_bytes_total",
		// gate 3's floors and ceilings
		"compatible-client success": `outcome="camouflage_won"`,
		"authenticated share":       `outcome="authenticated"`,
		"degraded ceilings":         "splice_fallback_terminated|splice_dial_failed|borrow_verify_failed",
		"coverage share":            `verdict="covered"`,
	} {
		if !slices.ContainsFunc(expressions, func(expression string) bool {
			return strings.Contains(expression, metric)
		}) {
			t.Errorf("no panel answers %q (expected %s)", question, metric)
		}
	}

	aggregated := regexp.MustCompile(`\b(sum|topk)\s*(by\s*\([^)]*\)\s*)?\(`)
	identifying := regexp.MustCompile(`client_id|network_id|user_id|public_key|\bip\b|address`)
	for _, expression := range expressions {
		if !strings.Contains(expression, `env="$env"`) {
			t.Errorf("query is not scoped to $env: %q", expression)
		}
		if !aggregated.MatchString(expression) {
			t.Errorf("query does not aggregate across instances: %q", expression)
		}
		if !strings.Contains(expression, "increase(") {
			t.Errorf("a counter is read raw: %q", expression)
		}
		if identifying.MatchString(expression) {
			t.Errorf("query names an identifying label: %q", expression)
		}
	}
	for _, panel := range dashboard.Panels {
		if panel.Type != "row" && panel.Description == "" {
			t.Errorf("panel %q says nothing of how to read it", panel.Title)
		}
	}
}
