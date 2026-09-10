package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestPaymentReconciliationHealthy(t *testing.T) {
	source := paymentReconciliationSource(
		[]Row{
			{"apple", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"stripe", "600", "600", "0", "0", "600", "1", "0", "0"},
		},
		nil,
	)
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy reconciliation alerts = %+v", alerts)
	}
}

// pending_task stores runtime-qualified Go symbols. A bare function name
// silently reports zero rows even while the recurring singleton is healthy.
func TestPaymentReconciliationTaskQueryUsesCanonicalFunctionName(t *testing.T) {
	const canonical = "WHERE function_name = 'github.com/urnetwork/server/taskworker/work.PaymentReconcile'"
	if !strings.Contains(paymentReconciliationHealthQuery, canonical) {
		t.Fatalf("payment reconciliation task query omits canonical function name")
	}
	if strings.Contains(paymentReconciliationHealthQuery, "WHERE function_name = 'PaymentReconcile'") {
		t.Fatal("payment reconciliation task query retains bare function name")
	}
}

func TestPaymentReconciliationFindsPerStoreSkipErrorAndStaleWatermark(t *testing.T) {
	source := paymentReconciliationSource(
		[]Row{
			{"apple", "1800", "-1", "3", "0", "1800", "1", "0", "0"},
			{"google", "1800", "14400", "0", "2", "1800", "1", "0", "0"},
			{"solana", "1800", "1800", "0", "0", "1800", "1", "0", "0"},
			{"stripe", "1800", "1800", "0", "0", "1800", "1", "0", "0"},
		},
		nil,
	)
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	skipped := requireAlertClass(t, alerts, "payment-reconciliation-store-skipped")
	if skipped.Target != "apple" || skipped.Severity != SeverityPage {
		t.Fatalf("unexpected skipped-store alert: %+v", skipped)
	}
	errorAlert := requireAlertClass(t, alerts, "payment-reconciliation-store-error")
	if errorAlert.Target != "google" || errorAlert.Severity != SeverityPage {
		t.Fatalf("unexpected store-error alert: %+v", errorAlert)
	}
	watermarks := 0
	for _, alert := range alerts {
		if alert.Class == "payment-reconciliation-watermark-stale" {
			watermarks++
		}
	}
	if watermarks != 2 {
		t.Fatalf("stale watermark alerts = %d, want 2: %+v", watermarks, alerts)
	}
	for _, secret := range []string{"synthetic-provider-token", "synthetic-transaction-id", "synthetic-account-id"} {
		requireAlertOmits(t, skipped, secret)
		requireAlertOmits(t, errorAlert, secret)
	}
}

func TestPaymentReconciliationFindsDeadTaskAndHeartbeat(t *testing.T) {
	source := paymentReconciliationSource(
		[]Row{
			{"apple", "10801", "600", "0", "0", "600", "0", "0", "0"},
			{"google", "10801", "600", "0", "0", "600", "0", "0", "0"},
			{"solana", "10801", "600", "0", "0", "600", "0", "0", "0"},
			{"stripe", "10801", "600", "0", "0", "600", "0", "0", "0"},
		},
		nil,
	)
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "payment-reconciliation-stale")
	requireAlertClass(t, alerts, "payment-reconciliation-task")
}

func TestPaymentReconciliationSurfacesSafetyNetRepairs(t *testing.T) {
	source := paymentReconciliationSource(
		[]Row{
			{"apple", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"stripe", "600", "600", "0", "0", "600", "1", "0", "0"},
		},
		[]Row{
			{"apple", "credited", "1", "120"},
			{"google", "entitlement_repaired", "1", "180"},
			{"stripe", "credited", "2", "240"},
		},
	)
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 3 {
		t.Fatalf("repair alerts = %d, want 3: %+v", len(alerts), alerts)
	}
	for _, alert := range alerts {
		if alert.Class != "payment-reconciliation-repair" {
			t.Fatalf("unexpected alert: %+v", alert)
		}
		for _, expected := range []string{"safety net", "ordinary", "exactly once"} {
			if !strings.Contains(strings.ToLower(alert.Markdown()), strings.ToLower(expected)) {
				t.Fatalf("repair alert omits %q:\n%s", expected, alert.Markdown())
			}
		}
		if strings.Contains(alert.Markdown(), "no real-time subscription-lifecycle consumer") {
			t.Fatalf("generic repair received Stripe-ended semantics:\n%s", alert.Markdown())
		}
		requireAlertOmits(t, alert, "synthetic-run-id", "synthetic-account-id", "synthetic-transaction-id")
	}
}

func TestPaymentReconciliationSpecializesStripeEndedLifecycleGap(t *testing.T) {
	source := paymentReconciliationSource(
		[]Row{
			{"apple", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"stripe", "600", "600", "0", "0", "600", "1", "0", "0"},
		},
		[]Row{{"stripe", "ended", "2", "240"}},
	)
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("Stripe ended alerts = %d, want 1: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "payment-reconciliation-repair")
	if alert.Target != "stripe" || alert.Frame != "action=ended" || alert.Severity != SeverityWarn {
		t.Fatalf("Stripe ended alert identity changed: %+v", alert)
	}
	rendered := alert.Markdown()
	for _, expected := range []string{
		"first applied 2 terminal Stripe subscription state(s)",
		"no real-time subscription-lifecycle consumer",
		"first implemented application path",
		"does not prove a lost delivery from an implemented handler",
		"idempotency ledger that does not exist",
		"Stripe still reports the subscription as terminal",
		"local renewal and matching Pro entitlement are ended",
		"no new stripe/ended repair",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("Stripe ended alert omits %q:\n%s", expected, rendered)
		}
	}
	for _, unsupported := range []string{
		"repaired 2 missed stripe ended event(s)",
		"later notifications apply normally",
		"trace the provider notification, verification, idempotency ledger",
	} {
		if strings.Contains(rendered, unsupported) {
			t.Fatalf("Stripe ended alert retains unsupported claim %q:\n%s", unsupported, rendered)
		}
	}
	requireAlertOmits(t, alert, "synthetic-run-id", "synthetic-account-id", "synthetic-transaction-id")
}

func TestPaymentReconciliationRejectsIncompleteOrUnknownAggregate(t *testing.T) {
	tests := []struct {
		name string
		rows []Row
	}{
		{name: "missing store", rows: []Row{
			{"apple", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
		}},
		{name: "unknown store", rows: []Row{
			{"apple", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"synthetic-unknown", "600", "600", "0", "0", "600", "1", "0", "0"},
		}},
		{name: "noncanonical integer", rows: []Row{
			{"apple", "0600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"stripe", "600", "600", "0", "0", "600", "1", "0", "0"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewPaymentReconciliationSignal().Run(
				context.Background(), syntheticSettings(paymentReconciliationSource(test.rows, nil)),
			)
			if err == nil {
				t.Fatal("malformed aggregate error = nil")
			}
		})
	}
}

func paymentReconciliationSource(health []Row, repairs []Row) *syntheticSource {
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "monitor-signal-2.21-payment-reconciliation-health"):
			return health, nil
		case strings.Contains(query, "monitor-signal-2.21-payment-reconciliation-repairs"):
			return repairs, nil
		default:
			return nil, nil
		}
	}}
}
