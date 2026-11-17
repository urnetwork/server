package grafana

import (
	"slices"
	"strings"
	"testing"
)

// The api dashboard's failed card checks (controller/subscription_stripe_card_failures.go,
// owner 2026-11-13): the fleet's failures by kind and decline class beside the
// bursts by owner kind, from the api and alt that receive the webhook, with no
// identity in the queries. The burst panel names the alert class it backs.
func TestApiDashboardShowsFailedCardChecksAndTheirBursts(t *testing.T) {
	dashboard := readTestDashboard(t, "api.json")
	if slices.Contains(dashboard.Tags, PublicTag) {
		t.Fatal("the api dashboard must remain authenticated")
	}
	failures := dashboardPanelById(dashboard, 66)
	bursts := dashboardPanelById(dashboard, 67)
	if failures == nil || bursts == nil {
		t.Fatal("the api dashboard has no failed card check panels")
	}
	if failures.Type != "timeseries" || len(failures.Targets) != 1 || failures.FieldConfig.Defaults.Unit != "ops" ||
		failures.Targets[0].Expr != `sum by (kind, reason_class) (rate(urnetwork_subscription_stripe_failures_total{env="$env",service=~"api|alt",instance!=""}[$__rate_interval]))` ||
		failures.Targets[0].LegendFormat != "{{kind}} {{reason_class}}" {
		t.Fatalf("failed card checks must be a per-second rate by kind and decline class from the api and alt: %+v", failures)
	}
	if bursts.Type != "timeseries" || len(bursts.Targets) != 1 ||
		bursts.Targets[0].Expr != `sum by (owner) (increase(urnetwork_subscription_stripe_failure_bursts_total{env="$env",service=~"api|alt",instance!=""}[$__rate_interval]))` ||
		bursts.Targets[0].LegendFormat != "{{owner}}" {
		t.Fatalf("card failure bursts must be counted by owner kind from the api and alt: %+v", bursts)
	}
	for _, want := range []string{"setup_intent.setup_failed", "charge.failed", "payment-card-failure-burst"} {
		if !strings.Contains(failures.Description, want) {
			t.Errorf("the failed card checks panel does not explain %q", want)
		}
	}
	if !strings.Contains(bursts.Description, "payment-card-failure-burst") {
		t.Error("the burst panel does not name the alert class it backs")
	}
	if failures.GridPos.Y != bursts.GridPos.Y || failures.GridPos.X+failures.GridPos.W != bursts.GridPos.X {
		t.Fatalf("the failures and their bursts do not share a row: %+v %+v", failures.GridPos, bursts.GridPos)
	}
	for _, panel := range []*testPanel{failures, bursts} {
		for _, forbidden := range []string{"network_id", "customer_id", "email", "invoice", "intent_id"} {
			if strings.Contains(panel.Targets[0].Expr, forbidden) {
				t.Errorf("panel %d query names %q", panel.Id, forbidden)
			}
		}
	}
}
