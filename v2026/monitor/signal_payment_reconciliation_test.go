package monitor

import (
	"context"
	"fmt"
	"os"
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
	watermarks := map[string]Alert{}
	for _, alert := range alerts {
		if alert.Class == "payment-reconciliation-watermark-stale" {
			watermarks[alert.Target] = alert
		}
	}
	if len(watermarks) != 2 {
		t.Fatalf("stale watermark alerts = %d, want 2: %+v", len(watermarks), alerts)
	}
	missingWatermark := watermarks["apple"]
	if missingWatermark.Severity != SeverityPage {
		t.Fatalf("missing watermark severity = %q, want page: %+v", missingWatermark.Severity, missingWatermark)
	}
	staleWatermark := watermarks["google"]
	if staleWatermark.Severity != SeverityWarn {
		t.Fatalf("four-hour watermark severity = %q, want warn: %+v", staleWatermark.Severity, staleWatermark)
	}
	for _, expected := range []string{
		"payment-reconciliation-watermark-stale",
		"never force the watermark forward",
		"zero skip/error events through two hourly runs",
	} {
		if !strings.Contains(missingWatermark.Markdown(), expected) {
			t.Fatalf("watermark alert omits %q:\n%s", expected, missingWatermark.Markdown())
		}
	}
	for _, secret := range []string{"synthetic-provider-token", "synthetic-transaction-id", "synthetic-account-id"} {
		requireAlertOmits(t, skipped, secret)
		requireAlertOmits(t, errorAlert, secret)
		requireAlertOmits(t, missingWatermark, secret)
	}
}

// Successful scheduling and sibling stores cannot identify which Stripe stage
// prevented its watermark from advancing, including old terminal handling.
func TestPaymentReconciliationStalledStripeCreditPreservesStageUncertainty(t *testing.T) {
	source := paymentReconciliationSource(
		[]Row{
			{"apple", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"stripe", "600", "432000", "0", "6", "600", "1", "0", "0"},
		},
		nil,
	)
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("stalled Stripe alerts = %d, want only store-error and watermark-stale: %+v", len(alerts), alerts)
	}
	requireAlertClass(t, alerts, "payment-reconciliation-store-error")
	requireAlertClass(t, alerts, "payment-reconciliation-watermark-stale")
	for _, alert := range alerts {
		if alert.Target != "stripe" || alert.Severity != SeverityPage {
			t.Fatalf("stalled Stripe misclassified as another store or severity: %+v", alert)
		}
		rendered := alert.Markdown()
		for _, expected := range []string{
			"listing may already have succeeded",
			"per-invoice credit",
			"executing Taskworker source",
			"both destination_deleted and destination_unresolved",
			"mandatory audit persistence",
		} {
			if !strings.Contains(rendered, expected) {
				t.Errorf("stalled Stripe %s omits %q:\n%s", alert.Class, expected, rendered)
			}
		}
		if strings.Contains(rendered, "proves the authoritative listing is not completing") {
			t.Errorf("stalled Stripe %s incorrectly attributes an aggregate to provider listing:\n%s", alert.Class, rendered)
		}
		requireAlertOmits(t, alert, "in_synthetic_private_2099", "synthetic-account-id", "synthetic-provider-token")
	}
}

