package monitor

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// paymentFailuresInLists returns the string literals of each
// `renewal.market IN (...)` list in the payment failures query, in order.
func paymentFailuresInLists(t *testing.T) [][]string {
	t.Helper()
	lists := [][]string{}
	for _, match := range regexp.MustCompile(`renewal\.market IN \(([^)]*)\)`).FindAllStringSubmatch(paymentFailuresQuery, -1) {
		values := []string{}
		for _, literal := range strings.Split(match[1], ",") {
			values = append(values, strings.Trim(strings.TrimSpace(literal), "'"))
		}
		lists = append(lists, values)
	}
	if len(lists) != 2 {
		t.Fatalf("payment failures query has %d market lists, want the entitlement and orphan lists", len(lists))
	}
	return lists
}

// A subscription paid with TAO, Bitcoin, ETH or gamma is a paying account like
// any other: a current renewal with no Pro window pages. The check once named
// no gamma, and the reader refused a gamma row as an unknown market, which
// failed the whole probe.
func TestPaymentFailuresWatchesEveryPaymentMarketEntitlement(t *testing.T) {
	entitlementMarkets := paymentFailuresInLists(t)[0]
	for _, market := range []string{"bittensor", "bitcoin", "ethereum", "gamma"} {
		if !slices.Contains(entitlementMarkets, market) {
			t.Errorf("entitlement check leaves out %s: %v", market, entitlementMarkets)
		}
		alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
			postgresFn: func(string) ([]Row, error) {
				return []Row{{"entitlement_missing", market, "1", "7200", "7200"}}, nil
			},
		}))
		if err != nil {
			t.Fatalf("%s entitlement row: %v", market, err)
		}
		alert := requireAlertClass(t, alerts, "payment-entitlement-missing")
		if alert.Severity != SeverityPage {
			t.Errorf("%s entitlement severity = %s, want page", market, alert.Severity)
		}
		finding, err := paymentFailureFinding("entitlement_missing", market, 1, 7200, 7200)
		if err != nil {
			t.Fatal(err)
		}
		// the hourly reconciler repairs no Pro window in these markets: the
		// alert must not send the operator to wait for it
		for _, want := range []string{
			"repairs a Pro window only for Apple, Google Play, Stripe and Solana",
			"nothing restores it",
			"with the owner's go",
		} {
			if !strings.Contains(finding.mechanism+" "+finding.action, want) {
				t.Errorf("%s entitlement alert lacks %q:\n%s\n%s", market, want, finding.mechanism, finding.action)
			}
		}
		if strings.Contains(finding.action, "allow the ordinary reconciler") || strings.Contains(finding.verify, "entitlement_repaired event") {
			t.Errorf("%s entitlement alert waits for a reconciler repair that never runs:\n%s\n%s", market, finding.action, finding.verify)
		}
	}
	// the stores the reconciler repairs keep their guidance
	finding, err := paymentFailureFinding("entitlement_missing", "apple", 1, 7200, 7200)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(finding.action, "allow the ordinary reconciler") {
		t.Errorf("apple entitlement alert lost its reconciler repair:\n%s", finding.action)
	}
}

// A plan paid with gamma renews from the account's deposit through our own
// biller, which account deletion does not stop. A deleted account with a
// current gamma renewal is a recurring-subscription orphan, with gamma's own
// disposition; a one-time crypto year is prepaid history, as Solana's is.
func TestPaymentFailuresWatchesGammaOrphanRenewals(t *testing.T) {
	orphanMarkets := paymentFailuresInLists(t)[1]
	if !slices.Contains(orphanMarkets, "gamma") {
		t.Fatalf("orphan-renewal check leaves out gamma: %v", orphanMarkets)
	}
	for _, prepaid := range []string{"bittensor", "bitcoin", "ethereum", "monero", "solana", "zcash"} {
		if slices.Contains(orphanMarkets, prepaid) {
			t.Errorf("orphan-renewal check counts prepaid %s history", prepaid)
		}
		if _, err := paymentFailureFinding("orphan_renewal", prepaid, 1, 10, 5); err == nil {
			t.Errorf("prepaid %s history was accepted as a recurring-subscription orphan", prepaid)
		}
	}

	alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{"orphan_renewal", "gamma", "1", "86400", "86400"}}, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "payment-renewal-orphan")
	if alert.Severity != SeverityWarn {
		t.Errorf("gamma orphan severity = %s, want warn", alert.Severity)
	}
	orphan, err := paymentFailureFinding("orphan_renewal", "gamma", 1, 86400, 86400)
	if err != nil {
		t.Fatal(err)
	}
	rendered := alertFromFinding(
		syntheticSettings(&syntheticSource{}),
		"2.22",
		"payment-failures",
		"Durable payment and entitlement failures",
		orphan,
	).Markdown()
	for _, want := range []string{
		"1 deleted account(s) retain in-window local gamma renewal history",
		"deleted_owner_count=1",
		"renews through our own biller, not a store",
		"gamma_account row",
		"without reading the network",
		"stop the deleted owner's automatic gamma renewal with the owner's go",
		"authorized refund or support disposition",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("gamma orphan alert lacks %q:\n%s", want, rendered)
		}
	}
	// the store dispositions are not gamma's
	for _, storeOnly := range []string{"Stripe cancellation", "Google Play", "provider tooling"} {
		if strings.Contains(rendered, storeOnly) {
			t.Errorf("gamma orphan alert carries store guidance %q:\n%s", storeOnly, rendered)
		}
	}
}
