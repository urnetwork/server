package monitor

import (
	"context"
	"strings"
	"testing"
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
	if entitlement := requireAlertClass(t, alerts, "payment-entitlement-missing"); entitlement.Severity != SeverityPage {
		t.Fatalf("entitlement severity = %s, want page", entitlement.Severity)
	}
	if orphan := requireAlertClass(t, alerts, "payment-renewal-orphan"); orphan.Severity != SeverityWarn {
		t.Fatalf("orphan renewal severity = %s, want warn", orphan.Severity)
	}
}

func TestPaymentFailuresSeparatesExistingAndDeletedNetworks(t *testing.T) {
	if strings.Count(paymentFailuresQuery, "renewal.market IN ('apple', 'google', 'monero', 'solana', 'stripe', 'zcash')") != 1 {
		t.Fatal("payment failures query does not limit entitlement checks to authoritative provider markets")
	}
	if strings.Count(paymentFailuresQuery, "renewal.market IN ('apple', 'google', 'stripe')") != 1 {
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
