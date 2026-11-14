package monitor

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

func TestPaymentFailuresHealthy(t *testing.T) {
	alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "monitor-signal-2.22-payment-failures") {
				t.Fatalf("unexpected query")
			}
			return nil, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy payment alerts = %+v", alerts)
	}
}

func TestPaymentFailuresClassifiesEveryDurableProblemWithoutIdentifiers(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{
			{"credit_unfulfillable", "gift", "1", "600", "600"},
			{"credit_unfulfillable", "stripe", "2", "900", "300"},
			{"entitlement_missing", "apple", "1", "7200", "7200"},
			{"orphan_renewal", "google", "2", "5400", "1200"},
			{"refund_unmatched", "stripe", "3", "3600", "60"},
			{"solana_unfulfilled", "no_intent", "4", "1800", "120"},
			{"solana_unfulfilled", "underpaid", "1", "600", "600"},
		}, nil
	}}
	alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{
		"payment-entitlement-missing",
		"payment-renewal-orphan",
		"payment-refund-unmatched",
		"payment-solana-unfulfilled",
		"payment-credit-unfulfillable",
		"payment-gift-mismatch",
	} {
		alert := requireAlertClass(t, alerts, class)
		for _, forbidden := range []string{
			"synthetic-network-id", "synthetic-user@example.invalid", "synthetic-transaction-signature", "synthetic-provider-token",
		} {
			requireAlertOmits(t, alert, forbidden)
		}
	}
	if unfulfillable := requireAlertClass(t, alerts, "payment-credit-unfulfillable"); unfulfillable.Severity != SeverityWarn {
		t.Fatalf("unfulfillable credit severity = %s, want warn", unfulfillable.Severity)
	}
	if mismatch := requireAlertClass(t, alerts, "payment-gift-mismatch"); mismatch.Severity != SeverityWarn {
		t.Fatalf("gift mismatch severity = %s, want warn", mismatch.Severity)
	}
	if entitlement := requireAlertClass(t, alerts, "payment-entitlement-missing"); entitlement.Severity != SeverityPage {
		t.Fatalf("entitlement severity = %s, want page", entitlement.Severity)
	}
	if orphan := requireAlertClass(t, alerts, "payment-renewal-orphan"); orphan.Severity != SeverityWarn {
		t.Fatalf("orphan renewal severity = %s, want warn", orphan.Severity)
	}
}

// The crypto rail's held payments and stalled scanners page (a sanctions
// match, an unreadable screen, a scanner that stopped) or warn (a payment an
// operator must check in Coinbase), without identifiers.
func TestPaymentFailuresClassifiesTheCryptoRail(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{
			{"crypto_payment_held", "tao", "1", "3600", "3600"},
			{"crypto_sanctions_hold", "btc", "1", "600", "600"},
			{"crypto_scanner_stalled", "tao", "1", "1200", "1200"},
			{"crypto_screening_unavailable", "eth", "2", "900", "300"},
		}, nil
	}}
	alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	for class, severity := range map[string]Severity{
		"crypto-sanctions-hold":        SeverityPage,
		"crypto-screening-unavailable": SeverityPage,
		"crypto-scanner-stalled":       SeverityPage,
		"crypto-payment-held":          SeverityWarn,
	} {
		alert := requireAlertClass(t, alerts, class)
		if alert.Severity != severity {
			t.Fatalf("%s severity = %s, want %s", class, alert.Severity, severity)
		}
	}
	// an asset the rail does not have is refused, not reported
	_, err = NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{{"crypto_sanctions_hold", "ltc", "1", "600", "600"}}, nil
	}}))
	if err == nil {
		alerts, _ := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
			return []Row{{"crypto_sanctions_hold", "ltc", "1", "600", "600"}}, nil
		}}))
		for _, alert := range alerts {
			if alert.Class == "crypto-sanctions-hold" {
				t.Fatalf("an unknown crypto asset was reported")
			}
		}
	}
	if !strings.Contains(paymentFailuresQuery, "FROM crypto_payment_receipt receipt") || !strings.Contains(paymentFailuresQuery, "FROM crypto_scan_cursor scan") {
		t.Fatalf("payment failures query reads no crypto rail state")
	}
}

