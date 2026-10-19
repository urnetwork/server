package monitor

// Google deletion guidance follows the cancellation and store-snapshot
// capabilities without treating retained paid history as provider state.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The real reducer must name the Google deletion capabilities rather than
// directing this market only to the older Stripe correction.
func TestPaymentGoogleOrphanGuidanceUsesDeletionCapabilities(t *testing.T) {
	alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{"orphan_renewal", "google", "2", "7200", "600"}}, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("Google orphan alert count = %d, want 1", len(alerts))
	}
	alert := requireAlertClass(t, alerts, "payment-renewal-orphan")
	for _, want := range []string{
		"commit 1a67d9d3",
		"Google Play cancellation before deletion",
		"commit cd881b22",
		"renewal recheck under the deletion lock",
		"only where those capabilities are absent",
	} {
		if !strings.Contains(alert.Action, want) {
			t.Fatalf("Google orphan lacks deletion capability guidance: %q", want)
		}
	}
	if alert.Severity != SeverityWarn || alert.Sustain != 1 ||
		alert.Identity() != "pg/payment-failures|payment-renewal-orphan|google|kind=orphan_renewal" {
		t.Fatal("Google guidance changed the warning identity, severity, or sustain")
	}
	if alert.Observed != "kind=orphan_renewal target=google deleted_owner_count=2 oldest_renewal_start_age_seconds=7200 newest_renewal_start_age_seconds=600" {
		t.Fatal("Google guidance changed the observed deleted-owner count or renewal ages")
	}
	if strings.Contains(alert.Action, "Apple and Google disposition is separate policy") {
		t.Fatal("Google guidance still omits the implemented store deletion policy")
	}
}

// A canceled subscription retains its paid local window. This same aggregate
// must keep the warning while refusing to infer current billing or failed repair.
func TestPaymentGoogleOrphanPreservesCanceledHistoryControl(t *testing.T) {
	alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{"orphan_renewal", "google", "1", "3600", "3600"}}, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "payment-renewal-orphan")
	for _, want := range []string{
		"successful cancellation",
		"paid local window",
		"provider-side disposition",
		"historical deletion artifact or deletion time",
		"no new unresolved provider disposition",
		"Confirmed canceled paid-window history may remain",
		"Alert disappearance from natural expiry alone is not causal closure",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("Google orphan lacks canceled-history or authority qualifier: %q", want)
		}
	}
	for _, forbidden := range []string{
		"no new deleted owner enters this aggregate",
		"retain an active google renewal",
	} {
		if strings.Contains(alert.Markdown(), forbidden) {
			t.Fatalf("Google orphan still attributes provider failure from retained history: %q", forbidden)
		}
	}
	if alert.Severity != SeverityWarn {
		t.Fatal("canceled-history control suppressed or escalated the retained warning")
	}
}

// Missing or partial database evidence cannot become a healthy empty result,
// even when the partial response happens to contain an orphan group.
func TestPaymentGoogleOrphanUnavailableEvidenceRemainsUnknown(t *testing.T) {
	sourceErr := errors.New("synthetic source unavailable")
	for _, rows := range [][]Row{nil, {{"orphan_renewal", "google", "1", "3600", "3600"}}} {
		alerts, err := NewPaymentFailuresSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
			postgresFn: func(string) ([]Row, error) { return rows, sourceErr },
		}))
		if !errors.Is(err, sourceErr) || len(alerts) != 0 {
			t.Fatal("unavailable Google orphan evidence was accepted as a complete observation")
		}
	}
}

// The Google-specific correction does not change another store's action or
// transfer its capability evidence to that store.
func TestPaymentGoogleOrphanGuidanceKeepsOtherMarketsSeparate(t *testing.T) {
	for _, market := range []string{"apple", "stripe"} {
		finding, err := paymentFailureFinding("orphan_renewal", market, 1, 3600, 3600)
		if err != nil {
			t.Fatal(err)
		}
		if finding.tier != tierWarn || finding.target != market || finding.class != "payment-renewal-orphan" {
			t.Fatalf("Google correction changed the %s orphan warning", market)
		}
		if !strings.Contains(finding.action, "current-main Server commit 2b28387a") ||
			!strings.Contains(finding.action, "commit df2ea15b") ||
			strings.Contains(finding.action, "commit 1a67d9d3") ||
			strings.Contains(finding.action, "commit cd881b22") {
			t.Fatalf("Google deletion guidance crossed into the %s orphan action", market)
		}
	}
}
