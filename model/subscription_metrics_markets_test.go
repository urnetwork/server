package model

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The payment markets added after the dashboard's first six stores. TAO,
// Bitcoin and ETH credit under their chain's market (crypto rail) and Pay with
// Gamma under gamma; each is a subscription the dashboard counts.
var subscriptionMetricsNewMarkets = []string{
	SubscriptionMarketBittensor,
	SubscriptionMarketBitcoin,
	SubscriptionMarketEthereum,
	SubscriptionMarketGamma,
}

// subscriptionMetricsInList returns the string literals of the one
// `<column> IN (...)` list in query.
func subscriptionMetricsInList(t *testing.T, query string, column string) []string {
	t.Helper()
	matches := regexp.MustCompile(regexp.QuoteMeta(column)+` IN \(([^)]*)\)`).FindAllStringSubmatch(query, -1)
	if len(matches) != 1 {
		t.Fatalf("query has %d `%s IN (...)` lists, want one:\n%s", len(matches), column, query)
	}
	values := []string{}
	for _, literal := range strings.Split(matches[0][1], ",") {
		values = append(values, strings.Trim(strings.TrimSpace(literal), "'"))
	}
	return values
}

// The account and reconciliation queries filter on exactly the published
// store labels, so every market the collector labels is also counted. The
// SQL once named six markets while the labels named nine: TAO, Bitcoin and
// ETH subscriptions were published as a constant zero.
func TestSubscriptionMetricsQueriesTrackEveryPublishedStore(t *testing.T) {
	stores := SubscriptionMetricsStores()
	for _, market := range subscriptionMetricsNewMarkets {
		if !slices.Contains(stores, market) {
			t.Errorf("store labels omit %s", market)
		}
	}
	for name, list := range map[string][]string{
		"account":        subscriptionMetricsInList(t, subscriptionMetricsAccountQuery, "renewal.market"),
		"reconciliation": subscriptionMetricsInList(t, subscriptionMetricsReconciliationQuery, "event.store"),
	} {
		if !slices.Equal(list, stores) {
			t.Errorf("%s query tracks %v, want the store labels %v", name, list, stores)
		}
		for _, market := range subscriptionMetricsNewMarkets {
			if !slices.Contains(list, market) {
				t.Errorf("%s query leaves out %s", name, market)
			}
		}
	}
}