func TestPaymentFailuresSeparatesExistingAndDeletedNetworks(t *testing.T) {
	if strings.Count(paymentFailuresQuery, "renewal.market IN ('apple', 'bitcoin', 'bittensor', 'ethereum', 'gamma', 'google', 'monero', 'solana', 'stripe', 'zcash')") != 1 {
		t.Fatal("payment failures query does not limit entitlement checks to authoritative provider markets")
	}
	if strings.Count(paymentFailuresQuery, "renewal.market IN ('apple', 'gamma', 'google', 'stripe')") != 1 {
		t.Fatal("payment failures query does not limit orphan-renewal checks to recurring provider markets")
	}
	if !strings.Contains(paymentFailuresQuery, "AND EXISTS (\n          SELECT 1 FROM network") {
		t.Fatal("payment failures query does not require an existing network for entitlement repair")
	}
	if !strings.Contains(paymentFailuresQuery, "AND NOT EXISTS (\n          SELECT 1 FROM network") ||
		!strings.Contains(paymentFailuresQuery, "'orphan_renewal'") {
		t.Fatal("payment failures query does not retain deleted-network renewals as a separate aggregate")
	}
	if _, err := paymentFailureFinding("orphan_renewal", "solana", 1, 10, 5); err == nil {
		t.Fatal("prepaid Solana history was accepted as a recurring-subscription orphan")
	}
	orphan, err := paymentFailureFinding("orphan_renewal", "stripe", 2, 5400, 1200)
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
		"2 deleted account(s)",
		"retain in-window local stripe renewal history",
		"distinct deleted owners rather than renewal rows",
		"deleted_owner_count=2",
		"oldest_renewal_start_age_seconds=5400",
		"provider-side disposition",
		"network-row deletion/credit fence",
		"provider-confirmed cancellation-before-delete",
		"current-main Server commit 2b28387a",
		"commit df2ea15b",
		"Deploy API and Taskworker only",
		"missing active local renewal metadata remain a separate discovery gap",
		"Apple and Google disposition is separate policy",
		"Alert disappearance from natural expiry alone is not causal closure",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("orphan renewal alert missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "retain an active stripe renewal") {
		t.Fatalf("orphan renewal alert overclaims provider state:\n%s", rendered)
	}
}

// The markets and coins the signal accepts come from the model's one list of
// each, so the next market the dashboard counts or the next coin the rail
// sells is watched here without an edit, and a row for it is never refused as
// unknown (which fails the whole probe).
func TestPaymentFailuresMarketsAreTheModelMarkets(t *testing.T) {
	if want := slices.Sorted(slices.Values(model.SubscriptionMarkets)); !slices.Equal(paymentFailuresEntitlementMarkets, want) {
		t.Errorf("entitlement markets = %v, want every subscription market %v", paymentFailuresEntitlementMarkets, want)
	}
	recurring := slices.DeleteFunc(slices.Clone(model.SubscriptionMarkets), func(market string) bool {
		return slices.Contains(model.SubscriptionOneTimeCryptoMarkets, market)
	})
	if want := slices.Sorted(slices.Values(recurring)); !slices.Equal(paymentFailuresOrphanMarkets, want) {
		t.Errorf("orphan-renewal markets = %v, want the recurring markets %v; give a new one its own disposition in paymentFailureFinding", paymentFailuresOrphanMarkets, want)
	}
	for _, market := range paymentFailuresRepairedEntitlementMarkets {
		if !slices.Contains(paymentFailuresEntitlementMarkets, market) {
			t.Errorf("repaired market %s is not an entitlement market", market)
		}
	}
	if len(paymentFailuresCryptoAssets) != len(model.CryptoAssets) {
		t.Errorf("crypto coins = %v, want the rail's %v", paymentFailuresCryptoAssets, model.CryptoAssets)
	}
	for _, asset := range model.CryptoAssets {
		if !paymentFailuresCryptoAssets[asset] {
			t.Errorf("the rail's coin %s is refused as unknown", asset)
		}
	}
	for _, unsafe := range []string{"", "o'brien", "Apple", "apple', 'x"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("market name %q was rendered into SQL", unsafe)
				}
			}()
			paymentFailuresSqlList([]string{unsafe})
		}()
	}
}

