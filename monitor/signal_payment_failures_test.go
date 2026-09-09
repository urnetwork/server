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
			{"email_fallback", "stripe", "2", "900", "300"},
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
		"payment-identity-fallback",
	} {
		alert := requireAlertClass(t, alerts, class)
		for _, forbidden := range []string{
			"synthetic-network-id", "synthetic-user@example.invalid", "synthetic-transaction-signature", "synthetic-provider-token",
		} {
			requireAlertOmits(t, alert, forbidden)
		}
	}
	if fallback := requireAlertClass(t, alerts, "payment-identity-fallback"); fallback.Severity != SeverityWarn {
		t.Fatalf("email fallback severity = %s, want warn", fallback.Severity)
	}
	if entitlement := requireAlertClass(t, alerts, "payment-entitlement-missing"); entitlement.Severity != SeverityPage {
		t.Fatalf("entitlement severity = %s, want page", entitlement.Severity)
	}
	if orphan := requireAlertClass(t, alerts, "payment-renewal-orphan"); orphan.Severity != SeverityWarn {
		t.Fatalf("orphan renewal severity = %s, want warn", orphan.Severity)
	}
}

func TestPaymentFailuresSeparatesExistingAndDeletedNetworks(t *testing.T) {
	if strings.Count(paymentFailuresQuery, "renewal.market IN ('apple', 'google', 'solana', 'stripe')") != 1 {
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
		"distinct deleted owners rather than renewal rows",
		"deleted_owner_count=2",
		"oldest_renewal_start_age_seconds=5400",
		"provider-side disposition",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("orphan renewal alert missing %q:\n%s", want, rendered)
		}
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
		{name: "inverted age", row: Row{"email_fallback", "stripe", "1", "5", "10"}},
		{name: "wrong columns", row: Row{"email_fallback", "stripe"}},
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
