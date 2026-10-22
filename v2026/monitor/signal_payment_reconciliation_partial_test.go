// Independent complete aggregates survive sibling observation failures through
// the public signal, cadence gate, and notification recovery reducer.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The expected store census is explicit; an omitted store stays omitted.
func paymentReconciliationPartialHealthRows(heartbeatAge string) []Row {
	return []Row{
		{"apple", heartbeatAge, "600", "0", "0", "600", "1", "0", "0"},
		{"google", heartbeatAge, "600", "0", "0", "600", "1", "0", "0"},
		{"solana", heartbeatAge, "600", "0", "0", "600", "1", "0", "0"},
		{"stripe", heartbeatAge, "600", "0", "0", "600", "1", "0", "0"},
		{"x402", heartbeatAge, "600", "0", "0", "600", "1", "0", "0"},
		{"monero", heartbeatAge, "600", "0", "0", "600", "1", "0", "0"},
		{"zcash", heartbeatAge, "600", "0", "0", "600", "1", "0", "0"},
	}
}

// Run the real adapter against bounded synthetic results for all three queries.
// Rows accompanying a query error are deliberately available but not authority.
func paymentReconciliationPartialRun(t *testing.T, health, repairs, terminal []Row, queryErrors map[string]error) Alerts {
	t.Helper()
	queries := []string{}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		queries = append(queries, query)
		var rows []Row
		switch query {
		case paymentReconciliationHealthQuery:
			rows = health
		case paymentReconciliationRepairQuery:
			rows = repairs
		case paymentReconciliationUnfulfillableQuery:
			rows = terminal
		default:
			t.Fatal("unexpected source query")
		}
		return rows, queryErrors[query]
	}}
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal("independent source failure discarded complete component results")
	}
	wantQueries := []string{paymentReconciliationHealthQuery, paymentReconciliationRepairQuery, paymentReconciliationUnfulfillableQuery}
	if len(queries) != len(wantQueries) {
		t.Fatal("independent source failure prevented a bounded sibling observation")
	}
	for index, query := range queries {
		if query != wantQueries[index] {
			t.Fatal("independent source order or query scope changed")
		}
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "synthetic-private-source-error", "synthetic-private-store", "synthetic-private-cell")
	}
	return alerts
}

// Component failures retain the stable visibility contract and bounded output.
func requirePaymentReconciliationPartialVisibility(t *testing.T, alerts Alerts, target, errorClass string) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == "cannot-observe" && alert.Target == target {
			if alert.SignalID != "monitor/visibility" || alert.SignalKey != "payment-reconciliation" ||
				alert.SignalNumber != "2.21" || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
				!isVisibilityAlert(alert) || alert.Observed != "error_class="+errorClass {
				t.Fatal("component observation failure lost its fixed public visibility contract")
			}
			return alert
		}
	}
	t.Fatal("component observation failure became healthy or lost its fixed target")
	return Alert{}
}

// A failed or malformed later repair source must not discard a complete health
// page, and must not prevent the independent terminal source from being read.
func TestPaymentReconciliationPartialSourceKeepsHealthPage(t *testing.T) {
	for _, test := range []struct {
		name       string
		repairs    []Row
		err        error
		errorClass string
	}{
		{name: "query failure", repairs: []Row{{"apple", "credited", "1", "60"}}, err: errors.New("synthetic-private-source-error"), errorClass: observationErrorClassUnclassified},
		{name: "source deadline with live parent", err: context.DeadlineExceeded, errorClass: observationErrorClassTimeout},
		{name: "malformed aggregate", repairs: []Row{{"apple", "credited", "1", "60"}, {"synthetic-private-store", "credited", "1", "60"}}, errorClass: observationErrorClassInvalidResponse},
	} {
		alerts := paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("10801"), test.repairs,
			[]Row{{"1", "2", "2", "0", "60"}}, map[string]error{paymentReconciliationRepairQuery: test.err})
		if len(alerts) != 3 {
			t.Fatalf("%s: want health page, repair visibility, and terminal page", test.name)
		}
		health := requireAlertClass(t, alerts, "payment-reconciliation-stale")
		terminal := requireAlertClass(t, alerts, "payment-reconciliation-credit-unfulfillable")
		if health.Severity != SeverityPage || terminal.Severity != SeverityPage || terminal.Target != "stripe" {
			t.Fatalf("%s: complete independent page lost its severity or owner", test.name)
		}
		requirePaymentReconciliationPartialVisibility(t, alerts, "payment-reconciliation/repairs", test.errorClass)
	}
}

