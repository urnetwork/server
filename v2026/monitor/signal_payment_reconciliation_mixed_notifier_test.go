// Existing Stripe audit-family controls preserve known valid terminal
// evidence beside invalid records, without any payment or external transport.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The existing fake PostgreSQL source supplies the complete seven-store
// health census and no repair events; only its terminal aggregate varies.
func paymentMixedSettings(terminal []Row) SignalSettings {
	health := []Row{}
	for _, store := range []string{"apple", "google", "monero", "solana", "stripe", "x402", "zcash"} {
		health = append(health, Row{store, "600", "600", "0", "0", "600", "1", "0", "0"})
	}
	settings := syntheticSettings(paymentReconciliationSourceWithUnfulfillable(health, nil, terminal))
	return settings
}

// Inspect class membership without constructing or substituting an Alert.
func paymentMixedHasClass(alerts Alerts, class string) bool {
	for _, alert := range alerts {
		if alert.Class == class {
			return true
		}
	}
	return false
}

// All fields and public renderings must omit private malformed values or
// source diagnostics; regression messages never print the forbidden value.
func requirePaymentMixedPrivacy(t *testing.T, alerts Alerts, forbidden string) {
	t.Helper()
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal("payment audit alerts could not be encoded")
	}
	var lines strings.Builder
	if err := alerts.WriteJSONL(&lines); err != nil {
		t.Fatal("payment audit alerts could not be rendered")
	}
	for _, output := range []string{string(encoded), lines.String(), alerts.Markdown()} {
		if strings.Contains(output, forbidden) {
			t.Error("payment audit public output retained a synthetic private source value")
		}
	}
}

// One valid credit-leg record plus one null-evidence invalid record gives
// distinct evidence=1, observations=2, runs=2, invalid=1 at the same snapshot.
func TestPaymentReconciliationMixedPublicPreservesBothClasses(t *testing.T) {
	settings := paymentMixedSettings([]Row{{"1", "2", "2", "1", "120"}})
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("consistent mixed audit did not complete its public observation")
	}
	if len(alerts) != 2 || !paymentMixedHasClass(alerts, "payment-reconciliation-credit-unfulfillable") ||
		!paymentMixedHasClass(alerts, "payment-reconciliation-credit-unfulfillable-invalid") {
		t.Fatal("invalid audit records suppressed independently retained valid terminal evidence")
	}
	for _, alert := range alerts {
		if alert.Severity != SeverityPage || alert.Target != "stripe" || alert.Sustain != 1 || isVisibilityAlert(alert) {
			t.Fatal("mixed terminal evidence lost its independent fixed PAGE identity")
		}
	}
}

// Compose actual public Run, actual cadence gate and production incident
// reducer. The empty webhook prevents a sender goroutine or any network use.
func paymentMixedTick(t *testing.T, signal Signal, settings SignalSettings, gate *cadenceAlertGate, notifier *SlackAlertNotifier, clock *slackTestHarness) (Alerts, []slackTransition) {
	t.Helper()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal("synthetic payment observation failed before notifier qualification")
	}
	ready, held := gate.split(signal, alerts)
	notifier.stateLock.Lock()
	transitions := notifier.observeWithLock(clock.Now(), signal.Key(), SignalExecution{
		Alerts: ready, HeldAlerts: held, Failed: err != nil,
	})
	notifier.stateLock.Unlock()
	clock.advance(signal.Cadence())
	return alerts, transitions
}

// Inspect the actual consumer's state using the original emitted identity.
func paymentMixedIncident(notifier *SlackAlertNotifier, signal Signal, seed Alert) (bool, int) {
	notifier.stateLock.Lock()
	defer notifier.stateLock.Unlock()
	incident := notifier.keyIncidents[slackIncidentKey(seed)]
	if incident == nil {
		return false, 0
	}
	return true, incident.signalKeyAbsentRuns[signal.Key()]
}

