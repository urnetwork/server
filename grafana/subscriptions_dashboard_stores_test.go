package grafana

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// subscriptionsDashboardStores is the exporter's tracked store label set
// (model.SubscriptionMetricsStores) as a PromQL alternation.
func subscriptionsDashboardStores() string {
	return strings.Join(model.SubscriptionMetricsStores(), "|")
}

// Every store list on the subscriptions dashboard names exactly the stores the
// exporter labels, except the trial panel's, which names the stores that sell
// a trial. A market the exporter gains then fails here until the dashboard
// shows it: TAO, Bitcoin, ETH and gamma were missing from every by-store panel.
func TestSubscriptionsDashboardShowsEveryTrackedStore(t *testing.T) {
	stores := model.SubscriptionMetricsStores()
	for _, market := range []string{
		model.SubscriptionMarketBittensor,
		model.SubscriptionMarketBitcoin,
		model.SubscriptionMarketEthereum,
		model.SubscriptionMarketGamma,
	} {
		if !slices.Contains(stores, market) {
			t.Fatalf("exporter store labels %v omit %s", stores, market)
		}
	}

	dashboard := readTestDashboard(t, "subscriptions.json")
	storeList := regexp.MustCompile(`store=~"([^"]*)"`)
	lists := 0
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			for _, match := range storeList.FindAllStringSubmatch(target.Expr, -1) {
				// the deduplicated total and the reconciliation heartbeat
				// (all) are not stores
				named := slices.DeleteFunc(strings.Split(match[1], "|"), func(name string) bool {
					return name == model.SubscriptionMetricsStoreDeduplicated || name == model.PaymentReconcileStoreAll
				})
				if panel.Id == 23 {
					if want := []string{model.SubscriptionMarketApple, model.SubscriptionMarketGoogle, model.SubscriptionMarketStripe}; !slices.Equal(named, want) {
						t.Errorf("trial panel selects stores %v, want the stores that sell a trial %v", named, want)
					}
					continue
				}
				lists += 1
				if !slices.Equal(named, stores) {
					t.Errorf("panel %d %q selects stores %v, want the tracked stores %v", panel.Id, panel.Title, named, stores)
				}
			}
		}
	}
	// current, new, engaged and churned subscribers by store, and the three
	// reconciliation panels (terminal events and repairs gate on each
	// store's source timestamp too)
	if lists != 9 {
		t.Errorf("subscriptions dashboard has %d tracked-store lists, want 9", lists)
	}

	documentBytes, err := dashboardsFs.ReadFile("dashboards/subscriptions.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"TAO (`bittensor`), Bitcoin, ETH (`ethereum`) and Pay with Gamma (`gamma`)",
		"Stripe, Solana, Monero, Zcash, TAO, Bitcoin, ETH and gamma",
		"Gamma has no reconciliation leg",
		"The crypto markets (Solana, Monero, Zcash, TAO, Bitcoin, ETH) and gamma sell no trial",
	} {
		if !strings.Contains(string(documentBytes), required) {
			t.Errorf("subscriptions dashboard does not say %q", required)
		}
	}
}