// A quiet complete health aggregate cannot make an unavailable repair source
// healthy, whether the source command or its aggregate parser failed.
func TestPaymentReconciliationPartialSourceHealthyHealthRemainsUnknown(t *testing.T) {
	for _, test := range []struct {
		repairs    []Row
		err        error
		errorClass string
	}{
		{err: errors.New("synthetic-private-source-error"), errorClass: observationErrorClassUnclassified},
		{repairs: []Row{{"apple", "credited", "synthetic-private-cell", "60"}}, errorClass: observationErrorClassInvalidResponse},
	} {
		alerts := paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("600"), test.repairs,
			[]Row{{"0", "0", "0", "0", "-1"}}, map[string]error{paymentReconciliationRepairQuery: test.err})
		if len(alerts) != 1 {
			t.Fatal("healthy health plus unavailable repairs must remain one unknown observation")
		}
		requirePaymentReconciliationPartialVisibility(t, alerts, "payment-reconciliation/repairs", test.errorClass)
	}
}

// A valid repair remains independently observable when the later terminal
// query fails or returns a malformed aggregate rather than a proven audit fault.
func TestPaymentReconciliationPartialSourceKeepsRepair(t *testing.T) {
	for _, test := range []struct {
		terminal   []Row
		err        error
		errorClass string
	}{
		{terminal: []Row{{"1", "1", "1", "0", "60"}}, err: errors.New("synthetic-private-source-error"), errorClass: observationErrorClassUnclassified},
		{terminal: []Row{{"0", "0", "0", "0", "0"}}, errorClass: observationErrorClassInvalidResponse},
		{terminal: []Row{{"1", "1", "1", "synthetic-private-cell", "60"}}, errorClass: observationErrorClassInvalidResponse},
	} {
		alerts := paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("600"),
			[]Row{{"stripe", "ended", "1", "60"}}, test.terminal,
			map[string]error{paymentReconciliationUnfulfillableQuery: test.err})
		if len(alerts) != 2 {
			t.Fatal("terminal observation failure discarded a valid repair or fabricated a terminal page")
		}
		repair := requireAlertClass(t, alerts, "payment-reconciliation-repair")
		if repair.Severity != SeverityWarn || repair.Target != "stripe" || repair.Frame != "action=ended" ||
			!strings.Contains(repair.Mechanism, "first implemented application path") {
			t.Fatal("independent Stripe ended repair lost its existing lifecycle semantics")
		}
		requirePaymentReconciliationPartialVisibility(t, alerts, "payment-reconciliation/unfulfillable", test.errorClass)
	}
}

// Repair qualification is in its own query, and terminal audit authority needs
// no heartbeat join. Failure of the first health query cannot hide either.
func TestPaymentReconciliationPartialSourceHealthFailureKeepsAudits(t *testing.T) {
	partialHealth := paymentReconciliationPartialHealthRows("10801")[:3]
	for _, test := range []struct {
		err        error
		errorClass string
	}{
		{err: errors.New("synthetic-private-source-error"), errorClass: observationErrorClassUnclassified},
		{errorClass: observationErrorClassInvalidResponse},
	} {
		alerts := paymentReconciliationPartialRun(t, partialHealth, []Row{{"apple", "credited", "1", "60"}},
			[]Row{{"1", "1", "1", "1", "60"}}, map[string]error{paymentReconciliationHealthQuery: test.err})
		if len(alerts) != 3 {
			t.Fatal("unavailable health must retain its visibility, independent repair, and proven invalid audit")
		}
		repair := requireAlertClass(t, alerts, "payment-reconciliation-repair")
		terminal := requireAlertClass(t, alerts, "payment-reconciliation-credit-unfulfillable-invalid")
		if repair.Severity != SeverityWarn || terminal.Severity != SeverityPage {
			t.Fatal("failed health query changed independent audit severity")
		}
		requirePaymentReconciliationPartialVisibility(t, alerts, "payment-reconciliation/health", test.errorClass)
	}
}

