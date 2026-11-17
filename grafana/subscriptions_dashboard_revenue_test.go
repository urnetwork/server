package grafana

import (
	"strings"
	"testing"
)

// The expected dollars in (owner request 2026-11-16) sit at the top of the
// subscriptions dashboard: net new forecasted revenue and net recurring
// revenue, fixed to the snapshot's 30-day window rather than the rolling
// window selector, with the expected trial conversion revenue and the
// conversion rate in use beside the forecast. Every earlier panel sits below
// them, and the definitions text says what each number means.
func TestSubscriptionsDashboardShowsExpectedRevenueAtTheTop(t *testing.T) {
	dashboard := readTestDashboard(t, "subscriptions.json")

	row := dashboardPanelById(dashboard, 32)
	if row == nil || row.Type != "row" || row.GridPos.Y != 0 {
		t.Fatal("the revenue row is not the dashboard's first row")
	}
	revenueIds := map[int]bool{32: true}
	for _, c := range []struct {
		id       int
		title    string
		unit     string
		metric   string
		required []string
	}{
		{33, "net new forecasted revenue (30 days)", "currencyUSD", "urnetwork_subscription_new_forecast_revenue_usd{", []string{`store="deduplicated"`, `window="30d"`}},
		{34, "expected trial conversion revenue", "currencyUSD", "urnetwork_subscription_trial_expected_revenue_usd{", []string{`store="deduplicated"`}},
		{35, "trial conversion rate in use", "percentunit", "urnetwork_subscription_trial_conversion_rate{", []string{`store=~"apple|google|stripe"`, `store="all"`}},
		{36, "net recurring revenue (30 days)", "currencyUSD", "urnetwork_subscription_recurring_revenue_usd{", []string{`store="all"`, `window="30d"`}},
		// the revenue is its own step: why its panels can show no data
		{40, "revenue refresh failures", "short", "urnetwork_subscription_revenue_refresh_failures{", nil},
	} {
		panel := dashboardPanelById(dashboard, c.id)
		if panel == nil || panel.Type != "stat" || panel.Title != c.title {
			t.Errorf("panel %d is not the stat %q", c.id, c.title)
			continue
		}
		revenueIds[c.id] = true
		if panel.GridPos.Y != 1 || panel.GridPos.H < 5 {
			t.Errorf("%q sits at %+v, want the top of the dashboard under the revenue row, at least 5 high", c.title, panel.GridPos)
		}
		if panel.FieldConfig.Defaults.Unit != c.unit {
			t.Errorf("%q unit = %q, want %q", c.title, panel.FieldConfig.Defaults.Unit, c.unit)
		}
		expressions := ""
		for _, target := range panel.Targets {
			expressions += target.Expr + "\n"
		}
		for _, required := range append([]string{c.metric}, c.required...) {
			if !strings.Contains(expressions, required) {
				t.Errorf("%q does not select %s: %s", c.title, required, expressions)
			}
		}
		if strings.Contains(expressions, "$window") {
			t.Errorf("%q follows the rolling window selector: %s", c.title, expressions)
		}
	}

	for _, c := range []struct {
		id       int
		metric   string
		required []string
	}{
		{37, "urnetwork_subscription_new_forecast_revenue_usd{", []string{`store=~"` + subscriptionsDashboardStores() + `"`, `window="30d"`}},
		{38, "urnetwork_subscription_trials_convertible{", []string{`store=~"apple|google|stripe"`, "urnetwork_subscription_trial_conversion_trials{", `outcome=~"converted|lapsed"`}},
		{39, "urnetwork_subscription_recurring_revenue_usd{", []string{`store=~"` + subscriptionsDashboardStores() + `"`, `window="30d"`}},
	} {
		panel := dashboardPanelById(dashboard, c.id)
		if panel == nil {
			t.Errorf("revenue breakdown panel %d is missing", c.id)
			continue
		}
		revenueIds[c.id] = true
		expressions := ""
		for _, target := range panel.Targets {
			expressions += target.Expr + "\n"
		}
		for _, required := range append([]string{c.metric}, c.required...) {
			if !strings.Contains(expressions, required) {
				t.Errorf("%q does not select %s: %s", panel.Title, required, expressions)
			}
		}
	}

	// every earlier panel sits below the revenue section, unchanged otherwise
	revenueBottom := 0
	for _, panel := range dashboard.Panels {
		if revenueIds[panel.Id] {
			revenueBottom = max(revenueBottom, panel.GridPos.Y+panel.GridPos.H)
		}
	}
	for _, panel := range dashboard.Panels {
		if !revenueIds[panel.Id] && panel.GridPos.Y < revenueBottom {
			t.Errorf("panel %d %q sits at y %d, above the revenue section's bottom %d", panel.Id, panel.Title, panel.GridPos.Y, revenueBottom)
		}
	}
	if definitions := dashboardPanelById(dashboard, 1); definitions == nil || definitions.GridPos.Y != revenueBottom {
		t.Errorf("the definitions row does not follow the revenue section")
	}

	text := dashboardPanelById(dashboard, 2)
	if text == nil || text.Type != "text" {
		t.Fatal("the definitions text is missing")
	}
	for _, required := range []string{
		"**Net new forecasted revenue** is the yearly net value of the subscriptions that started in the last 30 days",
		"a monthly net x 12 and a yearly net x 1 by the plan",
		"**expected trial conversion revenue**",
		"**trial conversion rate**",
		"the share of the trials whose free period ended 7 to 90 days ago",
		"the forecast holds the expected trial conversion revenue whole, once",
		"**Net recurring revenue** is the net collected from renewals in the last 30 days",
		"A store's first paid window and trials never count",
		"every Google Play credit records the play.yml supporter price",
		"gift redemptions (no sale",
		"Stripe trials only the payment reconcile credited before the payment-sheet card check deployed",
		"a trial the reconcile credited after it (a missed webhook) counts",
		"**revenue refresh failures** counts the refreshes in a row it failed, and every other panel stays current",
	} {
		if !strings.Contains(text.Options.Content, required) {
			t.Errorf("the definitions text does not say %q", required)
		}
	}
	if strings.Contains(text.Options.Content, "No panel here shows revenue") {
		t.Error("the definitions text still says no panel shows revenue")
	}
}
