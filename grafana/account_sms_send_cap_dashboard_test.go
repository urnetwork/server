package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The api dashboard's account SMS send caps row (controller/aws_sms_send_cap.go,
// owner 2026-11-25 and 2026-11-26): the texts the send caps refused, per hour
// by service and cap, from every service that sends one. The description names
// each cap the refusal metric labels, the hourly and the daily ones, and the
// alert the panel backs, and the query names no number or account.
func TestApiDashboardShowsTheAccountSmsSendCapRefusals(t *testing.T) {
	dashboard := readTestDashboard(t, "api.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("the api dashboard must remain authenticated")
	}
	panel := dashboardPanelById(dashboard, 108)
	if panel == nil {
		t.Fatal("the api dashboard has no account SMS send cap panel")
	}
	if panel.Type != "timeseries" || len(panel.Targets) != 1 ||
		panel.Targets[0].Expr != `sum by (service, cap) (increase(urnetwork_account_message_sms_cap_refusals_total{env="$env",instance!=""}[1h]))` ||
		panel.Targets[0].LegendFormat != "{{service}} {{cap}}" {
		t.Fatalf("the send cap refusals must be counted per hour by service and cap: %+v", panel)
	}
	for _, want := range []string{
		"global, ", "country, ", "global_daily, ", "country_daily, ",
		"trailing hour", "trailing 24 hours", "country ZZ",
		"AccountSmsSendCapRefusals", "warp grafana/alerting/account-sms-send-cap.yml",
		"never the number",
	} {
		if !strings.Contains(panel.Description, want) {
			t.Errorf("the send cap panel does not explain %q", want)
		}
	}
	for _, forbidden := range []string{"phone", "number", "network_id", "user_auth"} {
		if strings.Contains(panel.Targets[0].Expr, forbidden) {
			t.Errorf("the send cap panel query names %q", forbidden)
		}
	}
}