// Even severe-looking retained rows cannot supply authority for an incomplete
// health aggregate. There is no partial-row salvage inside a failed component.
func TestPaymentReconciliationPartialSourceIncompleteHealthCannotPage(t *testing.T) {
	partialHealth := paymentReconciliationPartialHealthRows("10801")[:3]
	partialHealth[0][2] = "-1"
	partialHealth[0][3] = "4"
	partialHealth[0][4] = "4"
	for _, health := range [][]Row{nil, partialHealth} {
		alerts := paymentReconciliationPartialRun(t, health, nil, []Row{{"0", "0", "0", "0", "-1"}}, nil)
		if len(alerts) != 1 {
			t.Fatal("partial health fabricated a health page or healthy execution")
		}
		requirePaymentReconciliationPartialVisibility(t, alerts, "payment-reconciliation/health", observationErrorClassInvalidResponse)
	}
}

// All failed sources remain three bounded unknowns, without losing later
// source coverage or leaking any source-controlled error or row content.
func TestPaymentReconciliationPartialSourceAllUnavailable(t *testing.T) {
	sourceErr := errors.New("synthetic-private-source-error")
	alerts := paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("10801"),
		[]Row{{"apple", "credited", "1", "60"}}, []Row{{"1", "1", "1", "0", "60"}},
		map[string]error{
			paymentReconciliationHealthQuery:        sourceErr,
			paymentReconciliationRepairQuery:        sourceErr,
			paymentReconciliationUnfulfillableQuery: sourceErr,
		})
	if len(alerts) != 3 {
		t.Fatal("all unavailable sources must retain three component observations")
	}
	for _, target := range []string{"payment-reconciliation/health", "payment-reconciliation/repairs", "payment-reconciliation/unfulfillable"} {
		requirePaymentReconciliationPartialVisibility(t, alerts, target, observationErrorClassUnclassified)
	}
}

// Only three complete healthy aggregates establish an alert-free execution.
func TestPaymentReconciliationPartialSourceCompleteHealthyControl(t *testing.T) {
	alerts := paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("600"), nil,
		[]Row{{"0", "0", "0", "0", "-1"}}, nil)
	if len(alerts) != 0 {
		t.Fatal("complete healthy sources did not retain the healthy control")
	}
}

// Parent cancellation remains a lifecycle failure, and stops new observations;
// it is not a failed independent source under a still-live observation context.
func TestPaymentReconciliationPartialSourceStopsAtParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sourceErr := errors.New("synthetic-private-source-error")
	calls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		calls++
		switch query {
		case paymentReconciliationHealthQuery:
			return paymentReconciliationPartialHealthRows("10801"), nil
		case paymentReconciliationRepairQuery:
			cancel()
			return nil, sourceErr
		default:
			t.Fatal("parent cancellation admitted another source observation")
			return nil, nil
		}
	}}
	settings := syntheticSettings(source)
	settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
	alerts, err := NewPaymentReconciliationSignal().Run(ctx, settings)
	if calls != 2 || len(alerts) != 0 || !errors.Is(err, context.Canceled) || !errors.Is(err, sourceErr) {
		t.Fatal("parent cancellation lost its lifecycle verdict or returned source cause")
	}
	if err.Error() != context.Canceled.Error() {
		t.Fatal("parent cancellation exposed the private source error")
	}
}