func TestPaymentCreditUnfulfillableNeverSuggestsEmailAuthority(t *testing.T) {
	unfulfillable, err := paymentFailureFinding("credit_unfulfillable", "stripe", 1, 1800, 1800)
	if err != nil {
		t.Fatal(err)
	}
	markdown := alertFromFinding(
		syntheticSettings(&syntheticSource{}),
		"2.22",
		"payment-failures",
		"Durable payment and entitlement failures",
		unfulfillable,
	).Markdown()
	for _, want := range []string{
		"customer-email fallback is removed",
		"set network_id metadata on the Stripe subscription",
		"resend the invoice.paid event",
		"Email alone is not safe write authority",
		"destination_unresolved with leg=webhook",
		"does not distinguish those reasons, establish account deletion, or prove a provider failure",
		"malformed records remain a separate §2.21 PAGE",
		"neither aging out nor a fresh reconciliation watermark proves disposition",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("unfulfillable credit alert missing %q:\n%s", want, markdown)
		}
	}
	if _, err := paymentFailureFinding("credit_unfulfillable", "solana", 1, 1800, 1800); err == nil {
		t.Fatal("unfulfillable credit accepted a store with no unfulfillable-credit writer")
	}
	if _, err := paymentFailureFinding("email_fallback", "stripe", 1, 1800, 1800); err == nil {
		t.Fatal("the retired email_fallback kind is still accepted")
	}
}

func TestPaymentFailuresRejectsUnknownOrMalformedAggregate(t *testing.T) {
	tests := []struct {
		name string
		row  Row
	}{
		{name: "unknown kind", row: Row{"synthetic_unknown", "stripe", "1", "10", "5"}},
		{name: "unknown market", row: Row{"entitlement_missing", "synthetic_market", "1", "10", "5"}},
		{name: "unknown orphan market", row: Row{"orphan_renewal", "synthetic_market", "1", "10", "5"}},
		{name: "prepaid market is not recurring orphan", row: Row{"orphan_renewal", "solana", "1", "10", "5"}},
		{name: "unknown Solana reason", row: Row{"solana_unfulfilled", "synthetic_reason", "1", "10", "5"}},
		{name: "nonpositive count", row: Row{"refund_unmatched", "stripe", "0", "10", "5"}},
		{name: "inverted age", row: Row{"credit_unfulfillable", "stripe", "1", "5", "10"}},
		{name: "wrong columns", row: Row{"credit_unfulfillable", "stripe"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
				postgresFn: func(string) ([]Row, error) { return []Row{test.row}, nil },
			}))
			if err == nil {
				t.Fatal("invalid aggregate error = nil")
			}
		})
	}
}

// Paid App Store and Play purchases that no live network can be credited for
// are now recorded as credit_unfulfillable (the webhooks and the reconciler).
// The aggregate must page them as a WARN with store-specific disposition
// guidance, and drop a purchase the app later bound and credited.
func TestPaymentCreditUnfulfillableCoversStorePurchases(t *testing.T) {
	for _, want := range []string{
		"event.store IN ('apple', 'google', 'stripe')",
		"FROM apple_subscription_transaction ledger",
		"WHERE ledger.transaction_id = event.evidence",
		"FROM play_purchase_binding binding",
		"COALESCE(event.details::jsonb ->> 'reason', '') = 'destination_unresolved'",
	} {
		if !strings.Contains(paymentFailuresQuery, want) {
			t.Fatalf("payment failures query missing %q", want)
		}
	}

	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return []Row{
			{"credit_unfulfillable", "apple", "2", "900", "300"},
			{"credit_unfulfillable", "google", "1", "600", "600"},
		}, nil
	}}
	alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if count := requireAlertClassCount(alerts, "payment-credit-unfulfillable"); count != 2 {
		t.Fatalf("store credit-unfulfillable alerts = %d, want 2", count)
	}
	for _, alert := range alerts {
		if alert.Severity != SeverityWarn {
			t.Fatalf("store credit-unfulfillable severity = %s, want warn", alert.Severity)
		}
		requireAlertOmits(t, alert, "synthetic-network-id", "synthetic-provider-token")
	}

	for target, wants := range map[string][]string{
		"apple":  {"paid App Store purchase(s)", "no appAccountToken and no offer-code binding", "App Store Connect order lookup", "Never credit a network on an email or name match"},
		"google": {"paid Google Play purchase(s)", "bound to a different network", "order id on the customer's receipt", "Never credit a network on an email or name match"},
	} {
		unfulfillable, err := paymentFailureFinding("credit_unfulfillable", target, 1, 1800, 1800)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		markdown := alertFromFinding(
			syntheticSettings(&syntheticSource{}),
			"2.22",
			"payment-failures",
			"Durable payment and entitlement failures",
			unfulfillable,
		).Markdown()
		for _, want := range wants {
			if !strings.Contains(markdown, want) {
				t.Fatalf("%s credit-unfulfillable alert missing %q:\n%s", target, want, markdown)
			}
		}
		if strings.Contains(markdown, "set network_id metadata on the Stripe subscription") {
			t.Fatalf("%s credit-unfulfillable alert carries Stripe-only guidance:\n%s", target, markdown)
		}
	}
}