func TestPaymentReconciliationCatalogNamesWatermarkClass(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	start := strings.Index(catalog, "### 2.21 Payment reconciliation liveness and repair audit")
	end := strings.Index(catalog, "### 2.22 Durable payment and entitlement failures")
	if start < 0 || end <= start {
		t.Fatal("payment reconciliation catalog section boundaries are missing")
	}
	section := strings.Join(strings.Fields(catalog[start:end]), " ")
	for _, expected := range []string{
		"`payment-reconciliation-watermark-stale`",
		"`payment-reconciliation-credit-unfulfillable`",
		"`payment-reconciliation-credit-unfulfillable-invalid`",
		"more than three hours old",
		"more than six hours old or absent",
		"never force the watermark forward",
		"two natural hourly runs",
		"distinct provider evidence",
		"does not pin the Stripe watermark",
		"audit append is the only retained handle",
		"keeps the watermark fixed",
		"Successfully completed repair audit inserts remain best effort",
		"`destination_unresolved`",
		"Missing or malformed authority data",
		"Incomplete checkout pagination",
	} {
		if !strings.Contains(section, expected) {
			t.Errorf("SIGNALS.md §2.21 omits %q", expected)
		}
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

func TestPaymentReconciliationSurfacesUnfulfillableStripeDestinationOncePerEvidence(t *testing.T) {
	source := paymentReconciliationSourceWithUnfulfillable(
		[]Row{
			{"apple", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"stripe", "600", "600", "0", "0", "600", "1", "0", "0"},
		},
		nil,
		[]Row{{"1", "3", "3", "0", "120"}},
	)
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("unfulfillable alerts = %d, want 1: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "payment-reconciliation-credit-unfulfillable")
	if alert.Target != "stripe" || alert.Frame != "action=credit_unfulfillable" || alert.Severity != SeverityPage {
		t.Fatalf("unexpected unfulfillable alert identity: %+v", alert)
	}
	rendered := alert.Markdown()
	for _, expected := range []string{
		"1 paid invoice destination(s)",
		"distinct_evidence_24h=1 observations_24h=3 distinct_runs_24h=3",
		"deleted before the ledger-gated credit",
		"legacy-email resolution found no destination",
		"incomplete pages",
		"destination_unresolved",
		"not a provider-listing failure",
		"does not pin the store watermark",
		"explicit authorized disposition",
		"Do not recreate or retarget the deleted network",
		"repeated overlap audit",
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("unfulfillable alert omits %q:\n%s", expected, rendered)
		}
	}
	requireAlertOmits(
		t,
		alert,
		"in_synthetic_private_2099",
		"synthetic-network-id",
		"synthetic-run-id",
		"synthetic-provider-token",
	)
	if !strings.Contains(paymentReconciliationUnfulfillableQuery, "count(DISTINCT evidence)") ||
		!strings.Contains(paymentReconciliationUnfulfillableQuery, "action = 'credit_unfulfillable'") ||
		!strings.Contains(paymentReconciliationUnfulfillableQuery, "COALESCE(details::jsonb ->> 'reason', '') NOT IN ('destination_deleted', 'destination_unresolved')") {
		t.Fatal("unfulfillable query does not aggregate distinct terminal evidence")
	}
}

func TestPaymentReconciliationKeepsInvalidUnfulfillableDispositionObservable(t *testing.T) {
	source := paymentReconciliationSourceWithUnfulfillable(
		[]Row{
			{"apple", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"google", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"solana", "600", "600", "0", "0", "600", "1", "0", "0"},
			{"stripe", "600", "600", "0", "0", "600", "1", "0", "0"},
		},
		nil,
		[]Row{{"1", "3", "3", "1", "120"}},
	)
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("invalid disposition alerts = %d, want 1: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "payment-reconciliation-credit-unfulfillable-invalid")
	if alert.Target != "stripe" || alert.Severity != SeverityPage ||
		!strings.Contains(alert.Markdown(), "invalid_observations_24h=1") {
		t.Fatalf("invalid disposition alert=%+v", alert)
	}
}

func TestPaymentReconciliationRejectsMalformedUnfulfillableAggregate(t *testing.T) {
	for _, rows := range [][]pgRow{
		nil,
		{{"0", "0", "0", "0", "0"}},
		{{"4", "3", "1", "0", "120"}},
		{{"1", "3", "4", "0", "120"}},
		{{"1", "3", "3", "4", "120"}},
		{{"1", "3", "3", "0", "-1"}},
	} {
		if finding, err := paymentReconciliationUnfulfillableFinding(rows); err == nil || finding != nil {
			t.Fatalf("malformed unfulfillable aggregate accepted: rows=%v finding=%+v", rows, finding)
		}
	}
}

func TestPaymentReconciliationPagesInvalidUnfulfillableDisposition(t *testing.T) {
	finding, err := paymentReconciliationUnfulfillableFinding([]pgRow{{"1", "3", "3", "1", "120"}})
	if err != nil {
		t.Fatal(err)
	}
	if finding == nil || finding.class != "payment-reconciliation-credit-unfulfillable-invalid" || finding.tier != tierPage ||
		!strings.Contains(finding.observed, "invalid_observations_24h=1") ||
		strings.Contains(fmt.Sprintf("%+v", finding), "synthetic") {
		t.Fatalf("invalid disposition finding=%+v", finding)
	}
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
	return paymentReconciliationSourceWithUnfulfillable(
		health,
		repairs,
		[]Row{{"0", "0", "0", "0", "-1"}},
	)
}

func paymentReconciliationSourceWithUnfulfillable(health []Row, repairs []Row, unfulfillable []Row) *syntheticSource {
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "monitor-signal-2.21-payment-reconciliation-health"):
			return health, nil
		case strings.Contains(query, "monitor-signal-2.21-payment-reconciliation-repairs"):
			return repairs, nil
		case strings.Contains(query, "monitor-signal-2.21-payment-reconciliation-unfulfillable"):
			return unfulfillable, nil
		default:
			return nil, nil
		}
	}}
}