// Intentional PostgreSQL exclusion retains the existing scope-policy warning
// and never becomes three failed commands or contacts the denied source.
func TestPaymentReconciliationPartialSourcePreservesHostExclusion(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		t.Fatal("excluded PostgreSQL source was contacted")
		return nil, nil
	}}
	settings, err := ExcludeHosts(syntheticSettings(source), "pg-1")
	if err != nil {
		t.Fatal("synthetic host exclusion was not admitted")
	}
	alerts, err := NewPaymentReconciliationSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" || !isVisibilityAlert(alerts[0]) {
		t.Fatal("intentional source exclusion changed its scope-policy visibility contract")
	}
}

// The real public signal, cadence gate and notifier reducer preserve existing
// incidents while visibility is held or raised. No webhook or network is used.
func TestPaymentReconciliationPartialSourceCannotClearNotifier(t *testing.T) {
	signal := NewPaymentReconciliationSignal()
	gate := newCadenceAlertGate()
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(""))
	defer closeSlackNotifier(t, notifier)
	observe := func(alerts Alerts) (Alerts, []slackTransition) {
		ready, held := gate.split(signal, alerts)
		notifier.stateLock.Lock()
		defer notifier.stateLock.Unlock()
		return held, notifier.observeWithLock(harness.Now(), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held})
	}
	terminal := []Row{{"0", "0", "0", "0", "-1"}}
	_, raised := observe(paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("10801"),
		[]Row{{"apple", "credited", "1", "60"}}, terminal, nil))
	if len(raised) != 2 || raised[0].kind != slackRaised || raised[1].kind != slackRaised {
		t.Fatal("complete initial health and repair incidents were not raised")
	}
	for run := 0; run < 2; run++ {
		held, transitions := observe(paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("600"), nil,
			terminal, map[string]error{paymentReconciliationRepairQuery: errors.New("synthetic-private-source-error")}))
		if run == 0 && len(held) != 1 {
			t.Fatal("first visibility observation did not exercise the held-sustain boundary")
		}
		for _, transition := range transitions {
			if transition.kind == slackCleared {
				t.Fatal("partial observation cleared an existing financial-liveness incident")
			}
		}
	}
	for run := 0; run < 2; run++ {
		_, transitions := observe(paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("600"), nil, terminal, nil))
		cleared := 0
		for _, transition := range transitions {
			if transition.kind == slackCleared {
				cleared++
			}
		}
		if run == 0 && cleared != 0 {
			t.Fatal("one complete healthy observation prematurely cleared incidents")
		}
		if run == 1 && cleared != 3 {
			t.Fatal("two complete healthy observations did not recover health, repair, and visibility incidents")
		}
	}
}

// The existing fifth-store correction retains its local grant semantics even
// when the independently read Stripe terminal aggregate is unavailable.
func TestPaymentReconciliationPartialSourceKeepsX402LocalRepair(t *testing.T) {
	alerts := paymentReconciliationPartialRun(t, paymentReconciliationPartialHealthRows("600"),
		[]Row{{"x402", "credited", "1", "60"}}, []Row{{"0", "0", "0", "0", "0"}}, nil)
	if len(alerts) != 2 {
		t.Fatal("terminal observation failure discarded a valid local x402 repair")
	}
	repair := requireAlertClass(t, alerts, "payment-reconciliation-repair")
	if repair.Target != "x402" || repair.Frame != "action=credited" || repair.Severity != SeverityWarn ||
		!strings.Contains(repair.Mechanism, "local post-settlement recovery") ||
		!strings.Contains(repair.Action, "do not replay settlement, retarget a payment or manually grant") {
		t.Fatal("independent x402 repair lost its existing local grant semantics")
	}
	requirePaymentReconciliationPartialVisibility(t, alerts, "payment-reconciliation/unfulfillable", observationErrorClassInvalidResponse)
}
