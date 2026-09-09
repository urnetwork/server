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
}

func TestPaymentFailuresRejectsUnknownOrMalformedAggregate(t *testing.T) {
	tests := []struct {
		name string
		row  Row
	}{
		{name: "unknown kind", row: Row{"synthetic_unknown", "stripe", "1", "10", "5"}},
		{name: "unknown market", row: Row{"entitlement_missing", "synthetic_market", "1", "10", "5"}},
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