// A gift order's Checkout payment that did not match its order (store gift,
// action credit_unfulfillable, reason gift_payment_mismatch) is a known
// record: a WARN saying the payment did not match its order's amount or
// currency and needs an operator, with none of the Stripe invoice guidance and
// nothing of an invalid record. The query counts no other gift record.
func TestPaymentFailuresGiftMismatchIsAKnownRecord(t *testing.T) {
	for _, want := range []string{
		"event.store IN ('apple', 'google', 'stripe')",
		"AND event.store = 'gift'",
		"AND COALESCE(event.details::jsonb ->> 'reason', '') = 'gift_payment_mismatch'",
	} {
		if !strings.Contains(paymentFailuresQuery, want) {
			t.Fatalf("payment failures query missing %q", want)
		}
	}

	alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{"credit_unfulfillable", "gift", "2", "900", "300"}}, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if count := requireAlertClassCount(alerts, "payment-gift-mismatch"); count != 1 || len(alerts) != 1 {
		t.Fatalf("gift mismatch alerts = %+v", alerts)
	}
	alert := requireAlertClass(t, alerts, "payment-gift-mismatch")
	if alert.Severity != SeverityWarn {
		t.Fatalf("gift mismatch severity = %s, want warn", alert.Severity)
	}
	requireAlertOmits(t, alert, "synthetic-network-id", "synthetic-provider-token")

	mismatch, err := paymentFailureFinding("credit_unfulfillable", "gift", 1, 1800, 1800)
	if err != nil {
		t.Fatal(err)
	}
	markdown := alertFromFinding(
		syntheticSettings(&syntheticSource{}),
		"2.22",
		"payment-failures",
		"Durable payment and entitlement failures",
		mismatch,
	).Markdown()
	for _, want := range []string{
		"gift order payment(s) did not match their order's amount or currency and were not fulfilled",
		"reason gift_payment_mismatch",
		"An operator settles each payment with its buyer",
		"refund the payment in Stripe",
		"Never fulfil an order from this record alone",
		"aging out does not prove disposition",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("gift mismatch alert missing %q:\n%s", want, markdown)
		}
	}
	for _, unwanted := range []string{
		"set network_id metadata on the Stripe subscription",
		"resend the invoice.paid event",
		"writer-contract regression",
		"malformed records remain a separate §2.21 PAGE",
	} {
		if strings.Contains(markdown, unwanted) {
			t.Fatalf("gift mismatch alert carries %q:\n%s", unwanted, markdown)
		}
	}
	// the other unfulfillable stores keep their guidance, and a store with no
	// writer is still refused
	stripe, err := paymentFailureFinding("credit_unfulfillable", "stripe", 1, 1800, 1800)
	if err != nil || stripe.class != "payment-credit-unfulfillable" {
		t.Fatalf("stripe unfulfillable credit = %+v, %v", stripe, err)
	}
	if _, err := paymentFailureFinding("credit_unfulfillable", "solana", 1, 1800, 1800); err == nil {
		t.Fatal("unfulfillable credit accepted a store with no unfulfillable-credit writer")
	}
}