// The notifier consequence is independent of the class-count assertion: do
// not stop before consuming both mixed turns when the old producer omits one.
func TestPaymentReconciliationMixedNotifierCannotClearValidIncident(t *testing.T) {
	settings := paymentMixedSettings([]Row{{"1", "1", "1", "0", "120"}})
	source := settings.Source.(*syntheticSource)
	original := source.postgresFn
	mixed := false
	source.postgresFn = func(query string) ([]Row, error) {
		if mixed && strings.Contains(query, "monitor-signal-2.21-payment-reconciliation-unfulfillable") {
			return []Row{{"1", "2", "2", "1", "120"}}, nil
		}
		return original(query)
	}
	clock := newSlackTestHarness()
	settings.Now = clock.Now
	signal := NewPaymentReconciliationSignal()
	gate := newCadenceAlertGate()
	notifier := NewSlackAlertNotifier(SlackAlertNotifierSettings{
		Now: clock.Now, ClearAfterCompleteRuns: 2, Logf: func(string, ...any) {},
	})
	if notifier.enabled {
		t.Fatal("in-memory payment notifier unexpectedly enabled transport")
	}
	seedAlerts, _ := paymentMixedTick(t, signal, settings, gate, notifier, clock)
	if len(seedAlerts) != 1 || seedAlerts[0].Class != "payment-reconciliation-credit-unfulfillable" {
		t.Fatal("valid audit seed did not establish the expected original incident")
	}
	seed := seedAlerts[0]
	paymentMixedTick(t, signal, settings, gate, notifier, clock)
	if present, absent := paymentMixedIncident(notifier, signal, seed); !present || absent != 0 {
		t.Fatal("valid audit incident was not mature before the mixed turns")
	}
	mixed = true
	falseClear, absenceAdvanced := false, false
	for turn := 0; turn < 2; turn++ {
		alerts, transitions := paymentMixedTick(t, signal, settings, gate, notifier, clock)
		if !paymentMixedHasClass(alerts, "payment-reconciliation-credit-unfulfillable-invalid") {
			t.Fatal("mixed audit lost its independent invalid-record PAGE")
		}
		for _, transition := range transitions {
			if transition.kind == slackCleared && transition.alert.Identity() == seed.Identity() {
				falseClear = true
			}
		}
		present, absent := paymentMixedIncident(notifier, signal, seed)
		absenceAdvanced = absenceAdvanced || !present || absent != 0
	}
	if falseClear || absenceAdvanced {
		t.Fatal("invalid records granted absence or clear credit while valid terminal evidence remained")
	}
}

// Complete healthy, valid-only and invalid-only source states remain distinct.
func TestPaymentReconciliationMixedIndependentControls(t *testing.T) {
	for _, test := range []struct {
		name  string
		row   Row
		class string
	}{
		{name: "healthy", row: Row{"0", "0", "0", "0", "-1"}},
		{name: "valid-only", row: Row{"1", "1", "1", "0", "120"}, class: "payment-reconciliation-credit-unfulfillable"},
		{name: "invalid-only", row: Row{"0", "1", "1", "1", "120"}, class: "payment-reconciliation-credit-unfulfillable-invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), paymentMixedSettings([]Row{test.row}))
			if err != nil {
				t.Fatal("complete terminal control did not return its public observation")
			}
			if test.class == "" {
				if len(alerts) != 0 {
					t.Fatal("complete empty terminal audit did not remain alert-free")
				}
			} else if len(alerts) != 1 || alerts[0].Class != test.class || alerts[0].Severity != SeverityPage || isVisibilityAlert(alerts[0]) {
				t.Fatal("independent valid or invalid terminal control changed classification")
			}
		})
	}
}

// Malformed counts and private source failures are unknown, not an invented
// financial condition or public raw error/identifier.
func TestPaymentReconciliationMixedUnknownAndPrivacy(t *testing.T) {
	const canary = "synthetic-private-payment-audit-value"
	for _, test := range []struct {
		name       string
		rows       []Row
		requestErr error
	}{
		{name: "shape", rows: []Row{{"1", "2", "2", "1"}}},
		{name: "contradictory-counts", rows: []Row{{"1", "2", "1", "3", "120"}}},
		{name: "private-number", rows: []Row{{"1", canary, "1", "0", "120"}}},
		{name: "private-source-error", requestErr: errors.New(canary)},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := paymentMixedSettings(test.rows)
			source := settings.Source.(*syntheticSource)
			original := source.postgresFn
			source.postgresFn = func(query string) ([]Row, error) {
				if test.requestErr != nil && strings.Contains(query, "monitor-signal-2.21-payment-reconciliation-unfulfillable") {
					return nil, test.requestErr
				}
				return original(query)
			}
			alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !isVisibilityAlert(alerts[0]) {
				t.Fatal("malformed or failed terminal source did not remain explicitly unknown")
			}
			requirePaymentMixedPrivacy(t, alerts, canary)
		})
	}
}

// Cancellation inside the actual third source callback cannot publish the
// complete-looking mixed row or manufacture a financial incident.
func TestPaymentReconciliationMixedCancellationWithholdsObservation(t *testing.T) {
	settings := paymentMixedSettings([]Row{{"1", "2", "2", "1", "120"}})
	source := settings.Source.(*syntheticSource)
	original := source.postgresFn
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source.postgresFn = func(query string) ([]Row, error) {
		rows, err := original(query)
		if strings.Contains(query, "monitor-signal-2.21-payment-reconciliation-unfulfillable") {
			cancel()
		}
		return rows, err
	}
	alerts, err := NewPaymentReconciliationSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("canceled terminal observation published a misleading financial result")
	}
}
